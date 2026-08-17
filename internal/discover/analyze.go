// Package discover разбирает дампы, снятые recorder-ом, и сам составляет
// endpoints.json.
//
// Задача такая: среди десятков ответов личного кабинета найти два — со счетами
// и с операциями — и понять, как в них называются поля. Делается это по
// признакам самих данных, а не по именам ручек: имена у банка непредсказуемые,
// а вот массив объектов, где есть дата, сумма и код валюты, ни с чем не
// спутаешь.
package discover

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/valerakrut/parserbank/internal/bank"
)

// minScore — порог, ниже которого кандидат не считается находкой. Подобран
// так, чтобы случайный массив служебных объектов не прошёл.
const minScore = 7

// dump — то, что пишет recorder.
type dump struct {
	URL            string            `json:"url"`
	Method         string            `json:"method"`
	Status         int               `json:"status"`
	RequestHeaders map[string]string `json:"request_headers"`
	RequestBody    string            `json:"request_body"`
	ResponseBody   json.RawMessage   `json:"response_body"`
	AuthScheme     string            `json:"auth_scheme"`
	TokenHint      *bank.TokenSource `json:"token_hint"`
	RecordedAt     string            `json:"recorded_at"`
}

// Candidate — массив внутри одного ответа, претендующий на роль списка счетов
// или операций.
type Candidate struct {
	Source   string // имя файла дампа
	URL      string
	Method   string
	ListPath string // gjson-путь до массива
	Count    int    // сколько элементов

	TxFields   map[string]string
	AcctFields map[string]string
	InValues   []string

	TxScore   int
	AcctScore int
	Notes     []string

	src *dump
}

// Result — итог разбора.
type Result struct {
	Accounts     *Candidate
	Cards        *Candidate
	Transactions *Candidate
	All          []*Candidate
	Files        int
}

// Analyze разбирает все дампы в каталоге.
func Analyze(dir string) (*Result, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("прочитать каталог дампов %s: %w", dir, err)
	}

	res := &Result{}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == "index.json" || !strings.HasSuffix(name, ".json") {
			continue
		}

		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}

		var d dump
		if err := json.Unmarshal(raw, &d); err != nil {
			continue
		}
		res.Files++

		if d.Status != 200 || len(d.ResponseBody) == 0 {
			continue
		}
		body := gjson.ParseBytes(d.ResponseBody)
		if !body.Exists() {
			continue
		}

		for _, hit := range collectArrays(body, "", 0) {
			if c := classify(hit, &d, name); c != nil {
				res.All = append(res.All, c)
			}
		}
	}

	res.pick()
	return res, nil
}

// pick выбирает лучших кандидатов на каждую роль.
func (r *Result) pick() {
	byTx := append([]*Candidate(nil), r.All...)
	sort.SliceStable(byTx, func(i, j int) bool { return byTx[i].TxScore > byTx[j].TxScore })
	for _, c := range byTx {
		if c.TxScore >= minScore {
			r.Transactions = c
			break
		}
	}

	// Счета и карты выглядят одинаково (остаток + валюта), поэтому берём двух
	// лучших кандидатов: первый — счета, второй — карты. У ВТБ это отдельные
	// коллекции accounts.@values и cards.@values в одном ответе.
	byAcct := append([]*Candidate(nil), r.All...)
	sort.SliceStable(byAcct, func(i, j int) bool { return byAcct[i].AcctScore > byAcct[j].AcctScore })
	for _, c := range byAcct {
		if c.AcctScore < minScore || c == r.Transactions {
			continue
		}
		switch {
		case r.Accounts == nil:
			r.Accounts = c
		case r.Cards == nil:
			r.Cards = c
		}
		if r.Accounts != nil && r.Cards != nil {
			break
		}
	}
}

// arrayHit — найденный в ответе массив объектов.
type arrayHit struct {
	path  string
	items []gjson.Result
}

