package bank

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/playwright-community/playwright-go"
	"github.com/tidwall/gjson"
)

// ErrNotAuthenticated возвращается, когда сессия в банке протухла и нужен
// повторный вход руками.
var ErrNotAuthenticated = errors.New("сессия в банке недействительна, нужен вход")

// Client ходит во внутренние ручки личного кабинета.
//
// Запросы уходят через APIRequestContext браузера, а не через net/http:
// так они наследуют куки живого профиля и уходят с того же TLS-стека, что и
// запросы самой страницы.
type Client struct {
	br *Browser
	ep *Endpoints

	// Поллер и хендлеры бота живут в разных горутинах, а вкладка браузера
	// одна. Запросы к банку сериализуем, чтобы не читать токен из страницы,
	// пока по ней идёт навигация.
	mu sync.Mutex
}

func NewClient(br *Browser, ep *Endpoints) *Client {
	return &Client{br: br, ep: ep}
}

// Accounts возвращает счета и карты с остатками.
func (c *Client) Accounts() ([]Account, error) {
	spec := c.ep.Accounts
	if spec == nil {
		return nil, errors.New("ручка accounts не описана в endpoints.json")
	}

	root, err := c.fetch(spec)
	if err != nil {
		return nil, err
	}

	items := selectList(root, spec.List)
	accounts := make([]Account, 0, len(items))
	for _, it := range items {
		accounts = append(accounts, Account{
			ID:       str(it, spec.Fields["id"]),
			Name:     str(it, spec.Fields["name"]),
			Number:   str(it, spec.Fields["number"]),
			Balance:  num(it, spec.Fields["balance"]),
			Currency: str(it, spec.Fields["currency"]),
		})
	}
	return accounts, nil
}

// Cards возвращает карты с остатками. Пусто, если ручка карт не описана.
func (c *Client) Cards() ([]Account, error) {
	spec := c.ep.Cards
	if spec == nil {
		return nil, nil
	}

	root, err := c.fetch(spec)
	if err != nil {
		return nil, err
	}

	items := selectList(root, spec.List)
	cards := make([]Account, 0, len(items))
	for _, it := range items {
		cards = append(cards, Account{
			ID:       str(it, spec.Fields["id"]),
			Name:     str(it, spec.Fields["name"]),
			Number:   str(it, spec.Fields["number"]),
			Balance:  num(it, spec.Fields["balance"]),
			Currency: str(it, spec.Fields["currency"]),
			IsCard:   true,
		})
	}
	return cards, nil
}

// Transactions возвращает последние операции, свежие первыми.
func (c *Client) Transactions(limit int) ([]Transaction, error) {
	return c.transactionsAt(time.Now(), limit)
}

// TransactionsWindow забирает период, сдвинутый в прошлое относительно anchor.
//
// Нужна для догрузки старой истории: банк отдаёт операции только за период,
// а границы периода в конфиге заданы смещениями от «сейчас». Подменяя точку
// отсчёта, тем же запросом получаем предыдущие месяцы.
func (c *Client) TransactionsWindow(anchor time.Time) ([]Transaction, error) {
	return c.transactionsAt(anchor, 0)
}

func (c *Client) transactionsAt(now time.Time, limit int) ([]Transaction, error) {
	spec := c.ep.Transactions
	if spec == nil {
		return nil, errors.New("ручка transactions не описана в endpoints.json")
	}

	root, err := c.fetchAt(spec, now)
	if err != nil {
		return nil, err
	}

	items := selectList(root, spec.List)
	txs := make([]Transaction, 0, len(items))
	for _, it := range items {
		txs = append(txs, mapTransaction(it, spec))
	}

	// Банк не гарантирует порядок — сортируем сами, свежие вперёд.
	sort.SliceStable(txs, func(i, j int) bool { return txs[i].Time.After(txs[j].Time) })

	assignKeys(txs)

	if limit > 0 && len(txs) > limit {
		txs = txs[:limit]
	}
	return txs, nil
}

// assignKeys проставляет операциям устойчивые ключи.
//
// Идентификатор из ответа сохраняется отдельно, справочно: у ВТБ он меняется
// от запроса к запросу и на роль ключа не годится.
func assignKeys(txs []Transaction) {
	seen := make(map[string]int, len(txs))

	for i := range txs {
		fp := txs[i].Fingerprint()
		seen[fp]++

		// Две неотличимые операции в одном ответе разводим порядковым
		// номером: одинаковые платежи в одну секунду редки, но терять их
		// молча не хочется.
		if n := seen[fp]; n > 1 {
			fp = fmt.Sprintf("%s#%d", fp, n)
		}

		txs[i].Ref = txs[i].ID
		txs[i].ID = fp
	}
}

