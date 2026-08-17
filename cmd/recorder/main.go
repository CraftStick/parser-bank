// Команда recorder — разовый discovery-шаг.
//
// Открывает личный кабинет в видимом окне, ждёт, пока вы походите по нему
// руками, и складывает все JSON-ответы банка в RECORD_DIR. На выходе сам
// разбирает снятое и составляет endpoints.json: какая ручка отдаёт остатки,
// какая — операции, и как называются поля внутри ответа.
//
// Запускать нужно один раз (и потом — если банк поменяет API).
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/valerakrut/parserbank/internal/bank"
	"github.com/valerakrut/parserbank/internal/config"
	"github.com/valerakrut/parserbank/internal/discover"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[recorder] ")

	if err := run(); err != nil {
		log.Fatalf("остановлен: %v", err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Без окна тут делать нечего: логин и клики по кабинету — ручные.
	cfg.Headless = false

	rec, err := bank.NewRecorder(cfg.RecordDir, cfg.BankHost)
	if err != nil {
		return err
	}

	br, err := bank.Launch(cfg)
	if err != nil {
		return err
	}
	defer br.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.AutoMirror {
		log.Print("подбираю живое зеркало")
		host, err := bank.NewMirrorFinder(br, cfg.Mirrors).Find(ctx)
		if err != nil {
			return err
		}
		br.SetHost(host)
	}
	log.Printf("банк: %s", br.Host())

	var dirty atomic.Bool
	rec.OnRecord = func() { dirty.Store(true) }
	rec.Attach(br)

	if err := br.Open("/home"); err != nil {
		return err
	}

	log.Print("──────────────────────────────────────────────")
	log.Print("1. Войдите в кабинет в открывшемся окне")
	log.Print("2. Откройте главную со счетами")
	log.Print("3. Откройте историю операций и пролистайте её")
	log.Print("4. Я сам скажу, когда нашёл всё нужное")
	log.Print("")
	log.Print("Закончив, нажмите Enter в этом окне.")
	log.Print("──────────────────────────────────────────────")

	go watch(ctx, cfg.RecordDir, &dirty)

	waitForFinish(ctx)

	log.Printf("поймано ручек: %d", rec.Count())

	if err := rec.Flush(); err != nil {
		return err
	}

	res, err := discover.Analyze(cfg.RecordDir)
	if err != nil {
		return err
	}

	fmt.Print("\n" + res.Report())

	if res.Accounts == nil && res.Transactions == nil {
		log.Print("ни счетов, ни операций не нашлось — видимо, нужные экраны не открывались")
		log.Printf("дампы остались в %s, разбор можно повторить: make map", cfg.RecordDir)
		return nil
	}

	return discover.Save(res, cfg.EndpointsPath)
}

// waitForFinish ждёт Enter или сигнал.
//
// Enter здесь основной способ: по Ctrl+C сигнал прилетает всей группе
// процессов, и make сообщает об ошибке, хотя запись завершилась штатно.
func waitForFinish(ctx context.Context) {
	entered := make(chan struct{})
	go func() {
		defer close(entered)
		bufio.NewReader(os.Stdin).ReadString('\n')
	}()

	select {
	case <-ctx.Done():
	case <-entered:
	}
}

// watch подсказывает, когда можно заканчивать: без этого непонятно, хватит ли
// уже накликанного или надо открыть ещё пару экранов.
func watch(ctx context.Context, dir string, dirty *atomic.Bool) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()

	var announced bool

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !dirty.Swap(false) || announced {
				continue
			}

			res, err := discover.Analyze(dir)
			if err != nil {
				continue
			}
			switch {
			case res.Accounts != nil && res.Transactions != nil:
				log.Print("✓ нашёл и счета, и операции — нажимайте Enter")
				announced = true
			case res.Transactions != nil:
				log.Print("· операции есть, счетов пока нет — откройте главную")
			case res.Accounts != nil:
				log.Print("· счета есть, операций пока нет — откройте историю и пролистайте её")
			}
		}
	}
}
