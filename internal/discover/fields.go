package discover

import (
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// field — одно скалярное поле внутри элемента массива, собранное по нескольким
// элементам сразу: по одному значению тип не угадать, а по пяти уже видно.
type field struct {
	Path    string // gjson-путь внутри элемента
	Key     string // последний сегмент пути
	Samples []gjson.Result
}

// last — последний сегмент пути. Именно он называет само поле, тогда как
// родитель лишь уточняет смысл: в sections.LIVE_BALANCES.displayOrder речь
// про порядок отображения, а вовсе не про остаток.
func (f *field) last() string {
	if i := strings.LastIndex(f.Path, "."); i >= 0 {
		return f.Path[i+1:]
	}
	return f.Path
}

// depth — глубина вложенности поля.
func (f *field) depth() int { return strings.Count(f.Path, ".") }

// distinct — сколько различных значений встретилось среди образцов.
func (f *field) distinct() int {
	seen := map[string]bool{}
	for _, s := range f.Samples {
		seen[s.Raw] = true
	}
	return len(seen)
}

// unique — значения различны у всех образцов. Поле, повторяющееся из записи
// в запись, не годится в идентификатор: по нему все операции слились бы в одну.
func (f *field) unique() bool {
	return len(f.Samples) > 1 && f.distinct() == len(f.Samples)
}

func (f *field) boolean() bool {
	for _, s := range f.Samples {
		if s.Type != gjson.True && s.Type != gjson.False {
			return false
		}
	}
	return len(f.Samples) > 0
}

func (f *field) numeric() bool {
	for _, s := range f.Samples {
		if s.Type == gjson.Number {
			return true
		}
		// Суммы часто приезжают строкой: "1 234,56"
		if s.Type == gjson.String && reNumericString.MatchString(s.String()) {
			return true
		}
	}
	return false
}

func (f *field) strings() []string {
	out := make([]string, 0, len(f.Samples))
	for _, s := range f.Samples {
		if s.Type == gjson.String {
			out = append(out, s.String())
		}
	}
	return out
}

// distinctNumbers — сколько различных числовых значений встретилось. Помогает
// отличить список операций (суммы разные) от служебного массива, где одно и то
// же число повторяется.
func (f *field) distinctNumbers() int {
	seen := map[float64]bool{}
	for _, s := range f.Samples {
		if s.Type == gjson.Number {
			seen[s.Float()] = true
		}
	}
	return len(seen)
}

var (
	reNumericString = regexp.MustCompile(`^-?[\d\s\x{00a0}]+([.,]\d+)?$`)

	reBalance   = regexp.MustCompile(`(?i)(balance|остат|available|actual|rest)`)
	reAmount    = regexp.MustCompile(`(?i)(amount|sum|value|сумм|total|money)`)
	reCurrency  = regexp.MustCompile(`(?i)(currency|curr|валют|iso)`)
	reDate      = regexp.MustCompile(`(?i)(date|time|created|дата|врем)`)
	reCard      = regexp.MustCompile(`(?i)(card|pan|masked|числ|number|счет|счёт|account)`)
	reID        = regexp.MustCompile(`(?i)(^id$|uuid|guid|id$|reference|^ref$|key$)`)
	reTitle     = regexp.MustCompile(`(?i)(name|title|descr|purpose|counterparty|payee|merchant|comment|назнач|получат|отправит|контрагент|коммент)`)
	reDirection = regexp.MustCompile(`(?i)(direction|drcr|debitcredit|operationtype|^type$|kind|^sign$|flow|напр)`)

	// Флаги-булевы, которыми банк кодирует направление операции.
	// ВТБ, например, кладёт в каждую операцию debet: true/false.
	reDebitFlag  = regexp.MustCompile(`(?i)(debet|debit|expense|outgoing|withdraw|расход|списан)`)
	reCreditFlag = regexp.MustCompile(`(?i)(credit|income|incoming|deposit|приход|зачисл)`)

	// Названия, по которым узнаём контрагента или назначение платежа.
	// Они информативнее, чем просто name: у ВТБ рядом лежит parentCategory.name
	// с категорией вроде «Переводы», и в уведомлении она бесполезна.
	reStrongTitle = regexp.MustCompile(`(?i)(counterparty|payee|merchant|purpose|descr|operationname|назнач|получат|отправит|контрагент)`)

	// Маскированный номер карты: •• 4321, *4321, 4276 **** **** 1234
	reMaskedCard = regexp.MustCompile(`[*•·]{1,}\s*\d{2,4}|\d{4}[\s*•·]+\d{4}`)

	// Коды валют — буквенные и цифровые по ISO 4217.
	reCurrencyCode = regexp.MustCompile(`^(?i)(RUB|RUR|USD|EUR|CNY|GBP|CHF|KZT|BYN|643|810|840|978|156)$`)
)

// directionValues — значения, которыми банки обозначают направление операции.
var directionValues = map[string]bool{
	"credit": true, "debit": true,
	"c": true, "d": true,
	"in": true, "out": true,
	"income": true, "expense": true,
	"incoming": true, "outgoing": true,
	"deposit": true, "withdrawal": true,
	"приход": true, "расход": true,
	"зачисление": true, "списание": true,
	"+": true, "-": true,
}

// dateLayouts — форматы, по которым узнаём дату. Совпадает с тем, что умеет
// разбирать сам клиент.
var dateLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
	"02.01.2006 15:04:05",
	"02.01.2006 15:04",
	"02.01.2006",
}

