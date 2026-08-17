package discover

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Формы ниже сняты с настоящих ответов ВТБ. Каждая закрывает случай, на
// котором эвристика уже один раз ошиблась.

// Счета и карты приезжают объектом, где ключ — идентификатор продукта,
// а не массивом.
const vtbPortfolios = `{
	"accounts": {
		"A1B2C3D40000000000000000000000AC": {
			"name": "Мастер-счёт", "number": "40817810000000000000",
			"publicId": "A1B2C3D40000000000000000000000AC",
			"contractNumber": "0000000000000000",
			"openDate": "2025-09-25", "status": "Opened",
			"type": "MASTER_ACCOUNT", "active": true, "order": 1,
			"balance": {"currency": "RUB", "amount": 15000.5},
			"balanceTransferAvailableSum": 15000.5,
			"overdraft": 0, "saldo": 15000.5
		}
	},
	"cards": {
		"D4C3B2A10000000000000000000000CD": {
			"name": "Мультикарта", "number": "220220******1234",
			"publicId": "D4C3B2A10000000000000000000000CD",
			"amount": {"currency": "RUB", "amount": 15000.5},
			"debitOperation": true, "creditLimit": 0,
			"openDate": "2025-09-25", "expireDate": "2032-09-30",
			"status": "Active", "type": "DEBET_CARD", "active": true, "order": 1
		},
		"4EE6F96C5C445974CEA521E7588802C2": {
			"name": "Цифровая карта", "number": "220220******5678",
			"publicId": "4EE6F96C5C445974CEA521E7588802C2",
			"amount": {"currency": "RUB", "amount": 300},
			"debitOperation": true, "creditLimit": 0,
			"openDate": "2026-01-11", "expireDate": "2033-01-31",
			"status": "Active", "type": "DEBET_CARD", "active": true, "order": 2
		},
		"5FF7A07D6D556A85DFB632F8699913D3": {
			"name": "Дополнительная", "number": "220220******9012",
			"publicId": "5FF7A07D6D556A85DFB632F8699913D3",
			"amount": {"currency": "RUB", "amount": 0},
			"debitOperation": false, "creditLimit": 0,
			"openDate": "2026-03-02", "expireDate": "2033-03-31",
			"status": "Locked", "type": "DEBET_CARD", "active": false, "order": 3
		}
	}
}`

// В операциях направление закодировано булевым debet, идентификатор счёта
// одинаков у всех записей, а рядом с суммой лежит комиссия.
const vtbHistory = `{"operations": [
	{
		"account": "*7471", "accountProductId": "A1B2C3D40000000000000000000000AC",
		"operationId": "aaaaaaaa-0000-0000-0000-000000000001",
		"internalId": "11111111111111111111111111111111",
		"debet": false, "isHold": false,
		"operationAmount": {"currency": "RUB", "sum": 1500.5},
		"feeAmount": {"currency": "RUB", "sum": 0},
		"transactionDate": "2026-08-13T10:00:00.000+03:00",
		"operationName": "Перевод СБП",
		"parentCategory": {"id": 7, "name": "Переводы"},
		"status": "Processed"
	},
	{
		"account": "*7471", "accountProductId": "A1B2C3D40000000000000000000000AC",
		"operationId": "aaaaaaaa-0000-0000-0000-000000000002",
		"internalId": "22222222222222222222222222222222",
		"debet": true, "isHold": false,
		"operationAmount": {"currency": "RUB", "sum": 349.9},
		"feeAmount": {"currency": "RUB", "sum": 10},
		"transactionDate": "2026-08-12T19:20:00.000+03:00",
		"operationName": "Пятёрочка",
		"parentCategory": {"id": 3, "name": "Супермаркеты"},
		"status": "Processed"
	},
	{
		"account": "*7471", "accountProductId": "A1B2C3D40000000000000000000000AC",
		"operationId": "aaaaaaaa-0000-0000-0000-000000000003",
		"internalId": "33333333333333333333333333333333",
		"debet": false, "isHold": false,
		"operationAmount": {"currency": "RUB", "sum": 80000},
		"feeAmount": {"currency": "RUB", "sum": 0},
		"transactionDate": "2026-08-12T09:05:00.000+03:00",
		"operationName": "ООО Ромашка",
		"parentCategory": {"id": 1, "name": "Зарплата"},
		"status": "Processed"
	}
]}`

// Настройки интерфейса: числа рядом со словом BALANCES, но денег тут нет.
const vtbSettings = `{
	"globalProductProperties": {"favoriteProduct": {"id": "X", "type": "CARD", "order": 1}},
	"sections": {
		"LIVE_BALANCES": {"displayOrder": 3, "visible": true, "collapsed": false},
		"MAIN_PRODUCTS": {"displayOrder": 1, "visible": true, "collapsed": false}
	}
}`

