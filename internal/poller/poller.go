// Package poller периодически спрашивает у банка список операций и замечает
// новые зачисления.
package poller

import (
	"context"
	"log"
	"time"

	"github.com/valerakrut/parserbank/internal/bank"
	"github.com/valerakrut/parserbank/internal/store"
)

// Events — куда поллер сообщает о происходящем.
type Events struct {
	OnIncoming func(bank.Transaction) // новое зачисление
	OnAuthLost func()                 // сессия умерла, нужен вход руками
	OnError    func(error)            // всё остальное
	OnSuccess  func()                 // проход прошёл без ошибок

	// Ready решает, можно ли сейчас опрашивать. Пока вход не сделан, ходить
	// в банк незачем: ответом будет отказ, а очередь неавторизованных
	// запросов — ровно то, на что смотрит антифрод.
	Ready func() bool
}

type Poller struct {
	cl       *bank.Client
	st       *store.Store
	ev       Events
	interval time.Duration
	limit    int
	maxAge   time.Duration

	// Чтобы не долбить владельца сообщением про потерянную сессию каждую минуту.
	lastAuthNotice time.Time
}

// New создаёт поллер. maxAge — предельный возраст зачисления, о котором ещё
// имеет смысл уведомлять; ноль отключает проверку.
func New(cl *bank.Client, st *store.Store, interval, maxAge time.Duration, limit int, ev Events) *Poller {
	return &Poller{cl: cl, st: st, ev: ev, interval: interval, maxAge: maxAge, limit: limit}
}

// Run крутит опрос до отмены контекста.
func (p *Poller) Run(ctx context.Context) {
	// Первый проход сразу, не дожидаясь тика.
	p.tick()

	t := time.NewTicker(p.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Print("поллер остановлен")
			return
		case <-t.C:
			p.tick()
		}
	}
}

func (p *Poller) tick() {
	if p.ev.Ready != nil && !p.ev.Ready() {
		return
	}
	if err := p.Poll(); err != nil {
		if bank.IsAuthError(err) {
			p.noticeAuthLost()
			return
		}
		if p.ev.OnError != nil {
			p.ev.OnError(err)
		}
		return
	}
	if p.ev.OnSuccess != nil {
		p.ev.OnSuccess()
	}
}

// Poll делает один проход: забирает операции, сохраняет новые и сообщает о
// зачислениях.
func (p *Poller) Poll() error {
	txs, err := p.cl.Transactions(p.limit)
	if err != nil {
		return err
	}

	bootstrapped, err := p.st.Bootstrapped()
	if err != nil {
		return err
	}

	// Идём от старых к новым, чтобы уведомления пришли в хронологическом
	// порядке, а не задом наперёд.
	for i := len(txs) - 1; i >= 0; i-- {
		tx := txs[i]

		isNew, err := p.st.Save(tx)
		if err != nil {
			return err
		}

		// На первом запуске база пуста, и «новыми» выглядят все операции.
		// Молча запоминаем их и начинаем уведомлять со следующего прохода.
		if !isNew || !bootstrapped || tx.Direction != bank.DirectionIn {
			continue
		}
		// Уведомление о позавчерашнем зачислении — не новость, а шум.
		// Так бывает после простоя: банк отдаёт период целиком, и без этой
		// проверки бот вывалил бы всю историю разом.
		if p.maxAge > 0 && !tx.Time.IsZero() && time.Since(tx.Time) > p.maxAge {
			log.Printf("пропускаю старое зачисление от %s", tx.Time.Format("02.01.2006 15:04"))
			continue
		}
		if p.ev.OnIncoming != nil {
			p.ev.OnIncoming(tx)
		}
	}

	// Прогрев засчитываем только на непустом ответе: иначе пустая выдача
	// (скажем, за период без операций) объявила бы бота прогретым, и первая
	// же настоящая история прилетела бы владельцу пачкой уведомлений.
	if !bootstrapped && len(txs) > 0 {
		if err := p.st.SetBootstrapped(); err != nil {
			return err
		}
		log.Printf("первый проход: запомнил %d операций без уведомлений", len(txs))
	}

	return p.st.Prune()
}

func (p *Poller) noticeAuthLost() {
	if time.Since(p.lastAuthNotice) < 30*time.Minute {
		return
	}
	p.lastAuthNotice = time.Now()
	log.Print("сессия в банке недействительна")
	if p.ev.OnAuthLost != nil {
		p.ev.OnAuthLost()
	}
}
