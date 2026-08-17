package bank

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Endpoints описывает, куда ходить за данными и как разбирать ответ.
//
// Схема вынесена в JSON намеренно: адреса внутренних ручек банка и структура
// их ответов нигде не документированы и меняются без предупреждения. Их
// находит recorder (cmd/recorder), а поправить потом можно без пересборки.
type Endpoints struct {
	Accounts     *EndpointSpec `json:"accounts"`
	Cards        *EndpointSpec `json:"cards,omitempty"`
	Transactions *EndpointSpec `json:"transactions"`
}

// EndpointSpec — одна ручка API.
type EndpointSpec struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`

	// Token описывает, откуда взять токен, если ручка требует Authorization.
	// Подставляется в заголовки на место {{token}}.
	Token *TokenSource `json:"token,omitempty"`

	// Params перезаписывает параметры запроса на каждом вызове.
	//
	// Без этого диапазон дат остался бы таким, каким был в момент записи:
	// история операций запрашивается за период, и зафиксированный «по вчера»
	// dateTo означает, что новых поступлений бот не увидит никогда.
	Params map[string]TimeParam `json:"params,omitempty"`

	// List — gjson-путь до массива элементов в ответе, например "data.items".
	// Пустая строка означает, что массив лежит в корне ответа.
	List string `json:"list"`

	// Fields — логическое имя поля -> gjson-путь внутри элемента массива.
	// Для счетов: id, name, number, balance, currency.
	// Для операций: id, time, amount, currency, direction, title, card.
	Fields map[string]string `json:"fields"`

	// TimeLayout — формат времени в стиле Go (см. README). Если пусто,
	// разбор идёт по списку распространённых форматов.
	TimeLayout string `json:"time_layout,omitempty"`

	// InValues — значения поля direction, которые означают зачисление.
	// Если поле direction не задано, направление берётся по знаку суммы.
	InValues []string `json:"in_values,omitempty"`
}

// TimeParam — параметр запроса, вычисляемый от текущего момента.
//
// Offset задаётся как "-30d", "-12h", "0d"; Layout — формат даты в стиле Go,
// снятый с того значения, которое отправлял сам сайт.
type TimeParam struct {
	Offset string `json:"offset"`
	Layout string `json:"layout"`
}

// Render возвращает значение параметра на момент now.
func (p TimeParam) Render(now time.Time) (string, error) {
	d, err := parseOffset(p.Offset)
	if err != nil {
		return "", err
	}
	layout := p.Layout
	if layout == "" {
		layout = time.RFC3339
	}
	return now.Add(d).Format(layout), nil
}

var reOffset = regexp.MustCompile(`^([+-]?\d+)([dhm])$`)

func parseOffset(s string) (time.Duration, error) {
	g := reOffset.FindStringSubmatch(strings.TrimSpace(s))
	if g == nil {
		return 0, fmt.Errorf("offset %q: ожидается вид -30d, -12h или 0m", s)
	}

	n, err := strconv.Atoi(g[1])
	if err != nil {
		return 0, fmt.Errorf("offset %q: %w", s, err)
	}

	switch g[2] {
	case "d":
		return time.Duration(n) * 24 * time.Hour, nil
	case "h":
		return time.Duration(n) * time.Hour, nil
	default:
		return time.Duration(n) * time.Minute, nil
	}
}

// TokenSource — откуда достать токен доступа из живой страницы.
type TokenSource struct {
	// Storage: "local" (localStorage), "session" (sessionStorage) или "cookie".
	Storage string `json:"storage"`
	Key     string `json:"key"`
	// Path — gjson-путь внутри значения, если под ключом лежит JSON.
	Path string `json:"path,omitempty"`
}

// LoadEndpoints читает endpoints.json.
func LoadEndpoints(path string) (*Endpoints, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf(
				"нет файла %s — сначала запустите discovery: make record (подробности в README)", path)
		}
		return nil, fmt.Errorf("прочитать %s: %w", path, err)
	}

	var e Endpoints
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("разобрать %s: %w", path, err)
	}

	if e.Accounts == nil && e.Transactions == nil {
		return nil, fmt.Errorf("в %s не описана ни одна ручка", path)
	}
	for name, spec := range map[string]*EndpointSpec{"accounts": e.Accounts, "transactions": e.Transactions} {
		if spec == nil {
			continue
		}
		if spec.URL == "" {
			return nil, fmt.Errorf("%s: пустой url", name)
		}
		if spec.Method == "" {
			spec.Method = "GET"
		}
		if len(spec.Fields) == 0 {
			return nil, fmt.Errorf("%s: не заполнен fields — непонятно, как разбирать ответ", name)
		}
	}

	return &e, nil
}