func vtbFixture(t *testing.T) *Result {
	t.Helper()
	dir := t.TempDir()

	write := func(name, url, body string) {
		d := map[string]any{
			"url": url, "method": "GET", "status": 200,
			"request_headers": map[string]string{},
			"response_body":   json.RawMessage(body),
			"auth_scheme":     "Bearer",
			"token_hint":      map[string]string{"storage": "session", "key": "@vtb/auth"},
			"recorded_at":     "2026-08-13T22:00:00+03:00",
		}
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	write("portfolios.json", "https://online.sbpvtb.ru/msa/api-gw/private/portfolio/portfolio-main-page/portfolios/active", vtbPortfolios)
	write("history.json", "https://online.sbpvtb.ru/msa/api-gw/private/history-hub/history-hub-homer/v1/history/byAccount?dateFrom=2026-07-13T00:00:00.000%2B03:00&dateTo=2026-08-13T23:59:59.999%2B03:00&account=A1B2C3D4", vtbHistory)
	write("settings.json", "https://online.sbpvtb.ru/msa/api-gw/private/rrbs/rrbs-client-settings/settings-object", vtbSettings)

	res, err := Analyze(dir)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	return res
}

// Коллекция может быть объектом с ключами-идентификаторами, а не массивом.
func TestVTBAccountsFromKeyedObject(t *testing.T) {
	res := vtbFixture(t)

	if res.Accounts == nil {
		t.Fatal("счета не найдены")
	}
	if res.Accounts.ListPath != "accounts.@values" {
		t.Errorf("ListPath = %q, ожидалось accounts.@values", res.Accounts.ListPath)
	}
	if got := res.Accounts.AcctFields["balance"]; got != "balance.amount" {
		t.Errorf("balance = %q, ожидалось balance.amount", got)
	}
	if got := res.Accounts.AcctFields["currency"]; got != "balance.currency" {
		t.Errorf("currency = %q, ожидалось balance.currency", got)
	}
}

// У счёта есть openDate, и она не должна выдавать его за список операций.
func TestVTBAccountDateDoesNotBreakDetection(t *testing.T) {
	res := vtbFixture(t)

	if res.Accounts == nil {
		t.Fatal("счета не найдены — openDate снова принят за дату операции")
	}
	if res.Accounts.TxFields["time"] != "" {
		t.Errorf("openDate попал во время операции: %q", res.Accounts.TxFields["time"])
	}
}

// Направление закодировано булевым debet: true — списание.
func TestVTBBooleanDirection(t *testing.T) {
	res := vtbFixture(t)

	tx := res.Transactions
	if tx == nil {
		t.Fatal("операции не найдены")
	}
	if got := tx.TxFields["direction"]; got != "debet" {
		t.Errorf("direction = %q, ожидалось debet", got)
	}
	if len(tx.InValues) != 1 || tx.InValues[0] != "false" {
		t.Errorf("InValues = %v, ожидалось [false] — зачисление там, где debet снят", tx.InValues)
	}
}

// accountProductId одинаков у всех операций: взяв его за идентификатор,
// бот схлопнул бы всю историю в одну запись.
func TestVTBIDMustBeUnique(t *testing.T) {
	res := vtbFixture(t)

	got := res.Transactions.TxFields["id"]
	if got == "accountProductId" || got == "account" {
		t.Fatalf("в ID попало повторяющееся поле %q", got)
	}
	if got != "operationId" && got != "internalId" {
		t.Errorf("id = %q, ожидался operationId или internalId", got)
	}
}

// Рядом с суммой операции лежит комиссия — она не должна её подменять.
func TestVTBAmountIsNotFee(t *testing.T) {
	res := vtbFixture(t)

	if got := res.Transactions.TxFields["amount"]; got != "operationAmount.sum" {
		t.Errorf("amount = %q, ожидалось operationAmount.sum", got)
	}
}

// «Пятёрочка» информативнее, чем категория «Супермаркеты».
func TestVTBTitlePrefersCounterpartyOverCategory(t *testing.T) {
	res := vtbFixture(t)

	if got := res.Transactions.TxFields["title"]; got != "operationName" {
		t.Errorf("title = %q, ожидалось operationName", got)
	}
}

func TestVTBMaskedCard(t *testing.T) {
	res := vtbFixture(t)

	if got := res.Transactions.TxFields["card"]; got != "account" {
		t.Errorf("card = %q, ожидалось account (там лежит *7471)", got)
	}
}

// Список карт обманчиво похож на список операций: есть сумма, валюта и даже
// булев debitOperation, который читается как направление. Отличает их
// отсутствие времени операции — по нему и разводим.
func TestVTBCardsAreNotTransactions(t *testing.T) {
	res := vtbFixture(t)

	tx := res.Transactions
	if tx == nil {
		t.Fatal("операции не найдены")
	}
	if tx.ListPath == "cards.@values" {
		t.Fatal("список карт принят за историю операций")
	}
	if filepath.Base(tx.Source) != "history.json" {
		t.Errorf("операции взяты из %s, ожидался history.json", tx.Source)
	}
	if got := tx.TxFields["direction"]; got == "debitOperation" {
		t.Errorf("флаг карты %q принят за направление операции", got)
	}
}

// Категории программы лояльности: дата и идентификатор есть, денег нет.
// Операцией такое быть не может.
func TestVTBLoyaltyCategoriesAreNotTransactions(t *testing.T) {
	dir := t.TempDir()

	body := `{"loyaltyPrograms": {"19-NPL": {"name": "Мультибонус", "code": "NPL",
		"isActive": true, "categories": [
			{"id": 1, "createTime": "2026-08-01T10:00:00.000+03:00", "code": "SUPERMARKET",
			 "categoryAttributes": {"description": "Супермаркеты", "percent": 5}},
			{"id": 2, "createTime": "2026-08-02T10:00:00.000+03:00", "code": "FUEL",
			 "categoryAttributes": {"description": "Заправки", "percent": 3}},
			{"id": 3, "createTime": "2026-08-03T10:00:00.000+03:00", "code": "PHARMACY",
			 "categoryAttributes": {"description": "Аптеки", "percent": 4}}
		]}}}`

	d := map[string]any{
		"url":    "https://online.sbpvtb.ru/msa/api-gw/private/portfolio/portfolio-main-page/portfolios/active",
		"method": "GET", "status": 200,
		"request_headers": map[string]string{},
		"response_body":   json.RawMessage(body),
		"recorded_at":     "2026-08-13T22:00:00+03:00",
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "loyalty.json"), raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	res, err := Analyze(dir)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if res.Transactions != nil {
		t.Errorf("категории приняты за операции: %s", res.Transactions.ListPath)
	}
}

// Счета и карты лежат в одном ответе — счетами должны стать именно счета.
func TestVTBAccountsWinOverCards(t *testing.T) {
	res := vtbFixture(t)

	if res.Accounts == nil {
		t.Fatal("счета не найдены")
	}
	if res.Accounts.ListPath != "accounts.@values" {
		t.Errorf("ListPath = %q, ожидалось accounts.@values", res.Accounts.ListPath)
	}
}

// sections.LIVE_BALANCES.displayOrder — порядок отображения, а не остаток.
func TestVTBSettingsAreNotAccounts(t *testing.T) {
	res := vtbFixture(t)

	for _, c := range []*Candidate{res.Accounts, res.Transactions} {
		if c != nil && filepath.Base(c.Source) == "settings.json" {
			t.Errorf("настройки интерфейса приняты за данные: %s", c.ListPath)
		}
	}
}

// Диапазон дат в запросе должен пересчитываться от текущего момента.
func TestVTBTimeParamsExtracted(t *testing.T) {
	res := vtbFixture(t)

	spec := res.Endpoints().Transactions
	if spec == nil {
		t.Fatal("ручка операций не собрана")
	}

	from, ok := spec.Params["dateFrom"]
	if !ok {
		t.Fatal("dateFrom не стал вычисляемым параметром — диапазон застынет навсегда")
	}
	// Записано в 22:00, а период начинался в 00:00 на 31 день раньше —
	// это 31 день и 22 часа. Округляем в сторону расширения окна.
	if from.Offset != "-32d" {
		t.Errorf("dateFrom.Offset = %q, ожидалось -32d", from.Offset)
	}
	if from.Layout == "" {
		t.Error("не сохранён формат даты")
	}

	to, ok := spec.Params["dateTo"]
	if !ok {
		t.Fatal("dateTo не стал вычисляемым параметром")
	}
	if to.Offset != "0d" {
		t.Errorf("dateTo.Offset = %q, ожидалось 0d", to.Offset)
	}

	// Идентификатор счёта датой не является и трогать его не надо.
	if _, ok := spec.Params["account"]; ok {
		t.Error("account ошибочно принят за дату")
	}
}

// Листая историю, фронт запрашивает всё более старые страницы, и записаться
// может любая из них. Окно обязано приехать к «сейчас», иначе бот навсегда
// застрянет в позапрошлом месяце.
func TestTimeParamsAnchorOldWindowToNow(t *testing.T) {
	// Пойманная страница: май–июнь, при том что запись велась в августе.
	raw := "https://online.sbpvtb.ru/x?dateFrom=2026-05-16T00:00:00.000%2B03:00" +
		"&dateTo=2026-06-15T23:59:59.999%2B03:00"

	got := timeParams(raw, "2026-08-17T05:04:43+03:00")

	if got["dateTo"].Offset != "0d" {
		t.Errorf("dateTo.Offset = %q, ожидалось 0d — верхняя граница должна быть «сейчас»",
			got["dateTo"].Offset)
	}
	// Длительность периода сохраняется: с 16 мая по 15 июня — 30 суток и почти сутки.
	if got["dateFrom"].Offset != "-31d" {
		t.Errorf("dateFrom.Offset = %q, ожидалось -31d", got["dateFrom"].Offset)
	}
}

// Одиночную дату сдвигать не от чего — она считается от момента записи.
func TestTimeParamsSingleDate(t *testing.T) {
	raw := "https://online.sbpvtb.ru/x?since=2026-07-18T00:00:00.000%2B03:00"

	got := timeParams(raw, "2026-08-17T05:00:00+03:00")

	if got["since"].Offset != "-31d" {
		t.Errorf("since.Offset = %q, ожидалось -31d", got["since"].Offset)
	}
}
