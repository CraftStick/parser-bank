package bank

import (
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func TestMoney(t *testing.T) {
	cases := []struct {
		amount   float64
		currency string
		want     string
	}{
		{12345.67, "RUB", "12 345,67 ₽"},
		{0, "RUB", "0,00 ₽"},
		{1234.5, "RUB", "1 234,50 ₽"},
		{-1234.5, "RUB", "-1 234,50 ₽"},
		{999.99, "USD", "999,99 $"},
		{1000000, "EUR", "1 000 000,00 €"},
		{100, "KZT", "100,00 KZT"},
	}

	for _, c := range cases {
		if got := Money(c.amount, c.currency); got != c.want {
			t.Errorf("Money(%v, %q) = %q, ожидалось %q", c.amount, c.currency, got, c.want)
		}
	}
}

func TestParseTime(t *testing.T) {
	cases := []struct {
		in     string
		layout string
		want   string // в формате RFC3339, пусто — ожидаем нулевое время
	}{
		{"2026-08-13T18:30:00Z", "", "2026-08-13T18:30:00Z"},
		{"2026-08-13 18:30:00", "", "2026-08-13T18:30:00Z"},
		{"13.08.2026 18:30", "", "2026-08-13T18:30:00Z"},
		{"13/08/2026", "02/01/2006", "2026-08-13T00:00:00Z"},
		{"1786645800000", "", "2026-08-13T18:30:00Z"}, // миллисекунды эпохи
		{"1786645800", "", "2026-08-13T18:30:00Z"},    // секунды эпохи
		{"", "", ""},
		{"не дата", "", ""},
	}

	for _, c := range cases {
		got := parseTime(c.in, c.layout)
		if c.want == "" {
			if !got.IsZero() {
				t.Errorf("parseTime(%q) = %v, ожидалось нулевое время", c.in, got)
			}
			continue
		}
		if got.UTC().Format(time.RFC3339) != c.want {
			t.Errorf("parseTime(%q, %q) = %v, ожидалось %s", c.in, c.layout, got.UTC(), c.want)
		}
	}
}

func TestSelectList(t *testing.T) {
	root := gjson.Parse(`{"data":{"items":[{"a":1},{"a":2}]},"single":{"a":3}}`)

	if got := selectList(root, "data.items"); len(got) != 2 {
		t.Errorf("вложенный массив: получено %d элементов, ожидалось 2", len(got))
	}
	// Одиночный объект оборачивается в список из одного элемента.
	if got := selectList(root, "single"); len(got) != 1 {
		t.Errorf("одиночный объект: получено %d элементов, ожидался 1", len(got))
	}
	if got := selectList(root, "нет.такого.пути"); got != nil {
		t.Errorf("несуществующий путь: ожидался nil, получено %v", got)
	}
	if got := selectList(gjson.Parse(`[{"a":1}]`), ""); len(got) != 1 {
		t.Errorf("массив в корне: получено %d элементов, ожидался 1", len(got))
	}
}

func TestMapTransactionByDirectionField(t *testing.T) {
	spec := &EndpointSpec{
		Fields: map[string]string{
			"id":        "id",
			"time":      "date",
			"amount":    "amount.sum",
			"currency":  "amount.currency",
			"direction": "type",
			"title":     "counterparty",
			"card":      "card",
		},
		InValues: []string{"CREDIT"},
	}

	in := gjson.Parse(`{
		"id": "op-1",
		"date": "2026-08-13T18:30:00Z",
		"amount": {"sum": 1500.5, "currency": "RUB"},
		"type": "CREDIT",
		"counterparty": "Иванов И.И.",
		"card": "•• 4321"
	}`)

	tx := mapTransaction(in, spec)

	if tx.ID != "op-1" {
		t.Errorf("ID = %q, ожидалось op-1", tx.ID)
	}
	if tx.Direction != DirectionIn {
		t.Errorf("Direction = %q, ожидалось %q", tx.Direction, DirectionIn)
	}
	if tx.Amount != 1500.5 {
		t.Errorf("Amount = %v, ожидалось 1500.5", tx.Amount)
	}
	if tx.Title != "Иванов И.И." {
		t.Errorf("Title = %q", tx.Title)
	}
	if tx.Card != "•• 4321" {
		t.Errorf("Card = %q", tx.Card)
	}

	// То же поле со значением не из in_values — это списание.
	out := gjson.Parse(`{"id":"op-2","type":"DEBIT","amount":{"sum":700,"currency":"RUB"}}`)
	if got := mapTransaction(out, spec); got.Direction != DirectionOut {
		t.Errorf("Direction = %q, ожидалось %q", got.Direction, DirectionOut)
	}
}

func TestMapTransactionBySign(t *testing.T) {
	// Поля direction нет — направление определяется по знаку суммы,
	// а сама сумма нормализуется в положительную.
	spec := &EndpointSpec{
		Fields: map[string]string{"id": "id", "amount": "sum"},
	}

	credit := mapTransaction(gjson.Parse(`{"id":"a","sum":250.25}`), spec)
	if credit.Direction != DirectionIn || credit.Amount != 250.25 {
		t.Errorf("зачисление: direction=%q amount=%v", credit.Direction, credit.Amount)
	}

	debit := mapTransaction(gjson.Parse(`{"id":"b","sum":-99.9}`), spec)
	if debit.Direction != DirectionOut || debit.Amount != 99.9 {
		t.Errorf("списание: direction=%q amount=%v", debit.Direction, debit.Amount)
	}
}

// Ключ обязан считаться по содержимому: ВТБ выдаёт в operationId новый UUID
// при каждом запросе, и с ним каждая операция выглядела бы новой — бот слал
// бы уведомление об одном и том же поступлении раз в минуту.
func TestKeyIgnoresRotatingServerID(t *testing.T) {
	base := Transaction{
		Time:      time.Date(2026, 8, 7, 11, 2, 49, 0, time.UTC),
		Amount:    1000,
		Currency:  "RUB",
		Direction: DirectionIn,
		Title:     "Стипендия",
		Card:      "*7471",
	}

	first := base
	first.ID = "01be1033-2d05-4a7c-9d48-000000000001"
	second := base
	second.ID = "7e13dfac-f11f-4949-8b5f-000000000002"

	a := []Transaction{first}
	b := []Transaction{second}
	assignKeys(a)
	assignKeys(b)

	if a[0].ID != b[0].ID {
		t.Errorf("ключ зависит от идентификатора банка: %q против %q", a[0].ID, b[0].ID)
	}
	if a[0].Ref != first.ID {
		t.Errorf("идентификатор банка потерян: Ref = %q", a[0].Ref)
	}
}

func TestKeyDistinguishesDifferentOperations(t *testing.T) {
	at := time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)

	txs := []Transaction{
		{Time: at, Amount: 100, Currency: "RUB", Direction: DirectionIn, Title: "Кофе"},
		{Time: at, Amount: 200, Currency: "RUB", Direction: DirectionIn, Title: "Кофе"},
		{Time: at, Amount: 100, Currency: "RUB", Direction: DirectionOut, Title: "Кофе"},
		{Time: at, Amount: 100, Currency: "RUB", Direction: DirectionIn, Title: "Чай"},
	}
	assignKeys(txs)

	seen := map[string]bool{}
	for _, tx := range txs {
		if seen[tx.ID] {
			t.Errorf("разные операции получили одинаковый ключ: %+v", tx)
		}
		seen[tx.ID] = true
	}
}

