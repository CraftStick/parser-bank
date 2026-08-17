package discover

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/valerakrut/parserbank/internal/bank"
)

// writeDump кладёт в каталог один дамп в том формате, который пишет recorder.
func writeDump(t *testing.T, dir, name, url, method, body string) {
	t.Helper()

	d := map[string]any{
		"url":             url,
		"method":          method,
		"status":          200,
		"request_headers": map[string]string{"content-type": "application/json"},
		"request_body":    `{"count":20}`,
		"response_body":   json.RawMessage(body),
		"auth_scheme":     "Bearer",
		"token_hint":      map[string]string{"storage": "session", "key": "auth", "path": "access_token"},
	}

	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("сериализовать дамп: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
		t.Fatalf("записать дамп: %v", err)
	}
}

const accountsBody = `{"data":{"accounts":[
	{"id":"acc-1","name":"Мастер-счёт","maskedNumber":"•• 4321",
	 "balance":{"amount":15000.5,"currency":"RUB"}},
	{"id":"acc-2","name":"Накопительный","maskedNumber":"•• 8765",
	 "balance":{"amount":230000,"currency":"RUB"}}
]}}`

const transactionsBody = `{"data":{"items":[
	{"id":"op-1","operationDate":"2026-08-13T10:00:00Z","direction":"CREDIT",
	 "amount":{"sum":1500.5,"currency":"RUB"},
	 "counterpartyName":"Иванов И.И.","cardMaskedNumber":"•• 4321"},
	{"id":"op-2","operationDate":"2026-08-12T19:20:00Z","direction":"DEBIT",
	 "amount":{"sum":349.9,"currency":"RUB"},
	 "counterpartyName":"Пятёрочка","cardMaskedNumber":"•• 4321"},
	{"id":"op-3","operationDate":"2026-08-12T09:05:00Z","direction":"CREDIT",
	 "amount":{"sum":80000,"currency":"RUB"},
	 "counterpartyName":"ООО Ромашка","cardMaskedNumber":"•• 4321"}
]}}`

// Меню и настройки — тоже массивы объектов, и они не должны сбивать разбор.
const noiseBody = `{"data":{"menu":[
	{"id":"m1","title":"Платежи","icon":"pay","order":1},
	{"id":"m2","title":"История","icon":"clock","order":2},
	{"id":"m3","title":"Настройки","icon":"gear","order":3}
]}}`

func analyzeFixture(t *testing.T) (*Result, string) {
	t.Helper()

	dir := t.TempDir()
	writeDump(t, dir, "a.json", "https://online.sbpvtb.ru/services/accounts/list", "POST", accountsBody)
	writeDump(t, dir, "b.json", "https://online.sbpvtb.ru/services/operations/history", "POST", transactionsBody)
	writeDump(t, dir, "c.json", "https://online.sbpvtb.ru/services/ui/menu", "GET", noiseBody)

	res, err := Analyze(dir)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	return res, dir
}

func TestAnalyzeFindsTransactions(t *testing.T) {
	res, _ := analyzeFixture(t)

	tx := res.Transactions
	if tx == nil {
		t.Fatal("операции не найдены")
	}
	if tx.ListPath != "data.items" {
		t.Errorf("ListPath = %q, ожидалось data.items", tx.ListPath)
	}
	if tx.Count != 3 {
		t.Errorf("Count = %d, ожидалось 3", tx.Count)
	}

	want := map[string]string{
		"id":        "id",
		"time":      "operationDate",
		"amount":    "amount.sum",
		"currency":  "amount.currency",
		"direction": "direction",
		"title":     "counterpartyName",
		"card":      "cardMaskedNumber",
	}
	for role, path := range want {
		if got := tx.TxFields[role]; got != path {
			t.Errorf("поле %s = %q, ожидалось %q", role, got, path)
		}
	}

	if len(tx.InValues) != 1 || tx.InValues[0] != "CREDIT" {
		t.Errorf("InValues = %v, ожидалось [CREDIT]", tx.InValues)
	}
}

