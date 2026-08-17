package bank

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/CraftStick/parser-bank/internal/config"
	"github.com/playwright-community/playwright-go"
)

// Browser — обёртка над persistent-контекстом Chromium.
//
// Профиль живёт на диске (PROFILE_DIR) и переживает перезапуски бота: именно
// в нём лежат куки сессии и всё, что банк записал про устройство. Поэтому
// профиль нельзя пересоздавать на каждый запуск — для антифрода это будет
// выглядеть как вход с нового устройства.
type Browser struct {
	pw   *playwright.Playwright
	ctx  playwright.BrowserContext
	page playwright.Page
	cfg  *config.Config

	// Активное зеркало. Меняется на ходу, когда текущий домен умирает,
	// поэтому под мьютексом: его читает поллер, а пишет MirrorFinder.
	mu   sync.Mutex
	host string
}

// browserArgs собирает флаги запуска Chromium.
//
// Общий принцип: трогаем только то, что не меняет отпечаток браузера. Банк
// смотрит на окружение, и «оптимизации» вроде отключённых картинок или
// подменённого user-agent обходятся дороже сэкономленной памяти.
func browserArgs(cfg *config.Config) []string {
	args := []string{
		// Убирает navigator.webdriver. Без этого фронт банка местами
		// ведёт себя иначе и ломается вёрстка личного кабинета.
		"--disable-blink-features=AutomationControlled",

		// Chromium держит разделяемую память в /dev/shm, а на VPS и в
		// контейнерах это обычно 64 МБ. Кабинет банка их выбирает, и вкладка
		// падает с «Target crashed» — причём вместе с сессией, которую потом
		// восстанавливать руками. С этим флагом Chromium использует обычный
		// диск: медленнее, но не умирает. Локально безвредно.
		"--disable-dev-shm-usage",
	}

	if cfg.Runtime != config.RuntimeVPS {
		return args
	}

	// На сервере память дороже отзывчивости: одна вкладка банка всё равно
	// одна, а лишние процессы рендерера стоят по сотне мегабайт.
	return append(args,
		"--renderer-process-limit=1",
		"--disable-extensions",
		"--disable-component-extensions-with-background-pages",
		// Фоновые сервисы браузера на сервере не нужны никому: обновления
		// компонентов, синхронизация, отчёты о сбоях.
		"--disable-background-networking",
		"--disable-sync",
		"--mute-audio",
	)
}

// Launch поднимает браузер с постоянным профилем.
func Launch(cfg *config.Config) (*Browser, error) {
	if err := os.MkdirAll(cfg.ProfileDir, 0o700); err != nil {
		return nil, fmt.Errorf("создать каталог профиля: %w", err)
	}

	install := &playwright.RunOptions{
		Browsers: []string{"chromium"},
		Verbose:  false,
	}
	// С системным браузером качать нечего: он уже стоит. Это ещё и обходит
	// CDN Playwright, который сейчас отдаёт ошибку на загрузку сборок.
	if cfg.BrowserChannel != "" {
		install.SkipInstallBrowsers = true
	}
	if err := playwright.Install(install); err != nil {
		return nil, fmt.Errorf("установить драйвер playwright: %w", err)
	}

	pw, err := playwright.Run()
	if err != nil {
		return nil, fmt.Errorf("запустить playwright: %w", err)
	}

	opts := playwright.BrowserTypeLaunchPersistentContextOptions{
		Headless:   playwright.Bool(cfg.Headless),
		Locale:     playwright.String(cfg.Locale),
		TimezoneId: playwright.String(cfg.Timezone),
		Viewport:   &playwright.Size{Width: 1440, Height: 900},
		Args:       browserArgs(cfg),
	}
	if cfg.BrowserChannel != "" {
		opts.Channel = playwright.String(cfg.BrowserChannel)
	}
	if cfg.Proxy != "" {
		opts.Proxy = &playwright.Proxy{Server: cfg.Proxy}
	}

	ctx, err := pw.Chromium.LaunchPersistentContext(cfg.ProfileDir, opts)
	if err != nil {
		pw.Stop()
		return nil, fmt.Errorf("открыть профиль %s: %w", cfg.ProfileDir, err)
	}

	// Persistent-контекст обычно стартует с одной пустой вкладкой.
	var page playwright.Page
	if pages := ctx.Pages(); len(pages) > 0 {
		page = pages[0]
	} else if page, err = ctx.NewPage(); err != nil {
		ctx.Close()
		pw.Stop()
		return nil, fmt.Errorf("открыть вкладку: %w", err)
	}

	return &Browser{pw: pw, ctx: ctx, page: page, cfg: cfg, host: cfg.BankHost}, nil
}