// Две неотличимые операции в одном ответе — редкость, но терять одну из них
// молча не годится.
func TestKeyKeepsIdenticalOperationsApart(t *testing.T) {
	at := time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)
	tx := Transaction{Time: at, Amount: 100, Currency: "RUB", Direction: DirectionIn, Title: "Перевод"}

	txs := []Transaction{tx, tx, tx}
	assignKeys(txs)

	if txs[0].ID == txs[1].ID || txs[1].ID == txs[2].ID || txs[0].ID == txs[2].ID {
		t.Errorf("одинаковые операции слились: %q, %q, %q", txs[0].ID, txs[1].ID, txs[2].ID)
	}

	// При этом набор ключей должен повторяться от опроса к опросу.
	again := []Transaction{tx, tx, tx}
	assignKeys(again)
	for i := range txs {
		if txs[i].ID != again[i].ID {
			t.Errorf("ключ %d не воспроизвёлся: %q против %q", i, txs[i].ID, again[i].ID)
		}
	}
}

func TestNumParsesStringAmounts(t *testing.T) {
	// Суммы нередко приходят строкой с пробелами и запятой.
	it := gjson.Parse(`{"a":"1 234,56","b":"789.01","c":1500}`)

	if got := num(it, "a"); got != 1234.56 {
		t.Errorf(`num("1 234,56") = %v, ожидалось 1234.56`, got)
	}
	if got := num(it, "b"); got != 789.01 {
		t.Errorf(`num("789.01") = %v, ожидалось 789.01`, got)
	}
	if got := num(it, "c"); got != 1500 {
		t.Errorf("num(1500) = %v, ожидалось 1500", got)
	}
	if got := num(it, ""); got != 0 {
		t.Errorf("пустой путь: %v, ожидался 0", got)
	}
}
