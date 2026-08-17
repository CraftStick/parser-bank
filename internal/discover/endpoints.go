package discover

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/CraftStick/parser-bank/internal/bank"
	"github.com/tidwall/gjson"
)

// Endpoints собирает конфиг из найденных кандидатов.
func (r *Result) Endpoints() *bank.Endpoints {
	e := &bank.Endpoints{}
	if r.Accounts != nil {
		e.Accounts = r.Accounts.spec(r.Accounts.AcctFields, nil)
	}
	if r.Cards != nil {
		e.Cards = r.Cards.spec(r.Cards.AcctFields, nil)
	}
	if r.Transactions != nil {
		e.Transactions = r.Transactions.spec(r.Transactions.TxFields, r.Transactions.InValues)
	}
	return e
}

func (c *Candidate) spec(fields map[string]string, inValues []string) *bank.EndpointSpec {
	spec := &bank.EndpointSpec{
		URL:      c.URL,
		Method:   strings.ToUpper(c.Method),
		List:     c.ListPath,
		Fields:   fields,
		InValues: inValues,
		Token:    c.src.TokenHint,
	}

	headers := carriedHeaders(c.src.RequestHeaders)

	// Значение токена в дампы не пишется — только схема и то, откуда его
	// брать. Подставится оно уже на живой странице.
	if c.src.AuthScheme != "" {
		headers["Authorization"] = c.src.AuthScheme + " {{token}}"
	}
	if len(headers) > 0 {
		spec.Headers = headers
	}

	if body := strings.TrimSpace(c.src.RequestBody); body != "" && gjson.Valid(body) {
		spec.Body = json.RawMessage(body)
	}

	spec.Params = timeParams(c.URL, c.src.RecordedAt)

	return spec
}

// queryLayouts — форматы дат, которые встречаются в параметрах запроса.
// Порядок важен: сначала самые подробные, иначе часть значения потеряется.
var queryLayouts = []string{
	"2006-01-02T15:04:05.000-07:00",
	"2006-01-02T15:04:05.000Z07:00",
	"2006-01-02T15:04:05-07:00",
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02",
	"02.01.2006",
}

// timeParams находит в адресе параметры-даты и описывает их как смещения от
// текущего момента.
//
// История операций запрашивается за период, и просто сохранить пойманный
// диапазон нельзя: пролистывая историю, фронт запрашивает всё более старые
// страницы, и записаться может любая из них. Конфиг с окном «май–июнь» выглядит
// правдоподобно, но бот по нему не увидит ни одного нового поступления.
//
// Поэтому окно сдвигается: самая поздняя из найденных дат становится «сейчас»,
// остальные считаются относительно неё. Длительность периода при этом
// сохраняется, а какая именно страница истории попалась — перестаёт иметь
// значение.
func timeParams(rawURL, recordedAt string) map[string]bank.TimeParam {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}

	type dateParam struct {
		name   string
		layout string
		ts     time.Time
	}

	var found []dateParam
	for name, values := range u.Query() {
		if len(values) == 0 {
			continue
		}
		if layout, ts, ok := parseQueryDate(values[0]); ok {
			found = append(found, dateParam{name: name, layout: layout, ts: ts})
		}
	}
	if len(found) == 0 {
		return nil
	}

	// Одиночную дату сдвигать не от чего — считаем от момента записи.
	base, err := time.Parse(time.RFC3339, recordedAt)
	if err != nil {
		base = time.Now()
	}
	if len(found) > 1 {
		base = found[0].ts
		for _, p := range found[1:] {
			if p.ts.After(base) {
				base = p.ts
			}
		}
	}

	out := make(map[string]bank.TimeParam, len(found))
	for _, p := range found {
		out[p.name] = bank.TimeParam{Offset: offsetDays(base, p.ts), Layout: p.layout}
	}
	return out
}

func parseQueryDate(v string) (layout string, ts time.Time, ok bool) {
	v = strings.TrimSpace(v)
	if len(v) < 8 {
		return "", time.Time{}, false
	}
	for _, l := range queryLayouts {
		if t, err := time.Parse(l, v); err == nil {
			return l, t, true
		}
	}
	return "", time.Time{}, false
}

// offsetDays округляет разницу до суток.
//
// Округление всегда в сторону расширения окна: лишний день истории безвреден,
// а недостающий означает пропущенное поступление.
func offsetDays(base, ts time.Time) string {
	// Верхняя граница периода — это «сейчас», а не будущее.
	days := min(int(math.Floor(ts.Sub(base).Hours()/24)), 0)
	return fmt.Sprintf("%dd", days)
}

// redacted повторяет метку, которой recorder заменяет секреты в дампах.
const redacted = "<вырезано>"

