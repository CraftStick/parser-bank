// Команда doctor проверяет, что окружение готово, не заходя в банк.
//
// Полезна тем, что отделяет проблемы настройки от проблем самого парсинга:
// если doctor зелёный, а бот не работает — дело в endpoints.json или в сессии,
// а не в браузере и не в сети.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/CraftStick/parser-bank/internal/bank"
	"github.com/CraftStick/parser-bank/internal/config"
	"github.com/CraftStick/parser-bank/internal/discover"
)

// status различает поломку и просто невыполненный шаг. Разница принципиальная:
// отсутствие endpoints.json до первого make record — это нормальный ход дела,
// и ронять на нём сборку означало бы приучать игнорировать красный вывод.
type status int

const (
	ok status = iota
	pending
	broken
)

type report struct {
	broken  int
	pending int
}

func (r *report) add(s status, name, detail string) {
	mark := "✓"
	switch s {
	case pending:
		mark = "·"
		r.pending++
	case broken:
		mark = "✗"
		r.broken++
	}
	fmt.Printf("  %s %-22s %s\n", mark, name, detail)
}

// need превращает условие в «работает / сломано».
func need(cond bool) status {
	if cond {
		return ok
	}
	return broken
}

// step превращает условие в «сделано / ещё предстоит».
func step(cond bool) status {
	if cond {
		return ok
	}
	return pending
}

func main() {
	log.SetFlags(0)

	r := run()
	fmt.Println()

	switch {
	case r.broken > 0:
		fmt.Printf("Сломано: %d — см. пометки ✗\n", r.broken)
		os.Exit(1)
	case r.pending > 0:
		fmt.Printf("Окружение в порядке. Осталось шагов: %d (пометки ·)\n", r.pending)
		fmt.Println("Дальше: make record")
	default:
		fmt.Println("Всё готово. Дальше: make bot")
	}
}

func run() *report {
	r := &report{}

	cfg, err := config.Load()
	if err != nil {
		fmt.Println("Конфигурация")
		r.add(broken, "конфиг", err.Error())
		return r
	}

	fmt.Println("Конфигурация")
	r.add(need(cfg.TGToken != ""), "TG_BOT_TOKEN", mask(cfg.TGToken))
	r.add(need(cfg.OwnerID != 0), "TG_OWNER_ID", fmt.Sprint(cfg.OwnerID))
	r.add(ok, "режим", string(cfg.Runtime))

	host := cfg.BankHost
	if cfg.AutoMirror {
		host = "auto"
	}
	r.add(ok, "банк", host)

	browser := cfg.BrowserChannel
	if browser == "" {
		browser = "chromium (встроенный)"
	}
	r.add(ok, "браузер", browser)

	fmt.Println("\nКонфиг ручек")
	endpoints, err := bank.LoadEndpoints(cfg.EndpointsPath)
	if err != nil {
		r.add(pending, cfg.EndpointsPath, "появится после make record")
	} else {
		r.add(step(endpoints.Accounts != nil), "accounts", describe(endpoints.Accounts != nil))
		r.add(step(endpoints.Transactions != nil), "transactions", describe(endpoints.Transactions != nil))
	}

	fmt.Println("\nДампы discovery")
	res, err := discover.Analyze(cfg.RecordDir)
	if err != nil {
		r.add(pending, "дампы", "каталога нет — снимутся при make record")
	} else {
		r.add(ok, "разобрано файлов", fmt.Sprint(res.Files))
		r.add(step(res.Transactions != nil), "операции найдены", describe(res.Transactions != nil))
		r.add(step(res.Accounts != nil), "счета найдены", describe(res.Accounts != nil))
	}

	fmt.Println("\nСеть")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	candidates := cfg.Mirrors
	if len(candidates) == 0 {
		candidates = bank.DefaultMirrors
	}
	alive := bank.Resolvable(ctx, candidates)
	r.add(need(len(alive) > 0), "зеркала резолвятся",
		fmt.Sprintf("%d из %d: %v", len(alive), len(candidates), alive))

	fmt.Println("\nБраузер")
	br, err := bank.Launch(cfg)
	if err != nil {
		r.add(broken, "запуск", err.Error())
		hintLaunchFailure()
		return r
	}
	defer br.Close()

	r.add(ok, "запуск", "ок")

	ua, err := br.Page().Evaluate("() => navigator.userAgent")
	if err != nil {
		r.add(broken, "user-agent", err.Error())
	} else {
		r.add(ok, "user-agent", short(fmt.Sprint(ua), 62))
	}

	if endpoints != nil {
		// Свой контекст: у проверки сети таймаут короткий, а восстановление
		// сессии заведомо дольше.
		sessCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		checkSession(sessCtx, r, cfg, br, endpoints)
	}

	return r
}

// checkSession выясняет, переживает ли вход перезапуск браузера.
//
// Это главный вопрос всей затеи: токен ВТБ лежит в sessionStorage и умирает
// вместе с окном. Если сессия не восстанавливается по кукам, заходить руками
// придётся при каждом старте бота — и лучше узнать это здесь, а не по молчанию
// бота в три часа ночи.
func checkSession(ctx context.Context, r *report, cfg *config.Config, br *bank.Browser, e *bank.Endpoints) {
	fmt.Println("\nСессия банка")

	if cfg.AutoMirror || br.Host() == "" {
		host, err := bank.NewMirrorFinder(br, cfg.Mirrors).Find(ctx)
		if err != nil {
			r.add(broken, "зеркало", err.Error())
			return
		}
		br.SetHost(host)
	}

	client := bank.NewClient(br, e)
	session := bank.NewSession(br, client)

	start := time.Now()
	err := session.Ensure(ctx, 45*time.Second)
	took := time.Since(start).Round(time.Second)

	switch {
	case err == nil:
		r.add(ok, "вход из профиля", fmt.Sprintf("восстановлен за %s", took))
	case bank.IsAuthError(err):
		r.add(pending, "вход из профиля", "не восстановился — нужен /login в боте")
	default:
		r.add(broken, "вход из профиля", err.Error())
	}
}

// hintLaunchFailure подсказывает самое частое на маке: macOS блокирует запуск
// чужого приложения процессом, у которого нет на это разрешения.
func hintLaunchFailure() {
	fmt.Println()
	fmt.Println("  Если macOS показала «Приложение заблокировано при попытке внести")
	fmt.Println("  изменения в приложения на Вашем Mac» — выдайте разрешение тому,")
	fmt.Println("  откуда запускаете: Системные настройки → Конфиденциальность")
	fmt.Println("  и безопасность → Управление приложениями.")
	fmt.Println("  Либо запустите из Terminal.app вместо встроенного терминала IDE.")
}

func describe(v bool) string {
	if v {
		return "есть"
	}
	return "нет"
}

// mask показывает, что токен задан, не печатая его целиком: вывод doctor-а
// люди прикладывают к issue.
func mask(s string) string {
	if s == "" {
		return "не задан"
	}
	if len(s) <= 8 {
		return "задан"
	}
	return "задан (" + s[:4] + "…" + s[len(s)-2:] + ")"
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
