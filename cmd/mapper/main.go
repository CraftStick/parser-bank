// Команда mapper разбирает уже снятые дампы и составляет endpoints.json.
//
// Recorder делает это сам на выходе, но mapper позволяет перезапустить разбор
// по готовым дампам — например, после правки эвристик или чтобы просто ещё раз
// посмотреть отчёт, не заходя в банк заново.
package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/valerakrut/parserbank/internal/config"
	"github.com/valerakrut/parserbank/internal/discover"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("[mapper] ")

	reportOnly := flag.Bool("report", false, "только показать отчёт, ничего не записывать")
	flag.Parse()

	if err := run(*reportOnly); err != nil {
		log.Fatalf("остановлен: %v", err)
	}
}

func run(reportOnly bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	res, err := discover.Analyze(cfg.RecordDir)
	if err != nil {
		return err
	}

	fmt.Print(res.Report())

	if res.Accounts == nil && res.Transactions == nil {
		return fmt.Errorf("в %s не нашлось ни счетов, ни операций — похоже, "+
			"нужные экраны кабинета не были открыты во время записи", cfg.RecordDir)
	}
	if reportOnly {
		return nil
	}

	return discover.Save(res, cfg.EndpointsPath)
}