// collectArrays обходит ответ и собирает все массивы объектов. Нужный массив
// может лежать на любой глубине, и заранее неизвестно, на какой именно.
func collectArrays(node gjson.Result, path string, depth int) []arrayHit {
	if depth > 5 {
		return nil
	}

	var out []arrayHit

	if node.IsArray() {
		items := node.Array()
		if len(items) > 0 && items[0].IsObject() {
			out = append(out, arrayHit{path: path, items: items})
		}
		// Внутрь элементов массива тоже заглядываем: вложенный список
		// операций внутри счёта — обычное дело.
		if len(items) > 0 {
			out = append(out, collectArrays(items[0], joinPath(path, "0"), depth+1)...)
		}
		return out
	}

	if node.IsObject() {
		// Коллекция не обязана быть массивом. ВТБ отдаёт счета и карты
		// объектом, где ключ — идентификатор продукта:
		//   "accounts": { "A1B2C3D4…": {...}, "D4C3B2A1…": {...} }
		// Для gjson такой объект превращается в массив модификатором @values.
		if items, ok := mapAsCollection(node); ok {
			out = append(out, arrayHit{path: joinPath(path, "@values"), items: items})
		}

		node.ForEach(func(k, v gjson.Result) bool {
			out = append(out, collectArrays(v, joinPath(path, escapeKey(k.String())), depth+1)...)
			return true
		})
	}

	return out
}

// mapAsCollection распознаёт объект-справочник: все значения — однотипные
// объекты, а ключи играют роль идентификаторов.
//
// Требование однородности отсекает обычные структуры вроде
// {"balance": {...}, "limits": {...}}, где значения — объекты, но разной формы.
func mapAsCollection(node gjson.Result) ([]gjson.Result, bool) {
	var items []gjson.Result
	shapes := make([]map[string]bool, 0, 8)

	homogeneous := true
	node.ForEach(func(_, v gjson.Result) bool {
		if !v.IsObject() {
			homogeneous = false
			return false
		}
		items = append(items, v)

		if len(shapes) < 8 {
			keys := map[string]bool{}
			v.ForEach(func(k, _ gjson.Result) bool {
				keys[k.String()] = true
				return true
			})
			shapes = append(shapes, keys)
		}
		return true
	})

	if !homogeneous || len(items) == 0 || len(shapes) == 0 {
		return nil, false
	}
	// Слишком мелкий объект — скорее структура, чем коллекция.
	if len(shapes[0]) < 3 {
		return nil, false
	}

	// Значения должны быть похожи друг на друга.
	for _, s := range shapes[1:] {
		if similarity(shapes[0], s) < 0.6 {
			return nil, false
		}
	}
	return items, true
}

// similarity — доля общих ключей относительно большего набора.
func similarity(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	common := 0
	for k := range a {
		if b[k] {
			common++
		}
	}
	return float64(common) / float64(max(len(a), len(b)))
}

// classify собирает поля элемента и оценивает, на что этот массив похож.
func classify(hit arrayHit, d *dump, file string) *Candidate {
	fields := flatten(hit.items)
	if len(fields) == 0 {
		return nil
	}

	c := &Candidate{
		Source:     file,
		URL:        d.URL,
		Method:     d.Method,
		ListPath:   hit.path,
		Count:      len(hit.items),
		TxFields:   map[string]string{},
		AcctFields: map[string]string{},
		src:        d,
	}

	// pick выбирает поле по форме значений, отдавая предпочтение тому, чьё имя
	// тоже подходит. Форма важнее имени: имена банк называет как хочет, а вот
	// код валюты остаётся кодом валюты.
	//
	// Имя сверяем с полным путём, а не с последним сегментом: смысл поля
	// нередко задаёт родитель — в balance.amount значение осмысленно только
	// вместе с «balance».
	pick := func(shape func(*field) bool, name *regexp.Regexp) *field {
		var fallback *field
		for _, f := range fields {
			if !shape(f) {
				continue
			}
			if name.MatchString(f.Path) {
				return f
			}
			if fallback == nil {
				fallback = f
			}
		}
		return fallback
	}

	dateF := pickDate(fields)
	currencyF := pick(looksLikeCurrency, reCurrency)
	cardF := pick(looksLikeMaskedCard, reCard)

	dirF, inValues := pickDirection(fields)
	balanceF, amountF := pickMoney(fields)
	idF := pickID(fields)
	titleF := pickTitle(fields, currencyF, cardF, idF)

	// Если поля с «говорящим» именем нет, берём любое числовое — но только
	// когда рядом есть валюта: это резко снижает шанс принять за сумму
	// какой-нибудь идентификатор.
	if amountF == nil && balanceF == nil && currencyF != nil {
		for _, f := range fields {
			if f.numeric() {
				amountF = f
				break
			}
		}
	}

	c.InValues = inValues
	c.scoreTransactions(dateF, amountF, currencyF, dirF, cardF, idF, titleF, balanceF)
	c.scoreAccounts(dateF, amountF, currencyF, cardF, idF, titleF, balanceF)

	if c.TxScore < minScore && c.AcctScore < minScore {
		return nil
	}
	return c
}

