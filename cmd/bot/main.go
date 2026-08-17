// Команда bot — основной процесс: держит браузер с сессией банка, опрашивает
// операции и общается с владельцем в Telegram.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/valerakrut/parserbank/internal/bank"
	"github.com/valerakrut/parserbank/internal/config"
	"github.com/valerakrut/parserbank/internal/poller"
	"github.com/valerakrut/parserbank/internal/store"
	"github.com/valerakrut/parserbank/internal/tg"
)

// Сколько ошибок подряд терпим, прежде чем заподозрить, что зеркало умерло.
const errorsBeforeMirrorSwitch = 3

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[parserbank] ")

	if err := run(); err != nil {
		log.Fatalf("остановлен: %v", err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.RequireBot(); err != nil {
		return err
	}

	endpoints, err := bank.LoadEndpoints(cfg.EndpointsPath)
	if err != nil {
		return err
	}

	st, err := store.Open(cfg.DBPath, cfg.HistoryLimit)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("режим %s, опрос раз в %s", cfg.Runtime, cfg.PollInterval)

	br, err := bank.Launch(cfg)
	if err != nil {
		return err
	}
	defer br.Close()

	mirrors := bank.NewMirrorFinder(br, cfg.Mirrors)

	if cfg.AutoMirror {
		log.Print("подбираю живое зеркало")
		host, err := mirrors.Find(ctx)
		if err != nil {
			return err
		}
		br.SetHost(host)
	}
	log.Printf("банк: %s", br.Host())

	client := bank.NewClient(br, endpoints)
	session := bank.NewSession(br, client)

	// Шаги перевода нужны, только если он включён. Ошибку не роняем: остальной
	// бот должен работать, даже если transfer.json не заполнен.
	var steps *bank.TransferSteps
	if cfg.Transfer.Enabled {
		if steps, err = bank.LoadTransferSteps(cfg.Transfer.StepsPath); err != nil {
			log.Printf("перевод выключен: %v", err)
			cfg.Transfer.Enabled = false
		} else if cfg.Transfer.Armed {
			// Боевой режим включают один раз и потом на него полагаются —
			// пусть неполный transfer.json всплывёт здесь, а не посреди
			// перевода с уже отправленной формой.
			if err := steps.ReadyForArmed(); err != nil {
				log.Printf("боевой режим выключен: %v — заполните %s (см. /inspect) и перезапустите",
					err, cfg.Transfer.StepsPath)
				cfg.Transfer.Armed = false
			} else {
				log.Print("ВНИМАНИЕ: перевод в боевом режиме — /pay отправит реальные деньги")
			}
		}
		if cfg.Transfer.Enabled && !cfg.Transfer.Armed {
			log.Print("перевод в черновом режиме: /pay заполнит форму, но не отправит")
		}
	}

	bot, err := tg.New(tg.Deps{
		Cfg:     cfg,
		Browser: br,
		Client:  client,
		Store:   st,
		Session: session,
		Mirrors: mirrors,
		Steps:   steps,
	})
	if err != nil {
		return err
	}

	// Телеграм поднимаем первым, чтобы бот отвечал на команды сразу.
	go bot.Start()
	log.Print("бот запущен")

	if err := br.Open("/home"); err != nil {
		log.Printf("не удалось открыть кабинет: %v", err)
	}

	// Дальше вход не выпрашивается командой, а просто ожидается: человек
	// логинится в открытом окне когда угодно, наблюдатель это замечает.
	bot.NotifyLogin("Открыл окно банка. Войдите в него — я замечу сам и начну следить за счётом.", false)
	log.Print("жду входа в открытом окне браузера")

	go session.Watch(ctx,
		func(alive bool) {
			if alive {
				log.Print("вход виден, слежу за счётом")
				bot.NotifyLogin("✅ Вижу вход. Слежу за поступлениями.", false)
				return
			}
			log.Print("вход пропал")
			bot.NotifyLogin("❌ Вход в банк пропал. Зайдите заново в открытом окне — я замечу.", true)
		},
		func(err error) {
			log.Printf("окно браузера недоступно: %v", err)
			bot.NotifyLogin("❌ Окно браузера закрыто, я больше не вижу банк. Запустите бота заново.", true)
			stop()
		},
	)

	// Счётчик трогает только горутина поллера, синхронизация не нужна.
	failures := 0

	p := poller.New(client, st, cfg.PollInterval, cfg.NotifyMaxAge, cfg.HistoryLimit, poller.Events{
		OnIncoming: bot.NotifyIncoming,
		OnSuccess:  func() { failures = 0 },
		Ready:      session.Ready,
		OnAuthLost: func() {
			bot.NotifyLogin("❌ Сессия в банке протухла. Отправьте /login и зайдите в кабинет руками.", true)
		},
		OnError: func(err error) {
			// Закрытое окно браузера не лечится повтором: без него бот
			// беспомощен, и молотить раз в минуту одинаковой ошибкой
			// бессмысленно.
			if bank.IsBrowserClosed(err) {
				log.Print("окно браузера закрыто — останавливаюсь")
				bot.NotifyLogin("❌ Окно браузера закрыто, я больше не вижу банк. Запустите бота заново.", true)
				stop()
				return
			}

			log.Printf("опрос: %v", err)
			failures++
			if failures >= errorsBeforeMirrorSwitch {
				failures = 0
				switchMirror(ctx, cfg, br, mirrors, bot)
			}
		},
	})
	go p.Run(ctx)

	<-ctx.Done()
	log.Print("останавливаюсь")
	bot.Stop()
	return nil
}

// switchMirror пробует переехать на живое зеркало, когда текущее перестало
// отвечать.
func switchMirror(ctx context.Context, cfg *config.Config, br *bank.Browser, mirrors *bank.MirrorFinder, bot *tg.Bot) {
	old := br.Host()

	if !cfg.AutoMirror {
		bot.Notify("Банк не отвечает несколько попыток подряд на <code>" + old + "</code>.\n" +
			"Возможно, зеркало умерло — поставьте BANK_HOST=auto или укажите другой домен.")
		return
	}

	log.Print("зеркало не отвечает, ищу живое")
	host, err := mirrors.Find(ctx)
	if err != nil {
		log.Printf("поиск зеркала: %v", err)
		return
	}
	if host == old {
		return
	}

	br.SetHost(host)
	log.Printf("переехал с %s на %s", old, host)

	// Куки привязаны к домену, поэтому переезд означает новый вход.
	bot.Notify("Зеркало <code>" + old + "</code> перестало отвечать, переехал на <code>" + host + "</code>.\n" +
		"Куки к домену не переезжают — отправьте /login и зайдите заново.")
}