// carriedHeaders отбирает заголовки, которые нужно повторять при каждом
// запросе.
//
// Одного токена шлюзу ВТБ мало: он требует собственные x-заголовки
// (x-channel, x-platform, x-unc, x-vtb-mf и другие) и без них отвечает
// отказом, неотличимым от протухшей сессии. Поэтому переносим их как есть.
//
// Всё, что подставляет сам браузер, наоборот не трогаем: подменять
// user-agent или sec-ch-* значениями из дампа бессмысленно, а расходиться
// с реальным браузером — заметно.
func carriedHeaders(src map[string]string) map[string]string {
	out := map[string]string{}

	for k, v := range src {
		lower := strings.ToLower(k)

		switch {
		case v == "" || v == redacted:
			continue
		// Заголовки соединения и опознания браузера — за браузером.
		case lower == "authorization" || lower == "cookie" ||
			lower == "host" || lower == "content-length" ||
			lower == "user-agent" || lower == "origin" ||
			strings.HasPrefix(lower, "sec-"):
			continue
		// Идентификатор конкретного запроса: повторять его на каждом опросе
		// нельзя — для шлюза это выглядит как дубль одного и того же вызова.
		case strings.HasSuffix(lower, "request-id") || lower == "x-correlation-id":
			continue
		case strings.HasPrefix(lower, "x-") ||
			lower == "accept" || lower == "accept-language" ||
			lower == "referer" || lower == "content-type":
			out[k] = v
		}
	}
	return out
}

// WriteEndpoints сохраняет конфиг. Существующий файл не трогает: правки,
// сделанные руками, дороже свежей догадки эвристики.
func WriteEndpoints(e *bank.Endpoints, path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("файл %s уже существует — удалите или переименуйте его, если хотите пересоздать", path)
	}

	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return fmt.Errorf("сериализовать конфиг: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("записать %s: %w", path, err)
	}
	return nil
}

// Save записывает найденный конфиг по пути path.
//
// Если файл уже есть, он не трогается: правки, сделанные руками, дороже свежей
// догадки эвристики. Предложение в этом случае ложится рядом, отдельным файлом.
func Save(res *Result, path string) error {
	endpoints := res.Endpoints()

	err := WriteEndpoints(endpoints, path)
	if err == nil {
		log.Printf("записал %s — проверьте глазами и запускайте бота", path)
		return nil
	}
	if _, statErr := os.Stat(path); statErr != nil {
		return err
	}

	alt := filepath.Join(filepath.Dir(path), "endpoints.suggested.json")
	if err := os.Remove(alt); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("удалить прошлое предложение %s: %w", alt, err)
	}
	if err := WriteEndpoints(endpoints, alt); err != nil {
		return err
	}

	log.Printf("%s уже существует, поэтому предложение записано в %s", path, alt)
	log.Print("сравните файлы и перенесите нужное руками")
	return nil
}

// Report — человекочитаемая сводка: что нашли и почему так решили.
func (r *Result) Report() string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "Разобрано дампов: %d, кандидатов: %d\n\n", r.Files, len(r.All))

	describe := func(role string, c *Candidate, fields map[string]string, score int) {
		if c == nil {
			fmt.Fprintf(&sb, "%s: не найдено\n\n", role)
			return
		}

		fmt.Fprintf(&sb, "%s (уверенность %d)\n", role, score)
		fmt.Fprintf(&sb, "  %s %s\n", c.Method, c.URL)
		fmt.Fprintf(&sb, "  массив: %s (%d элементов)\n", orRoot(c.ListPath), c.Count)
		fmt.Fprintf(&sb, "  файл:   %s\n", c.Source)

		for _, key := range []string{"id", "time", "amount", "balance", "currency", "direction", "title", "name", "number", "card"} {
			if path, ok := fields[key]; ok {
				fmt.Fprintf(&sb, "  %-9s → %s\n", key, path)
			}
		}
		// in_values имеют смысл только для операций.
		if role == "ОПЕРАЦИИ" && len(c.InValues) > 0 {
			fmt.Fprintf(&sb, "  зачисления: %s\n", strings.Join(c.InValues, ", "))
		}
		for name, p := range timeParams(c.URL, c.src.RecordedAt) {
			fmt.Fprintf(&sb, "  параметр %s: %s от текущего момента\n", name, p.Offset)
		}
		if c.src.TokenHint != nil {
			fmt.Fprintf(&sb, "  токен: %s storage, ключ %s\n", c.src.TokenHint.Storage, c.src.TokenHint.Key)
		} else if c.src.AuthScheme != "" {
			fmt.Fprintf(&sb, "  токен: схема %s, но источник не найден — впишите его руками\n", c.src.AuthScheme)
		}
		sb.WriteString("\n")
	}

	describe("СЧЕТА", r.Accounts, accountFields(r.Accounts), score(r.Accounts, true))
	describe("КАРТЫ", r.Cards, accountFields(r.Cards), score(r.Cards, true))
	describe("ОПЕРАЦИИ", r.Transactions, txFields(r.Transactions), score(r.Transactions, false))

	return sb.String()
}

func accountFields(c *Candidate) map[string]string {
	if c == nil {
		return nil
	}
	return c.AcctFields
}

func txFields(c *Candidate) map[string]string {
	if c == nil {
		return nil
	}
	return c.TxFields
}

func score(c *Candidate, accounts bool) int {
	if c == nil {
		return 0
	}
	if accounts {
		return c.AcctScore
	}
	return c.TxScore
}

func orRoot(path string) string {
	if path == "" {
		return "корень ответа"
	}
	return path
}