func looksLikeDate(f *field) bool {
	for _, s := range f.strings() {
		if s == "" {
			continue
		}
		for _, l := range dateLayouts {
			if _, err := time.Parse(l, s); err == nil {
				return true
			}
		}
	}
	// Эпоха в миллисекундах: диапазон примерно с 2001 по 2286 год.
	for _, s := range f.Samples {
		if s.Type == gjson.Number {
			v := s.Int()
			if v > 1_000_000_000_000 && v < 10_000_000_000_000 {
				return true
			}
		}
	}
	return false
}

func looksLikeCurrency(f *field) bool {
	vals := f.strings()
	if len(vals) == 0 {
		return false
	}
	for _, v := range vals {
		if !reCurrencyCode.MatchString(strings.TrimSpace(v)) {
			return false
		}
	}
	return true
}

func looksLikeDirection(f *field) bool {
	vals := f.strings()
	if len(vals) == 0 {
		return false
	}
	for _, v := range vals {
		if !directionValues[strings.ToLower(strings.TrimSpace(v))] {
			return false
		}
	}
	return true
}

func looksLikeMaskedCard(f *field) bool {
	return slices.ContainsFunc(f.strings(), reMaskedCard.MatchString)
}

var (
	// Поля, которые называются суммой, но основной суммой операции не являются.
	reSecondaryMoney = regexp.MustCompile(
		`(?i)(fee|commission|comission|limit|available|overdraft|^min|^max|debt|liability|hold|bonus|miles|cashback|transfer)`)

	// Идентификаторы, привязанные к самой записи, а не к продукту или клиенту.
	reStrongID = regexp.MustCompile(`(?i)^(operation|internal|transaction|document|record|entry|event)`)

	// Даты, которые относятся к продукту или к служебной метке времени,
	// а не к моменту операции. У счёта есть openDate, у карты — expireDate,
	// и принимать их за время операции нельзя: иначе счета выглядят как
	// история платежей.
	reNonOperationDate = regexp.MustCompile(
		`(?i)(open|close|expire|start|end|issue|valid|birth|actual|updated|modified|sync|refresh)`)
)

// boolDirectionInValues распознаёт направление, закодированное булевым флагом,
// и возвращает значения, означающие зачисление.
//
// Читается такое поле как строка "true"/"false" — именно в этом виде его
// отдаёт gjson, и именно с ним потом сравнивает клиент.
func boolDirectionInValues(f *field) ([]string, bool) {
	if !f.boolean() {
		return nil, false
	}
	switch {
	case reDebitFlag.MatchString(f.Path):
		// Флаг «это списание» → зачисление там, где он снят.
		return []string{"false"}, true
	case reCreditFlag.MatchString(f.Path):
		return []string{"true"}, true
	}
	return nil, false
}

// directionInValues возвращает те из встреченных значений, которые означают
// зачисление, — их надо положить в in_values.
func directionInValues(f *field) []string {
	in := map[string]bool{
		"credit": true, "c": true, "in": true, "income": true,
		"incoming": true, "deposit": true, "приход": true,
		"зачисление": true, "+": true,
	}

	seen := map[string]bool{}
	var out []string
	for _, v := range f.strings() {
		v = strings.TrimSpace(v)
		if in[strings.ToLower(v)] && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