// Page — живая вкладка банка.
//
// Возвращает не ту вкладку, что была при старте, а актуальную: пока человек
// ходит по кабинету, SPA открывает и закрывает вкладки, и зафиксированная
// ссылка быстро протухает — обращение к ней падает с «target closed».
// Поэтому каждый раз выбираем последнюю открытую вкладку на домене банка.
func (b *Browser) Page() playwright.Page {
	host := b.Host()
	pages := b.ctx.Pages()

	// С конца: свежая вкладка — это та, где человек сейчас работает.
	for i := len(pages) - 1; i >= 0; i-- {
		p := pages[i]
		if p.IsClosed() {
			continue
		}
		if host == "" || strings.Contains(p.URL(), host) {
			return p
		}
	}
	// Ни одной вкладки банка — берём любую живую.
	for i := len(pages) - 1; i >= 0; i-- {
		if !pages[i].IsClosed() {
			return pages[i]
		}
	}
	return b.page
}

// Context — контекст браузера.
func (b *Browser) Context() playwright.BrowserContext { return b.ctx }

// Host — активное зеркало. Пусто, пока оно не выбрано (режим auto).
func (b *Browser) Host() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.host
}

// SetHost переключает активное зеркало.
//
// Важно: куки сессии привязаны к домену, поэтому после переключения вход в
// банк придётся делать заново — старая сессия на новый домен не переедет.
func (b *Browser) SetHost(host string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.host = host
}

// BaseURL — корень активного зеркала.
func (b *Browser) BaseURL() string { return "https://" + b.Host() }

// Open переходит на страницу личного кабинета.
func (b *Browser) Open(path string) error {
	if b.Host() == "" {
		return fmt.Errorf("зеркало ещё не выбрано")
	}
	url := b.BaseURL() + path
	_, err := b.Page().Goto(url, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(60000),
	})
	if err != nil {
		return fmt.Errorf("перейти на %s: %w", url, err)
	}
	return nil
}

// readStorage ищет ключ во всех открытых вкладках банка.
//
// Смотреть только в первую вкладку недостаточно: вход через SSO может увести
// пользователя в соседнюю, а Chrome при старте с готовым профилем
// восстанавливает прежние вкладки, и «первая» оказывается не той, где человек
// работает. Хранилище привязано к вкладке, поэтому токен нужно искать по всем.
func (b *Browser) readStorage(store, key string) (string, error) {
	pages := b.ctx.Pages()
	if len(pages) == 0 {
		return "", fmt.Errorf("нет ни одной вкладки")
	}

	host := b.Host()
	var lastErr error

	for _, p := range pages {
		if host != "" && !strings.Contains(p.URL(), host) {
			continue
		}

		v, err := p.Evaluate(
			fmt.Sprintf(`k => { try { return window.%s.getItem(k); } catch (e) { return null; } }`, store),
			key)
		if err != nil {
			lastErr = fmt.Errorf("прочитать %s[%s]: %w", store, key, err)
			continue
		}
		if s, okStr := v.(string); okStr && s != "" {
			return s, nil
		}
	}

	// Ошибку возвращаем, только если ни одна вкладка не ответила: иначе это
	// обычное «ещё не вошли», а не поломка.
	if lastErr != nil {
		return "", lastErr
	}
	return "", nil
}

// TabPaths — пути открытых вкладок банка.
//
// Только путь, без query: в адресах кабинета попадаются параметры сессии,
// а строка уходит в лог, который потом прикладывают к issue.
func (b *Browser) TabPaths() []string {
	host := b.Host()
	var out []string

	for _, p := range b.ctx.Pages() {
		raw := p.URL()
		if host != "" && !strings.Contains(raw, host) {
			continue
		}
		if u, err := url.Parse(raw); err == nil {
			out = append(out, u.Path)
		}
	}
	return out
}

// PageSnapshot — видимые поля и кнопки текущей страницы. Значения полей не
// снимаются, только то, как элемент назван.
type PageSnapshot struct {
	URL     string       `json:"url"`
	Inputs  []InputField `json:"inputs"`
	Buttons []ButtonInfo `json:"buttons"`
}

// testidAttrs — все написания тестового атрибута, которые встречаются в вёрстке
// кабинета и вообще в природе.
//
// Playwright знает ровно одно — data-testid. ВТБ подписывает элементы иначе,
// поэтому GetByTestId не находил поле, которое инспектор честно показывал на
// странице. Один список на пакет: по нему и снимается снимок, и собирается
// селектор в шагах перевода — иначе они снова разъедутся.
var testidAttrs = []string{
	"data-testid",
	"data-test-id",
	"data-test",
	"data-qa",
	"data-qa-id",
	"testid",
}

