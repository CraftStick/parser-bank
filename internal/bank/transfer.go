package bank

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/playwright-community/playwright-go"
)

// Перевод себе — самая ответственная часть: тут уходят реальные деньги.
//
// Сделан намеренно консервативно. Форма заполняется кликами по видимой
// странице, а не прямыми запросами к API: так владелец своими глазами видит
// в окне браузера, что именно бот ввёл, до того как подтвердит операцию кодом.
// Ошибка в маппинге JSON увела бы деньги молча; ошибка в клике по форме
// видна сразу.
//
// Поток разбит надвое. Prepare заполняет форму и доводит до экрана, где банк
// просит код, — на этом шаге ничего не отправлено. Confirm вводит код и жмёт
// финальную кнопку. Между ними человек присылает OTP из SMS.

// TransferSteps описывает, как пройти форму перевода. Вынесено в JSON, потому
// что вёрстка кабинета меняется, а деньги — не то, ради чего хочется
// пересобирать бинарник.
type TransferSteps struct {
	// URL страницы перевода (путь, хост подставляется от активного зеркала).
	Path string `json:"path"`

	// Prepare — заполнение формы. Ничего не отправляет.
	Prepare []Step `json:"prepare"`
	// Submit — кнопка «Продолжить»: форма уходит на экран сверки. Деньги на
	// этом шаге ещё не двигаются, банк только показывает, что получилось.
	Submit Step `json:"submit"`
	// Confirm — финальная кнопка на экране сверки. Необратимый шаг.
	Confirm Step `json:"confirm"`

	// OTP — поле кода и кнопка под ним. Необязательны: перевод себе банк
	// подтверждает по-разному, и код после финальной кнопки просит не всегда.
	// Если поля нет, шаги остаются пустыми, а исход читается со страницы.
	OTPField   Step `json:"otp_field"`
	OTPConfirm Step `json:"otp_confirm"`

	// Success/Failure — по чему видно исход. Надёжнее элемент: текст на экране
	// статуса банк формулирует как хочет и меняет без предупреждения, а вот
	// кнопка «Получить чек» появляется только у проведённого перевода.
	// Тексты остаются запасным вариантом.
	Success Step `json:"success"`
	Failure Step `json:"failure"`

	SuccessText string `json:"success_text"`
	FailureText string `json:"failure_text"`
}

