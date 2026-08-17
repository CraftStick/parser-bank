package bank

import (
	"strings"
	"testing"
)

func TestTrimAmount(t *testing.T) {
	cases := map[float64]string{
		5000:    "5000",
		5000.5:  "5000.5",
		1234.56: "1234.56",
		0.9:     "0.9",
		100.0:   "100",
	}
	for in, want := range cases {
		if got := trimAmount(in); got != want {
			t.Errorf("trimAmount(%v) = %q, ожидалось %q", in, got, want)
		}
	}
}

func TestSubstitute(t *testing.T) {
	req := TransferRequest{
		Amount:  5000,
		Phone:   "+79991234567",
		Bank:    "Т-Банк",
		Comment: "Себе",
	}

	cases := map[string]string{
		"{amount}":           "5000",
		"{phone}":            "+79991234567",
		"{bank}":             "Т-Банк",
		"{comment}":          "Себе",
		"Перевод {amount} ₽": "Перевод 5000 ₽",
		"без подстановок":    "без подстановок",
	}
	for tmpl, want := range cases {
		if got := substitute(tmpl, req); got != want {
			t.Errorf("substitute(%q) = %q, ожидалось %q", tmpl, got, want)
		}
	}
}

func TestTestidSelector(t *testing.T) {
	sel := testidSelector("sum_input")

	// Ради этого всё и затевалось: кабинет подписывает поля data-test-id,
	// а Playwright сам по себе ищет только data-testid.
	for _, want := range []string{
		`[data-testid="sum_input"]`,
		`[data-test-id="sum_input"]`,
		`[id="sum_input"]`,
	} {
		if !strings.Contains(sel, want) {
			t.Errorf("в селекторе нет %s:\n%s", want, sel)
		}
	}

	// Кавычки в значении не должны разваливать CSS.
	if sel := testidSelector(`a"b`); !strings.Contains(sel, `[data-testid="a\"b"]`) {
		t.Errorf("кавычка не экранирована: %s", sel)
	}
}

func TestInspectScriptSubstitutesAttrs(t *testing.T) {
	js := inspectScript()
	if strings.Contains(js, "__TESTID_ATTRS__") {
		t.Fatal("список атрибутов не подставлен в скрипт снимка")
	}
	// Снимок и шаги перевода должны искать одни и те же атрибуты, иначе
	// /inspect снова покажет то, чего шаг не найдёт.
	for _, attr := range testidAttrs {
		if !strings.Contains(js, `"`+attr+`"`) {
			t.Errorf("в скрипте снимка нет атрибута %s", attr)
		}
	}
}

func TestReadyForArmed(t *testing.T) {
	// Шагов кода нет и не должно быть: перевод себе банк проводит без SMS.
	full := &TransferSteps{
		Submit:  Step{Action: "click", Testid: "transferbyphone_input_submit"},
		Confirm: Step{Action: "click", Testid: "confirm_button"},
	}
	if err := full.ReadyForArmed(); err != nil {
		t.Errorf("описанного пути без кода оказалось мало: %v", err)
	}

	// Шаг без селектора не лучше отсутствующего: элемент по нему не найти.
	half := &TransferSteps{
		Submit:  Step{Action: "click", Testid: "transferbyphone_input_submit"},
		Confirm: Step{Action: "click"},
	}
	err := half.ReadyForArmed()
	if err == nil {
		t.Fatal("неполные шаги приняты как боевые")
	}
	if !strings.Contains(err.Error(), "confirm") {
		t.Errorf("в ошибке не назван недостающий шаг: %v", err)
	}
}

func TestMarkerStepsNeedNoAction(t *testing.T) {
	// Маркер исхода не нажимают — на него смотрят, поэтому action ему не нужен.
	marker := Step{Testid: "transferbyphone_notification_print_receipt"}
	if !marker.hasSelector() {
		t.Error("маркер с testid не признан описанным")
	}
	if marker.describesElement() {
		t.Error("маркер без action не должен считаться шагом-действием")
	}
	if (Step{Action: "click"}).hasSelector() {
		t.Error("шаг без селектора признан описанным")
	}
}

func TestNotArmedRefusesToSend(t *testing.T) {
	// Предохранитель проверяется без браузера: методы обязаны отказать до
	// того, как дотянутся до страницы.
	tr := NewTransfer(nil, &TransferSteps{
		Submit:  Step{Action: "click", Testid: "x"},
		Confirm: Step{Action: "click", Testid: "y"},
	}, false)

	if err := tr.Submit(); err != ErrNotArmed {
		t.Errorf("Submit в черновом режиме вернул %v", err)
	}
	if err := tr.Send(); err != ErrNotArmed {
		t.Errorf("Send в черновом режиме вернул %v", err)
	}
	if err := tr.EnterCode("1234"); err != ErrNotArmed {
		t.Errorf("EnterCode в черновом режиме вернул %v", err)
	}
}

func TestSameValue(t *testing.T) {
	// Поле возвращает сумму по-своему: с разрядкой, с копейками, с валютой.
	// Всё это — то же самое число, и повторять ввод не нужно.
	same := []struct{ got, want string }{
		{"5000", "5000"},
		{"5 000", "5000"},
		{"5 000,00", "5000"},   // неразрывный пробел, копейки
		{"5 000,00 ₽", "5000"}, // ещё и валюта
		{"+7 (999) 123-45-67", "+79991234567"},
		{"1234.56", "1234,56"},
	}
	for _, c := range same {
		if !sameValue(c.got, c.want) {
			t.Errorf("sameValue(%q, %q) = false, ожидалось true", c.got, c.want)
		}
	}

	// А это разные значения: перепутанный порядок цифр в сумме — ровно та
	// ошибка, которую сверка обязана поймать.
	diff := []struct{ got, want string }{
		{"50 000", "5000"},
		{"500", "5000"},
		{"", "5000"},
		{"5000", ""},
		{"неизвестно", "5000"},
	}
	for _, c := range diff {
		if sameValue(c.got, c.want) {
			t.Errorf("sameValue(%q, %q) = true, ожидалось false", c.got, c.want)
		}
	}
}

func TestParseNumeric(t *testing.T) {
	if _, ok := parseNumeric("₽"); ok {
		t.Error("строка без цифр не должна разбираться в число")
	}
	if v, ok := parseNumeric("5 000,50"); !ok || v != 5000.50 {
		t.Errorf("parseNumeric(\"5 000,50\") = %v, %v", v, ok)
	}
}

func TestOneLine(t *testing.T) {
	err := "Timeout 15000ms exceeded.\nCall log:\n  - waiting for locator"
	if got := oneLine(err); got != "Timeout 15000ms exceeded." {
		t.Errorf("oneLine = %q", got)
	}
}

func TestStepLabel(t *testing.T) {
	// Note важнее прочего: он написан человеком для человека.
	if got := stepLabel(Step{Note: "выбрать банк", Text: "Т-Банк"}); got != "выбрать банк" {
		t.Errorf("stepLabel с note = %q", got)
	}
	if got := stepLabel(Step{Text: "Перевести"}); got != "Перевести" {
		t.Errorf("stepLabel по text = %q", got)
	}
	if got := stepLabel(Step{Action: "wait"}); got != "wait" {
		t.Errorf("stepLabel без описания = %q", got)
	}
}