// TokenReady сообщает, появился ли токен в хранилище страницы.
//
// Проверка бесплатная — она не ходит в банк, а только читает страницу.
// Нужна потому, что токен живёт в sessionStorage и исчезает вместе с
// браузером: после запуска бота его там нет, пока SPA не загрузится и не
// восстановит сессию по кукам. Опрашивать этот момент запросами к банку
// нельзя — получилась бы очередь обращений с неавторизованным клиентом.
func (c *Client) TokenReady() bool {
	ready, _ := c.TokenState()
	return ready
}

// TokenState отделяет «человек ещё не вошёл» от «читать страницу больше
// нечем»: первое — обычное состояние ожидания, второе означает, что окно
// браузера закрыли и продолжать бессмысленно.
func (c *Client) TokenState() (bool, error) {
	spec := c.ep.Accounts
	if spec == nil {
		spec = c.ep.Transactions
	}
	if spec == nil {
		return false, errors.New("не описана ни одна ручка")
	}
	if spec.Token == nil {
		return true, nil // ручке токен не нужен
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	switch _, err := c.readToken(spec.Token); {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNotAuthenticated):
		return false, nil
	default:
		return false, err
	}
}

// Alive проверяет, жива ли сессия.
func (c *Client) Alive() bool { return c.Check() == nil }

// Check делает контрольный запрос и возвращает причину отказа.
//
// Отдельно от Alive потому, что «не получилось» без объяснения бесполезно:
// когда токен на странице есть, а банк всё равно отвечает отказом, разница
// между истёкшим токеном, кривым адресом ручки и мёртвым зеркалом видна
// только по тексту ошибки.
func (c *Client) Check() error {
	spec := c.ep.Accounts
	if spec == nil {
		spec = c.ep.Transactions
	}
	if spec == nil {
		return errors.New("не описана ни одна ручка")
	}
	_, err := c.fetch(spec)
	return err
}

func (c *Client) fetch(spec *EndpointSpec) (gjson.Result, error) {
	return c.fetchAt(spec, time.Now())
}

func (c *Client) fetchAt(spec *EndpointSpec, now time.Time) (gjson.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var empty gjson.Result

	headers, err := c.buildHeaders(spec)
	if err != nil {
		return empty, err
	}

	// В endpoints.json адреса записаны с тем доменом, на котором их поймал
	// recorder. Домены-зеркала меняются, поэтому хост берём актуальный, а из
	// файла используем только путь.
	target, err := c.resolveURL(spec, now)
	if err != nil {
		return empty, err
	}

	req := c.br.Context().Request()
	var resp playwright.APIResponse

	switch strings.ToUpper(spec.Method) {
	case "GET":
		resp, err = req.Get(target, playwright.APIRequestContextGetOptions{
			Headers: headers,
			Timeout: playwright.Float(30000),
		})
	case "POST":
		opts := playwright.APIRequestContextPostOptions{
			Headers: headers,
			Timeout: playwright.Float(30000),
		}
		if len(spec.Body) > 0 {
			var body any
			if err := json.Unmarshal(spec.Body, &body); err != nil {
				return empty, fmt.Errorf("body в endpoints.json — не валидный JSON: %w", err)
			}
			opts.Data = body
		}
		resp, err = req.Post(target, opts)
	default:
		return empty, fmt.Errorf("метод %q не поддерживается", spec.Method)
	}
	if err != nil {
		return empty, fmt.Errorf("запрос к %s: %w", target, err)
	}
	defer resp.Dispose()

	if resp.Status() == 401 || resp.Status() == 403 {
		return empty, fmt.Errorf("%w: %s ответил %d", ErrNotAuthenticated, endpointPath(target), resp.Status())
	}
	if !resp.Ok() {
		body, _ := resp.Text()
		return empty, fmt.Errorf("%s ответил %d %s: %s",
			endpointPath(target), resp.Status(), resp.StatusText(), snippet(body))
	}

	text, err := resp.Text()
	if err != nil {
		return empty, fmt.Errorf("прочитать ответ %s: %w", target, err)
	}
	if !gjson.Valid(text) {
		// Самый частый случай: вместо JSON прилетела HTML-страница логина.
		return empty, fmt.Errorf("%w: %s вернул не JSON: %s",
			ErrNotAuthenticated, endpointPath(target), snippet(text))
	}

	return gjson.Parse(text), nil
}

// endpointPath оставляет от адреса только путь: query кабинета содержит
// идентификаторы счетов, а строка уходит в лог.
func endpointPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Path
}