// Step — одно действие над формой. Элемент ищется по одному из полей (в этом
// порядке): testid, role+name, label, placeholder, text. Значение value
// поддерживает подстановки {amount}, {phone}, {bank}, {comment}.
type Step struct {
	Action      string `json:"action"` // click | fill | wait
	Testid      string `json:"testid,omitempty"`
	Role        string `json:"role,omitempty"`
	Name        string `json:"name,omitempty"`
	Text        string `json:"text,omitempty"`
	Label       string `json:"label,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	Value       string `json:"value,omitempty"`
	Note        string `json:"note,omitempty"` // человекочитаемое описание шага
}

// TransferRequest — что перевести. Получатель сюда не входит: он берётся из
// конфига, из чата приходит только сумма.
type TransferRequest struct {
	Amount  float64
	Phone   string
	Bank    string
	Comment string
}

// LoadTransferSteps читает описание формы перевода.
func LoadTransferSteps(path string) (*TransferSteps, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("нет файла %s — опишите шаги перевода (см. transfer.example.json)", path)
		}
		return nil, fmt.Errorf("прочитать %s: %w", path, err)
	}

	var s TransferSteps
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("разобрать %s: %w", path, err)
	}
	if len(s.Prepare) == 0 {
		return nil, fmt.Errorf("в %s не описан ни один шаг подготовки", path)
	}
	return &s, nil
}

// ReadyForArmed проверяет, что описан весь боевой путь.
//
// В черновом режиме недостающие шаги не мешают: до них дело не доходит. А вот
// в боевом «шаг не описан» всплывёт в худший момент — на экране сверки, когда
// форма уже отправлена. Лучше отказаться на старте.
//
// Шаги кода не проверяются: банк просит его не всегда, и на переводе себе
// экран подтверждения обходится одной кнопкой.
func (s *TransferSteps) ReadyForArmed() error {
	var missing []string
	for _, c := range []struct {
		name string
		step Step
	}{
		{"submit (кнопка «Продолжить»)", s.Submit},
		{"confirm (финальная кнопка)", s.Confirm},
	} {
		if !c.step.describesElement() {
			missing = append(missing, c.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("не описаны шаги: %s", strings.Join(missing, ", "))
	}
	return nil
}

// hasSelector — описан ли хоть один способ найти элемент. Маркерам исхода
// этого достаточно: их не нажимают, на них смотрят.
func (s Step) hasSelector() bool {
	return s.Testid != "" || s.Role != "" || s.Label != "" || s.Placeholder != "" || s.Text != ""
}

// describesElement — есть ли в шаге действие и хоть один способ найти элемент.
func (s Step) describesElement() bool {
	return s.Action != "" && s.hasSelector()
}

// Transfer ведёт перевод через форму банка.
type Transfer struct {
	br    *Browser
	steps *TransferSteps

	// armed — предохранитель. Хранится здесь, а не передаётся в каждый метод:
	// флаг, который надо не забыть протащить, рано или поздно забывают.
	armed bool
}

func NewTransfer(br *Browser, steps *TransferSteps, armed bool) *Transfer {
	return &Transfer{br: br, steps: steps, armed: armed}
}

// ErrNotArmed — попытка что-то отправить в черновом режиме.
var ErrNotArmed = fmt.Errorf("черновой режим: отправка выключена")

// Prepare заполняет форму. Деньги на этом шаге не уходят, ничего не
// отправляется. Возвращает ошибку, если какой-то элемент не найден.
func (t *Transfer) Prepare(req TransferRequest) error {
	if err := t.br.Open(t.steps.Path); err != nil {
		return err
	}

	for i, step := range t.steps.Prepare {
		if err := t.run(step, req); err != nil {
			return fmt.Errorf("шаг %d (%s): %w", i+1, stepLabel(step), err)
		}
	}
	return nil
}

// Submit жмёт «Продолжить»: форма уходит на экран сверки. Деньги ещё не
// двигаются — банк только показывает, что получилось, и ждёт финального
// нажатия. Отсюда и до Send есть время передумать.
func (t *Transfer) Submit() error {
	if !t.armed {
		return ErrNotArmed
	}
	return t.run(t.steps.Submit, TransferRequest{})
}

// Send жмёт финальную кнопку на экране сверки. Точка невозврата: после неё
// деньги ушли (или банк спросит код, если для этого перевода он нужен).
func (t *Transfer) Send() error {
	if !t.armed {
		return ErrNotArmed
	}
	return t.run(t.steps.Confirm, TransferRequest{})
}

// CodeRequested говорит, появилось ли после Send поле для кода.
//
// Перевод себе банк часто проводит без SMS — на экране сверки одна кнопка и
// больше ничего. Поэтому код не выпрашивается вслепую: сначала смотрим, есть
// ли куда его вводить. Если шаг otp_field не описан, значит кода и не ждут.
func (t *Transfer) CodeRequested(within time.Duration) bool {
	if !t.steps.OTPField.describesElement() {
		return false
	}
	loc, err := t.locate(t.steps.OTPField)
	if err != nil {
		return false
	}
	return loc.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateAttached,
		Timeout: playwright.Float(float64(within.Milliseconds())),
	}) == nil
}

// EnterCode вводит код из SMS и жмёт кнопку под полем, если она описана.
func (t *Transfer) EnterCode(code string) error {
	if !t.armed {
		return ErrNotArmed
	}
	if err := t.run(t.steps.OTPField.withValue(code), TransferRequest{}); err != nil {
		return fmt.Errorf("ввести код: %w", err)
	}
	// Кнопки может не быть: некоторые формы отправляются сами, как только
	// набран последний символ кода.
	if t.steps.OTPConfirm.describesElement() {
		if err := t.run(t.steps.OTPConfirm, TransferRequest{}); err != nil {
			return fmt.Errorf("подтвердить: %w", err)
		}
	}
	return nil
}

// Outcome ждёт на странице маркер успеха или ошибки.
func (t *Transfer) Outcome() (bool, error) { return t.waitOutcome() }

// CheckSubmit проверяет, что кнопка «Продолжить» на месте, — не нажимая её.
//
// В черновом режиме до неё дело не доходит, и её селектор остался бы
// непроверенным до первого боевого перевода: сумма введена, деньги наготове —
// и выясняется, что кнопку не найти. Здесь то же самое выясняется бесплатно.
func (t *Transfer) CheckSubmit() error {
	loc, err := t.locate(t.steps.Submit)
	if err != nil {
		return err
	}

	if err := loc.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateAttached,
		Timeout: playwright.Float(5000),
	}); err != nil {
		return fmt.Errorf("кнопка «%s» не найдена: %w%s",
			stepLabel(t.steps.Submit), err, t.diagnose(t.steps.Submit))
	}

	// Кнопка бывает выключена, пока форма не заполнена целиком. В черновом
	// режиме это единственный признак, что банк чем-то недоволен.
	if enabled, e := loc.IsEnabled(playwright.LocatorIsEnabledOptions{
		Timeout: playwright.Float(5000),
	}); e == nil && !enabled {
		return fmt.Errorf("кнопка «%s» найдена, но неактивна — форма заполнена не до конца",
			stepLabel(t.steps.Submit))
	}
	return nil
}

// waitOutcome ждёт на странице маркер успеха или ошибки.
//
// Сначала проверяется признак неудачи: экран статуса банк показывает в обоих
// случаях, и принять отказ за успех хуже, чем наоборот.
func (t *Transfer) waitOutcome() (bool, error) {
	deadline := time.Now().Add(30 * time.Second)

	for {
		if t.markerVisible(t.steps.Failure, t.steps.FailureText) {
			return false, fmt.Errorf("банк отклонил перевод")
		}
		if t.markerVisible(t.steps.Success, t.steps.SuccessText) {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, fmt.Errorf("банк не показал результат за 30с — проверьте окно браузера")
		}
		time.Sleep(time.Second)
	}
}

// markerVisible — виден ли на странице признак исхода: описанный элемент или,
// если элемент не задан, текст.
func (t *Transfer) markerVisible(marker Step, text string) bool {
	if marker.hasSelector() {
		if loc, err := t.locate(marker); err == nil {
			if n, err := loc.Count(); err == nil && n > 0 {
				return true
			}
		}
	}
	if text != "" {
		if ok, _ := t.br.Page().GetByText(text).First().IsVisible(); ok {
			return true
		}
	}
	return false
}

// run выполняет один шаг.
func (t *Transfer) run(step Step, req TransferRequest) error {
	if step.Action == "wait" {
		d := 2 * time.Second
		if step.Value != "" {
			if parsed, err := time.ParseDuration(step.Value); err == nil {
				d = parsed
			}
		}
		time.Sleep(d)
		return nil
	}

	loc, err := t.locate(step)
	if err != nil {
		return err
	}

	// Ждём появления в DOM, а не «видимости». Денежные поля банка — кастомные:
	// настоящий input скрыт (visibility:hidden), поверх него нарисован div.
	// Требование visible на таком поле не выполняется никогда, хотя ввод
	// работает.
	if err := loc.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateAttached,
		Timeout: playwright.Float(20000),
	}); err != nil {
		return fmt.Errorf("элемент не появился: %w%s", err, t.diagnose(step))
	}

	switch step.Action {
	case "click":
		return loc.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(15000)})
	case "fill":
		return t.fill(loc, substitute(step.Value, req))
	default:
		return fmt.Errorf("неизвестное действие %q", step.Action)
	}
}

// fill заполняет поле, устойчиво к кастомным компонентам.
//
// Денежное поле кабинета — не обычный input: настоящий input скрыт
// (visibility:hidden), а видимую часть рисует div поверх. Поэтому три попытки,
// от самой «честной» к самой грубой:
//
//  1. Fill с Force — обычный путь, снято только требование видимости;
//  2. фокус + настоящие нажатия клавиш — клавиатура шлёт события в
//     сфокусированный элемент и про видимость не спрашивает вовсе;
//  3. нативный сеттер value + input/change — последний рубеж для полей,
//     которые не реагируют даже на клавиатуру.
//
// После каждой попытки поле перечитывается: банк любит молча проглотить ввод,
// и «ошибки не было» тут недостаточно.
func (t *Transfer) fill(loc playwright.Locator, value string) error {
	var failures []string

	attempts := []struct {
		name string
		do   func() error
	}{
		{"fill", func() error {
			return loc.Fill(value, playwright.LocatorFillOptions{
				Force:   playwright.Bool(true),
				Timeout: playwright.Float(10000),
			})
		}},
		{"клавиатура", func() error { return t.typeInto(loc, value) }},
		{"нативный сеттер", func() error { return setValueByJS(loc, value) }},
	}

	for _, a := range attempts {
		if err := a.do(); err != nil {
			failures = append(failures, a.name+": "+oneLine(err.Error()))
			continue
		}
		if fieldAccepted(loc, value) {
			return nil
		}
		failures = append(failures, a.name+": поле осталось пустым или с другим значением")
	}

	return fmt.Errorf("поле не приняло %q (%s)", value, strings.Join(failures, "; "))
}

// typeInto набирает значение с клавиатуры.
//
// Focus ждёт только присутствия элемента в DOM, а keyboard шлёт события туда,
// где фокус, — связка обходит проверку видимости целиком. Перед вводом поле
// очищается выделением, иначе повторная попытка допишет к остатку прошлой.
func (t *Transfer) typeInto(loc playwright.Locator, value string) error {
	if err := loc.Focus(playwright.LocatorFocusOptions{
		Timeout: playwright.Float(10000),
	}); err != nil {
		return err
	}

	kb := t.br.Page().Keyboard()
	if err := kb.Press("ControlOrMeta+a"); err == nil {
		_ = kb.Press("Delete")
	}

	// Задержка между нажатиями: и кастомные маски успевают пересчитаться, и
	// антифроду такой темп привычнее мгновенной вставки.
	return kb.Type(value, playwright.KeyboardTypeOptions{Delay: playwright.Float(60)})
}

// setValueByJS пишет значение напрямую и сам шлёт события, которые обычно
// рождает браузер.
//
// Присваивание el.value в обход нативного сеттера React не замечает — его
// перехватчик висит именно на сеттере прототипа. Поэтому значение ставится
// через него, а следом идут input и change: без них форма считает поле пустым,
// даже когда текст в нём виден.
func setValueByJS(loc playwright.Locator, value string) error {
	_, err := loc.Evaluate(`(el, value) => {
		if (el.isContentEditable) {
			el.textContent = value;
		} else {
			const proto = el instanceof HTMLTextAreaElement
				? HTMLTextAreaElement.prototype
				: HTMLInputElement.prototype;
			const setter = Object.getOwnPropertyDescriptor(proto, 'value').set;
			setter.call(el, value);
		}
		el.dispatchEvent(new Event('input', { bubbles: true }));
		el.dispatchEvent(new Event('change', { bubbles: true }));
	}`, value, playwright.LocatorEvaluateOptions{Timeout: playwright.Float(10000)})
	return err
}

// fieldAccepted перечитывает поле и говорит, есть ли там нужное значение.
//
// Сравнение нестрогое: денежное поле возвращает сумму по-своему — «5 000,00»
// вместо «5000», телефон приходит в маске. Сверяются числа, а не строки.
// Если значение прочитать нельзя (элемент вообще не input), верим действию:
// поднимать тревогу не на чем.
func fieldAccepted(loc playwright.Locator, want string) bool {
	got, err := loc.InputValue(playwright.LocatorInputValueOptions{
		Timeout: playwright.Float(5000),
	})
	if err != nil {
		return true
	}
	return sameValue(got, want)
}

func sameValue(got, want string) bool {
	if got == want {
		return true
	}
	if got == "" || want == "" {
		return false
	}
	if g, ok := parseNumeric(got); ok {
		if w, ok := parseNumeric(want); ok {
			return math.Abs(g-w) < 0.005
		}
	}
	return false
}

// parseNumeric достаёт число из того, что показывает поле: убирает пробелы
// (в том числе неразрывные), знак валюты и прочее оформление, а запятую
// приводит к точке.
func parseNumeric(s string) (float64, bool) {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ',' || r == '.':
			b.WriteRune('.')
		}
	}
	// Хвост вида «5000.» или пустая строка числом не считается.
	v, err := strconv.ParseFloat(strings.TrimSuffix(b.String(), "."), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// oneLine схлопывает многострочные ошибки Playwright: в сообщении об ошибке
// шага важна суть, а не весь его лог ожидания.
func oneLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// locate находит элемент по описанию шага.
func (t *Transfer) locate(step Step) (playwright.Locator, error) {
	page := t.br.Page()

	switch {
	case step.Testid != "":
		// Самый устойчивый якорь: тестовый идентификатор не зависит ни от
		// текста, ни от вёрстки, и переживает косметические правки формы.
		//
		// Ищем своим селектором, а не GetByTestId: тот знает единственное
		// написание атрибута (data-testid), а кабинет подписывает элементы
		// иначе — и поле «не находилось», хотя лежало на странице.
		return page.Locator(testidSelector(step.Testid)).First(), nil
	case step.Role != "":
		opts := playwright.PageGetByRoleOptions{}
		if step.Name != "" {
			opts.Name = step.Name
		}
		return page.GetByRole(playwright.AriaRole(step.Role), opts).First(), nil
	case step.Label != "":
		return page.GetByLabel(step.Label).First(), nil
	case step.Placeholder != "":
		return page.GetByPlaceholder(step.Placeholder).First(), nil
	case step.Text != "":
		return page.GetByText(step.Text).First(), nil
	default:
		return nil, fmt.Errorf("шаг не указывает, какой элемент искать")
	}
}

// probeTestidScript считает, сколько элементов с таким идентификатором есть в
// документе и под каким атрибутом.
const probeTestidScript = `(arg) => {
	const found = [];
	for (const a of arg.attrs) {
		const n = document.querySelectorAll('[' + a + '=' + JSON.stringify(arg.id) + ']').length;
		if (n) found.push(a + ' ×' + n);
	}
	if (document.getElementById(arg.id)) found.push('id');
	return found.join(', ');
}`

// diagnose объясняет, почему шаг не нашёл элемент.
//
// Разница между «элемента нет» и «элемент есть, но ожидание не прошло» —
// это разница между правкой transfer.json и правкой кода. Ответ на неё
// снимается за один запрос к странице, поэтому берём его сразу, а не
// следующим прогоном вслепую. Заодно проверяются фреймы: элемент во фрейме
// не виден ни ожиданию, ни инспектору.
func (t *Transfer) diagnose(step Step) string {
	if step.Testid == "" {
		return ""
	}
	page := t.br.Page()
	arg := map[string]any{"id": step.Testid, "attrs": testidAttrs}

	var notes []string
	for _, fr := range page.Frames() {
		res, err := fr.Evaluate(probeTestidScript, arg)
		if err != nil {
			continue
		}
		hits, _ := res.(string)
		if hits == "" {
			continue
		}
		where := "на странице"
		if fr != page.MainFrame() {
			where = "во фрейме " + fr.URL()
		}
		notes = append(notes, where+": "+hits)
	}

	if len(notes) == 0 {
		return fmt.Sprintf("\nэлемента %q нет ни под одним из атрибутов (%s) — проверьте /inspect",
			step.Testid, strings.Join(testidAttrs, ", "))
	}
	return "\nэлемент на месте (" + strings.Join(notes, "; ") + ") — значит дело не в поиске, а в ожидании"
}

// testidSelector собирает CSS по всем написаниям тестового атрибута плюс id:
// в transfer.json пишется просто "sum_input", а каким именно атрибутом он
// подписан — забота кода, а не человека, правящего конфиг.
func testidSelector(id string) string {
	quoted := strconv.Quote(id)
	parts := make([]string, 0, len(testidAttrs)+1)
	for _, attr := range testidAttrs {
		parts = append(parts, fmt.Sprintf("[%s=%s]", attr, quoted))
	}
	parts = append(parts, fmt.Sprintf("[id=%s]", quoted))
	return strings.Join(parts, ", ")
}

func (s Step) withValue(v string) Step {
	s.Value = v
	return s
}

func stepLabel(s Step) string {
	if s.Note != "" {
		return s.Note
	}
	for _, v := range []string{s.Name, s.Text, s.Label, s.Placeholder, s.Testid} {
		if v != "" {
			return v
		}
	}
	return s.Action
}

func substitute(tmpl string, req TransferRequest) string {
	r := strings.NewReplacer(
		"{amount}", trimAmount(req.Amount),
		"{phone}", req.Phone,
		"{bank}", req.Bank,
		"{comment}", req.Comment,
	)
	return r.Replace(tmpl)
}

// trimAmount печатает сумму без лишних нулей: 5000, а не 5000.00 — форма
// обычно ждёт именно так.
func trimAmount(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	return s
}