type InputField struct {
	Type        string `json:"type"`
	Placeholder string `json:"placeholder"`
	Name        string `json:"name"`
	Aria        string `json:"aria"`
	Testid      string `json:"testid"`
	Filled      bool   `json:"filled"`

	// TestidAttr — каким именно атрибутом подписан элемент. Показывается
	// рядом с самим идентификатором: без этого непонятно, почему поиск по
	// «тому же» testid не срабатывает.
	TestidAttr string `json:"testid_attr"`

	// Hidden — поле есть в DOM, но Playwright не считает его видимым.
	// Такие поля показываются отдельно: денежные и кодовые поля кабинета
	// часто именно такие — настоящий input спрятан, поверх нарисован div.
	// Раньше они молча выпадали из снимка, и было непонятно, почему шаг
	// падает по таймауту на поле, которого «нет».
	Hidden bool `json:"hidden"`
}

type ButtonInfo struct {
	Text       string `json:"text"`
	Testid     string `json:"testid"`
	TestidAttr string `json:"testid_attr"`
}

// inspectTemplate — снимок страницы. __TESTID_ATTRS__ подставляется из
// testidAttrs, чтобы список жил в одном месте.
const inspectTemplate = `() => {
	const TESTID_ATTRS = __TESTID_ATTRS__;
	const testid = el => {
		for (const a of TESTID_ATTRS) {
			const v = el.getAttribute(a);
			if (v) return { id: v, attr: a };
		}
		return { id: '', attr: '' };
	};

	// Видимость считается так же, как её понимает Playwright: непустая рамка
	// и visibility, отличная от hidden. Размера тут мало — у денежного поля
	// кабинета рамка есть, а visibility:hidden, и шаг падает по таймауту,
	// хотя снимок бодро показывал поле как обычное.
	const vis = el => {
		const r = el.getBoundingClientRect();
		if (r.width <= 0 || r.height <= 0) return false;
		return getComputedStyle(el).visibility !== 'hidden';
	};
	const out = { url: location.pathname, inputs: [], buttons: [] };

	document.querySelectorAll('input, textarea, [contenteditable=true]').forEach(el => {
		// type=hidden — служебные носители данных, их на странице десятки.
		if (el.getAttribute('type') === 'hidden') return;

		const tid = testid(el);
		const field = {
			type: el.getAttribute('type') || el.tagName.toLowerCase(),
			placeholder: el.getAttribute('placeholder') || '',
			name: el.getAttribute('name') || '',
			aria: el.getAttribute('aria-label') || '',
			testid: tid.id,
			testid_attr: tid.attr,
			filled: el.value ? true : false,
			hidden: !vis(el)
		};
		// Невидимое поле попадает в снимок, только если по нему есть за что
		// зацепиться: безымянные скрытые input'ы — шум, а не подсказка.
		if (field.hidden && !field.testid && !field.name && !field.placeholder && !field.aria) return;
		out.inputs.push(field);
	});

	document.querySelectorAll('button, [role=button], a[href]').forEach(el => {
		if (!vis(el)) return;
		const t = (el.innerText || el.textContent || '').trim().replace(/\s+/g, ' ');
		if (!t || t.length > 50) return;
		const tid = testid(el);
		out.buttons.push({ text: t, testid: tid.id, testid_attr: tid.attr });
	});

	return JSON.stringify(out);
}`

// inspectScript подставляет в шаблон актуальный список атрибутов.
func inspectScript() string {
	list, _ := json.Marshal(testidAttrs)
	return strings.Replace(inspectTemplate, "__TESTID_ATTRS__", string(list), 1)
}

// Inspect снимает поля и кнопки текущей страницы.
//
// Работает по активной вкладке того же браузера, в котором сделан вход, —
// поэтому и видит залогиненную форму. Отдельным процессом это не снять: вход
// живёт только внутри одного запуска.
func (b *Browser) Inspect() (PageSnapshot, error) {
	var snap PageSnapshot

	raw, err := b.Page().Evaluate(inspectScript())
	if err != nil {
		return snap, fmt.Errorf("прочитать страницу: %w", err)
	}
	text, _ := raw.(string)
	if err := json.Unmarshal([]byte(text), &snap); err != nil {
		return snap, fmt.Errorf("разобрать снимок страницы: %w", err)
	}
	return snap, nil
}

// Close аккуратно гасит браузер и драйвер.
func (b *Browser) Close() {
	if b.ctx != nil {
		b.ctx.Close()
	}
	if b.pw != nil {
		b.pw.Stop()
	}
}