// snippet — короткая выдержка из ответа для сообщения об ошибке.
func snippet(body string) string {
	s := strings.Join(strings.Fields(body), " ")
	if s == "" {
		return "(пустой ответ)"
	}
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

// resolveURL готовит адрес запроса: подставляет активное зеркало и пересчитывает
// параметры, зависящие от текущего момента.
func (c *Client) resolveURL(spec *EndpointSpec, now time.Time) (string, error) {
	host := c.br.Host()
	if host == "" {
		return "", errors.New("зеркало ещё не выбрано")
	}

	u, err := url.Parse(spec.URL)
	if err != nil {
		return "", fmt.Errorf("некорректный url в endpoints.json (%s): %w", spec.URL, err)
	}
	u.Host = host

	if len(spec.Params) > 0 {
		q := u.Query()
		for name, p := range spec.Params {
			v, err := p.Render(now)
			if err != nil {
				return "", fmt.Errorf("параметр %s: %w", name, err)
			}
			q.Set(name, v)
		}
		u.RawQuery = q.Encode()
	}

	return u.String(), nil
}

// buildHeaders собирает заголовки запроса и подставляет {{token}}, если
// ручка требует токен из хранилища страницы.
func (c *Client) buildHeaders(spec *EndpointSpec) (map[string]string, error) {
	headers := map[string]string{"Accept": "application/json"}
	maps.Copy(headers, spec.Headers)

	if spec.Token == nil {
		return headers, nil
	}

	token, err := c.readToken(spec.Token)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		if strings.Contains(v, "{{token}}") {
			headers[k] = strings.ReplaceAll(v, "{{token}}", token)
		}
	}
	return headers, nil
}

func (c *Client) readToken(ts *TokenSource) (string, error) {
	var (
		raw string
		err error
	)

	switch strings.ToLower(ts.Storage) {
	case "local", "localstorage":
		raw, err = c.br.readStorage("localStorage", ts.Key)
		if err != nil {
			return "", err
		}

	case "session", "sessionstorage":
		raw, err = c.br.readStorage("sessionStorage", ts.Key)
		if err != nil {
			return "", err
		}

	case "cookie":
		cookies, err := c.br.Context().Cookies()
		if err != nil {
			return "", fmt.Errorf("прочитать куки: %w", err)
		}
		for _, ck := range cookies {
			if ck.Name == ts.Key {
				raw = ck.Value
				break
			}
		}

	default:
		return "", fmt.Errorf("неизвестное хранилище токена %q (ожидается local, session или cookie)", ts.Storage)
	}

	if raw == "" {
		return "", ErrNotAuthenticated
	}
	if ts.Path != "" {
		return gjson.Get(raw, ts.Path).String(), nil
	}
	return raw, nil
}

func mapTransaction(it gjson.Result, spec *EndpointSpec) Transaction {
	amount := num(it, spec.Fields["amount"])

	tx := Transaction{
		ID:        str(it, spec.Fields["id"]),
		Time:      parseTime(str(it, spec.Fields["time"]), spec.TimeLayout),
		Amount:    math.Abs(amount),
		Currency:  str(it, spec.Fields["currency"]),
		Title:     str(it, spec.Fields["title"]),
		Card:      str(it, spec.Fields["card"]),
		Direction: DirectionUnknown,
		Raw:       it.Raw,
	}

	if path := spec.Fields["direction"]; path != "" {
		val := strings.ToLower(strings.TrimSpace(str(it, path)))
		tx.Direction = DirectionOut
		for _, in := range spec.InValues {
			if strings.ToLower(strings.TrimSpace(in)) == val {
				tx.Direction = DirectionIn
				break
			}
		}
	} else if amount > 0 {
		tx.Direction = DirectionIn
	} else if amount < 0 {
		tx.Direction = DirectionOut
	}

	return tx
}

// selectList достаёт массив элементов по gjson-пути. Пустой путь означает,
// что массив лежит в корне ответа.
func selectList(root gjson.Result, path string) []gjson.Result {
	node := root
	if path != "" {
		node = root.Get(path)
	}
	if !node.Exists() {
		return nil
	}
	if node.IsArray() {
		return node.Array()
	}
	return []gjson.Result{node}
}

func str(it gjson.Result, path string) string {
	if path == "" {
		return ""
	}
	return strings.TrimSpace(it.Get(path).String())
}

func num(it gjson.Result, path string) float64 {
	if path == "" {
		return 0
	}
	r := it.Get(path)
	if r.Type == gjson.String {
		// Банки любят отдавать суммы строкой: "1 234,56"
		s := strings.NewReplacer(" ", "", " ", "", ",", ".").Replace(r.String())
		return gjson.Parse(s).Float()
	}
	return r.Float()
}

// commonLayouts — форматы, которые встречаются в ответах чаще всего.
var commonLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
	"02.01.2006 15:04:05",
	"02.01.2006 15:04",
	"02.01.2006",
}

func parseTime(v, layout string) time.Time {
	if v == "" {
		return time.Time{}
	}
	if layout != "" {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	for _, l := range commonLayouts {
		if t, err := time.Parse(l, v); err == nil {
			return t
		}
	}
	// Иногда время приходит миллисекундами эпохи.
	if ms := gjson.Parse(v).Int(); ms > 1_000_000_000 {
		if ms > 1_000_000_000_000 {
			return time.UnixMilli(ms)
		}
		return time.Unix(ms, 0)
	}
	return time.Time{}
}
