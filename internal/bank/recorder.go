package bank

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/playwright-community/playwright-go"
	"github.com/tidwall/gjson"
)

// Recorder слушает сетевые запросы личного кабинета и складывает JSON-ответы
// на диск. Нужен один раз — чтобы понять, какая ручка отдаёт остатки, а какая
// список операций. Адреса и структура ответов нигде не описаны, так что
// единственный честный способ их узнать — посмотреть, что делает сам сайт.
type Recorder struct {
	dir  string
	host string
	br   *Browser

	// OnRecord вызывается после того, как поймана новая ручка. Нужен, чтобы
	// сообщать в консоль, что искомое уже нашлось и можно заканчивать.
	OnRecord func()

	mu           sync.Mutex
	seen         map[string]int
	index        []recordEntry
	authScheme   string
	tokenHint    *TokenSource
	tokenChecked bool
}

type recordEntry struct {
	File        string `json:"file"`
	Method      string `json:"method"`
	URL         string `json:"url"`
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	Bytes       int    `json:"bytes"`
	Hits        int    `json:"hits"`
	Preview     string `json:"preview"`

	key string // ключ дедупликации, наружу не пишется
}

func NewRecorder(dir, host string) (*Recorder, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("создать каталог %s: %w", dir, err)
	}
	return &Recorder{dir: dir, host: host, seen: map[string]int{}}, nil
}

// Attach подписывается на ответы браузера.
func (r *Recorder) Attach(br *Browser) {
	r.br = br
	br.Context().OnResponse(func(resp playwright.Response) {
		if err := r.handle(resp); err != nil {
			log.Printf("recorder: %v", err)
		}
	})
}

func (r *Recorder) handle(resp playwright.Response) error {
	req := resp.Request()

	switch req.ResourceType() {
	case "xhr", "fetch":
	default:
		return nil
	}

	url := resp.URL()
	if !strings.Contains(url, r.host) {
		return nil
	}

	ctype := resp.Headers()["content-type"]
	if !strings.Contains(strings.ToLower(ctype), "json") {
		return nil
	}

	postData, _ := req.PostData()

	// Ключ включает query и тело запроса. Без этого разные вызовы одной ручки
	// схлопываются в один: фронт банка дёргает, например, portfolios/active
	// несколько раз с разными requestProductTypes, и по одному пустому ответу
	// оттуда потом не понять, где на самом деле лежат счета.
	key := dedupKey(req.Method(), url, postData)

	r.mu.Lock()
	r.seen[key]++
	hits := r.seen[key]
	r.mu.Unlock()

	// Одинаковые запросы повторяются десятками — храним первый ответ,
	// дальше только считаем попадания.
	if hits > 1 {
		r.mu.Lock()
		for i := range r.index {
			if r.index[i].key == key {
				r.index[i].Hits = hits
				break
			}
		}
		r.mu.Unlock()
		return nil
	}

	body, err := resp.Body()
	if err != nil {
		// Ответ мог уже уехать из памяти браузера — это не повод падать.
		return nil
	}

	// Разбираемся с авторизацией: сам токен на диск не попадёт, только схема
	// и указание, из какого ключа хранилища его брать.
	scheme, hint := r.inspectAuth(req.Headers())

	dump := map[string]any{
		"url":              url,
		"method":           req.Method(),
		"status":           resp.Status(),
		"request_headers":  safeHeaders(req.Headers()),
		"request_body":     postData,
		"response_headers": resp.Headers(),
		"response_body":    json.RawMessage(body),
		"auth_scheme":      scheme,
		"token_hint":       hint,
		"recorded_at":      time.Now().Format(time.RFC3339),
	}

	name := fileName(req.Method(), url, postData)
	path := filepath.Join(r.dir, name)

	pretty, err := json.MarshalIndent(dump, "", "  ")
	if err != nil {
		// response_body мог оказаться не-JSON, несмотря на content-type.
		dump["response_body"] = string(body)
		if pretty, err = json.MarshalIndent(dump, "", "  "); err != nil {
			return fmt.Errorf("сериализовать дамп %s: %w", url, err)
		}
	}
	if err := os.WriteFile(path, pretty, 0o600); err != nil {
		return fmt.Errorf("записать %s: %w", path, err)
	}

	r.mu.Lock()
	r.index = append(r.index, recordEntry{
		File:        name,
		Method:      req.Method(),
		URL:         url,
		Status:      resp.Status(),
		ContentType: ctype,
		Bytes:       len(body),
		Hits:        hits,
		Preview:     preview(body, 220),
		key:         key,
	})
	r.mu.Unlock()

	log.Printf("записал %s %s (%d, %d Б)", req.Method(), stripQuery(url), resp.Status(), len(body))

	if r.OnRecord != nil {
		r.OnRecord()
	}
	return nil
}