func (c *Candidate) scoreTransactions(date, amount, currency, dir, card, id, title, balance *field) {
	// Операция — это сумма в момент времени. Оба признака обязательны, и
	// требование намеренно жёсткое: без него в историю операций попадает то
	// список карт (там есть сумма, валюта и даже булев debitOperation,
	// читающийся как направление, — но нет момента совершения), то категории
	// программы лояльности (есть дата и идентификатор, но нет денег).
	if date == nil || amount == nil {
		return
	}

	add := func(n int, why string) {
		c.TxScore += n
		if why != "" {
			c.Notes = append(c.Notes, why)
		}
	}

	add(4, "есть дата: "+date.Path)
	c.TxFields["time"] = date.Path

	add(3, "есть сумма: "+amount.Path)
	c.TxFields["amount"] = amount.Path
	if amount.distinctNumbers() > 1 {
		add(2, "суммы различаются")
	}
	if dir != nil {
		add(3, "есть направление: "+dir.Path)
		c.TxFields["direction"] = dir.Path
	}
	if currency != nil {
		c.TxFields["currency"] = currency.Path
	}
	if card != nil {
		add(1, "есть маскированная карта: "+card.Path)
		c.TxFields["card"] = card.Path
	}
	if id != nil {
		add(1, "")
		c.TxFields["id"] = id.Path
	}
	if title != nil {
		c.TxFields["title"] = title.Path
	}
	if c.Count >= 3 {
		add(2, "")
	}

	// Остаток по счёту — признак продукта, а не операции.
	if balance != nil {
		add(-5, "")
	}
}

func (c *Candidate) scoreAccounts(date, amount, currency, card, id, title, balance *field) {
	add := func(n int) { c.AcctScore += n }

	money := balance
	if money == nil {
		money = amount
	}

	if balance != nil {
		add(4)
		c.AcctFields["balance"] = balance.Path
	} else if amount != nil {
		add(1)
		c.AcctFields["balance"] = amount.Path
	}
	if money == nil {
		c.AcctScore = 0
		return
	}

	if currency != nil {
		add(2)
		c.AcctFields["currency"] = currency.Path
	}
	// У счёта нет даты операции — а у операции есть. Это самый надёжный
	// признак, отличающий одно от другого.
	if date == nil {
		add(3)
	} else {
		add(-4)
	}
	if card != nil {
		add(1)
		c.AcctFields["number"] = card.Path
	}
	if title != nil {
		add(1)
		c.AcctFields["name"] = title.Path
	}
	if id != nil {
		add(1)
		c.AcctFields["id"] = id.Path
	}
	if c.Count >= 1 && c.Count <= 20 {
		add(1)
	}
}

// flatten собирает скалярные поля элементов массива. Смотрим несколько
// элементов сразу: по одному значению тип поля не определить.
func flatten(items []gjson.Result) []*field {
	const maxSamples = 5

	byPath := map[string]*field{}
	var order []string

	limit := min(len(items), maxSamples)
	for i := range limit {
		walk(items[i], "", 0, func(path string, key string, v gjson.Result) {
			f, ok := byPath[path]
			if !ok {
				f = &field{Path: path, Key: key}
				byPath[path] = f
				order = append(order, path)
			}
			f.Samples = append(f.Samples, v)
		})
	}

	out := make([]*field, 0, len(order))
	for _, p := range order {
		out = append(out, byPath[p])
	}
	return out
}

func walk(node gjson.Result, prefix string, depth int, fn func(path, key string, v gjson.Result)) {
	if depth > 3 || !node.IsObject() {
		return
	}
	node.ForEach(func(k, v gjson.Result) bool {
		key := k.String()
		path := joinPath(prefix, escapeKey(key))

		switch v.Type {
		case gjson.String, gjson.Number, gjson.True, gjson.False:
			fn(path, key, v)
		default:
			if v.IsObject() {
				walk(v, path, depth+1, fn)
			}
		}
		return true
	})
}

func joinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// escapeKey экранирует точки в имени поля — в gjson точка разделяет уровни.
func escapeKey(k string) string { return strings.ReplaceAll(k, ".", `\.`) }