func TestAnalyzeFindsAccounts(t *testing.T) {
	res, _ := analyzeFixture(t)

	acc := res.Accounts
	if acc == nil {
		t.Fatal("счета не найдены")
	}
	if acc.ListPath != "data.accounts" {
		t.Errorf("ListPath = %q, ожидалось data.accounts", acc.ListPath)
	}

	want := map[string]string{
		"id":       "id",
		"name":     "name",
		"number":   "maskedNumber",
		"balance":  "balance.amount",
		"currency": "balance.currency",
	}
	for role, path := range want {
		if got := acc.AcctFields[role]; got != path {
			t.Errorf("поле %s = %q, ожидалось %q", role, got, path)
		}
	}
}

func TestAnalyzeIgnoresNoise(t *testing.T) {
	res, _ := analyzeFixture(t)

	// Массив пунктов меню не должен попасть ни в одну из ролей.
	for _, c := range []*Candidate{res.Accounts, res.Transactions} {
		if c != nil && c.ListPath == "data.menu" {
			t.Errorf("меню принято за %s", c.URL)
		}
	}
}

func TestAnalyzeRolesAreDistinct(t *testing.T) {
	res, _ := analyzeFixture(t)

	if res.Accounts == nil || res.Transactions == nil {
		t.Fatal("нашлось не всё")
	}
	if res.Accounts == res.Transactions {
		t.Error("один массив назначен и счетами, и операциями")
	}
	if res.Accounts.URL == res.Transactions.URL {
		t.Errorf("обе роли указывают на одну ручку: %s", res.Accounts.URL)
	}
}

// Главная проверка: то, что сгенерировал анализатор, должно без правок
// приниматься самим клиентом.
func TestGeneratedConfigLoadsBack(t *testing.T) {
	res, dir := analyzeFixture(t)

	path := filepath.Join(dir, "endpoints.json")
	if err := Save(res, path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := bank.LoadEndpoints(path)
	if err != nil {
		t.Fatalf("сгенерированный конфиг не читается клиентом: %v", err)
	}

	if loaded.Accounts == nil || loaded.Transactions == nil {
		t.Fatal("в конфиге описаны не обе ручки")
	}
	if loaded.Transactions.List != "data.items" {
		t.Errorf("list = %q", loaded.Transactions.List)
	}
	if loaded.Transactions.Method != "POST" {
		t.Errorf("method = %q, ожидался POST", loaded.Transactions.Method)
	}
	if got := loaded.Transactions.Headers["Authorization"]; got != "Bearer {{token}}" {
		t.Errorf("Authorization = %q, ожидалось Bearer {{token}}", got)
	}
	if loaded.Transactions.Token == nil || loaded.Transactions.Token.Key != "auth" {
		t.Errorf("источник токена не перенесён: %+v", loaded.Transactions.Token)
	}
	// Тело сравниваем по смыслу: при записи конфига JSON переформатируется,
	// и побайтовое равенство тут ничего не значит.
	var compact bytes.Buffer
	if err := json.Compact(&compact, loaded.Transactions.Body); err != nil {
		t.Fatalf("тело запроса — не валидный JSON: %v", err)
	}
	if compact.String() != `{"count":20}` {
		t.Errorf("body = %s", compact.String())
	}
}

func TestSaveKeepsExistingFile(t *testing.T) {
	res, dir := analyzeFixture(t)

	// Ручные правки дороже свежей догадки: существующий файл не трогаем,
	// предложение кладём рядом.
	path := filepath.Join(dir, "endpoints.json")
	original := []byte(`{"мой":"конфиг"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("подготовить файл: %v", err)
	}

	if err := Save(res, path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("прочитать файл: %v", err)
	}
	if string(got) != string(original) {
		t.Errorf("существующий файл затёрт: %s", got)
	}

	if _, err := os.Stat(filepath.Join(dir, "endpoints.suggested.json")); err != nil {
		t.Errorf("предложение не записано рядом: %v", err)
	}
}

func TestAnalyzeEmptyDir(t *testing.T) {
	res, err := Analyze(t.TempDir())
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if res.Accounts != nil || res.Transactions != nil {
		t.Error("на пустом каталоге что-то нашлось")
	}
}

func TestAnalyzeMissingDir(t *testing.T) {
	if _, err := Analyze(filepath.Join(t.TempDir(), "нет-такого")); err == nil {
		t.Error("ожидалась ошибка на несуществующем каталоге")
	}
}