// inspectAuth разбирает заголовок Authorization и один раз выясняет, откуда
// фронт берёт токен.
//
// Значение токена никуда не сохраняется — ни в дамп, ни в лог. На диск идёт
// только схема («Bearer») и адрес ключа в хранилище, по которому клиент потом
// возьмёт свежий токен с живой страницы.
func (r *Recorder) inspectAuth(headers map[string]string) (string, *TokenSource) {
	auth := ""
	for k, v := range headers {
		if strings.EqualFold(k, "authorization") {
			auth = v
			break
		}
	}
	if auth == "" {
		return "", nil
	}

	scheme, token, ok := strings.Cut(auth, " ")
	if !ok {
		scheme, token = "Bearer", auth
	}
	token = strings.TrimSpace(token)

	r.mu.Lock()
	if r.tokenChecked {
		hint := r.tokenHint
		r.mu.Unlock()
		return scheme, hint
	}
	r.tokenChecked = true
	r.mu.Unlock()

	hint := r.findTokenSource(token)
	if hint != nil {
		log.Printf("токен берётся из %s storage, ключ %q", hint.Storage, hint.Key)
	} else {
		log.Print("токен есть, но в хранилищах страницы не найден — источник придётся указать руками")
	}

	r.mu.Lock()
	r.authScheme = scheme
	r.tokenHint = hint
	r.mu.Unlock()

	return scheme, hint
}

// storageDump — то, что возвращает скрипт со страницы.
const storageDump = `() => {
	const out = [];
	for (const [name, store] of [["local", localStorage], ["session", sessionStorage]]) {
		try {
			for (let i = 0; i < store.length; i++) {
				const k = store.key(i);
				out.push({storage: name, key: k, value: store.getItem(k)});
			}
		} catch (e) { /* хранилище может быть недоступно */ }
	}
	return JSON.stringify(out);
}`

// findTokenSource ищет, в каком ключе хранилища лежит этот токен.
func (r *Recorder) findTokenSource(token string) *TokenSource {
	if r.br == nil || token == "" {
		return nil
	}

	raw, err := r.br.Page().Evaluate(storageDump)
	if err != nil {
		return nil
	}
	dump, _ := raw.(string)

	var found *TokenSource
	gjson.Parse(dump).ForEach(func(_, entry gjson.Result) bool {
		storage := entry.Get("storage").String()
		key := entry.Get("key").String()
		value := entry.Get("value").String()

		// Токен лежит в ключе целиком.
		if value == token {
			found = &TokenSource{Storage: storage, Key: key}
			return false
		}
		// Или внутри JSON-объекта под этим ключом.
		if strings.Contains(value, token) && gjson.Valid(value) {
			if path := findJSONPath(gjson.Parse(value), token, ""); path != "" {
				found = &TokenSource{Storage: storage, Key: key, Path: path}
				return false
			}
		}
		return true
	})

	if found != nil {
		return found
	}

	// Токен может жить и в куке.
	cookies, err := r.br.Context().Cookies()
	if err != nil {
		return nil
	}
	for _, ck := range cookies {
		if ck.Value == token {
			return &TokenSource{Storage: "cookie", Key: ck.Name}
		}
	}
	return nil
}

// findJSONPath ищет строковое значение внутри JSON и возвращает gjson-путь
// до него.
func findJSONPath(node gjson.Result, target, prefix string) string {
	var result string

	node.ForEach(func(k, v gjson.Result) bool {
		path := k.String()
		if prefix != "" {
			path = prefix + "." + path
		}

		switch {
		case v.Type == gjson.String && v.String() == target:
			result = path
			return false
		case v.IsObject():
			if found := findJSONPath(v, target, path); found != "" {
				result = found
				return false
			}
		}
		return true
	})

	return result
}

// Flush пишет сводный индекс. С него удобно начинать разбор.
func (r *Recorder) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	path := filepath.Join(r.dir, "index.json")
	data, err := json.MarshalIndent(r.index, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	log.Printf("итого ручек: %d, индекс: %s", len(r.index), path)
	return nil
}

// Count — сколько разных ручек поймали.
func (r *Recorder) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.index)
}

// safeHeaders выбрасывает заголовки авторизации: дамп лежит на диске
// и незачем хранить в нём живой токен.
// redacted — метка вырезанного значения. Разбор дампов ориентируется на неё,
// чтобы не принять её за настоящий заголовок.
const redacted = "<вырезано>"

func safeHeaders(h map[string]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		switch strings.ToLower(k) {
		case "authorization", "cookie", "x-csrf-token", "x-auth-token":
			out[k] = redacted
		default:
			out[k] = v
		}
	}
	return out
}

func stripQuery(url string) string {
	base, _, _ := strings.Cut(url, "?")
	return base
}

var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// dedupKey различает запросы к одной ручке с разными параметрами.
func dedupKey(method, url, body string) string {
	key := method + " " + url
	if body != "" {
		sum := sha1.Sum([]byte(body))
		key += " " + hex.EncodeToString(sum[:6])
	}
	return key
}

func fileName(method, url, body string) string {
	clean := stripQuery(url)
	clean = strings.TrimPrefix(clean, "https://")
	clean = unsafeChars.ReplaceAllString(clean, "_")

	// Имя строится по пути без query, поэтому запросы, различающиеся только
	// параметрами, дали бы одинаковые имена и затирали друг друга. Хвост из
	// хеша полного ключа это разводит.
	var suffix string
	if strings.Contains(url, "?") || body != "" {
		sum := sha1.Sum([]byte(dedupKey(method, url, body)))
		suffix = "_" + hex.EncodeToString(sum[:4])
	}

	// Длинный путь может не влезть в имя файла.
	if len(clean) > 90 {
		sum := sha1.Sum([]byte(url))
		clean = clean[:90]
		if suffix == "" {
			suffix = "_" + hex.EncodeToString(sum[:4])
		}
	}

	return strings.ToLower(method) + "_" + clean + suffix + ".json"
}

func preview(body []byte, n int) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
