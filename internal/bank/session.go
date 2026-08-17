package bank

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync/atomic"
	"time"
)

// Session следит за тем, жива ли авторизация в личном кабинете.
//
// Логин и пароль здесь не хранятся и не вводятся программно — сознательно.
// Причины две. Первая: вход всё равно подтверждается кодом из SMS или пуша,
// так что полностью автоматический вход невозможен, и хранение пароля просто
// добавило бы риск, ничего не дав взамен. Вторая: если бот переедет на VPS,
// на чужой машине не будет лежать ничего, чем можно увести счёт — только
// профиль браузера с сессией, который банк в любой момент может погасить.
//
// Поэтому вход делает человек, один раз, в видимом окне браузера. Дальше
// сессия живёт в профиле, а бот только замечает, когда она умерла, и просит
// зайти снова.
type Session struct {
	br *Browser
	cl *Client

	// Последнее известное состояние входа. Читается из горутины поллера,
	// пишется наблюдателем.
	alive atomic.Bool

	// Когда вход появился. Хранится наносекундами в int64: время жизни сессии
	// приходится мерить эмпирически — банк нигде его не объявляет, а знать,
	// сколько она продержалась в прошлый раз, важнее любых оценок.
	aliveSince atomic.Int64
}

func NewSession(br *Browser, cl *Client) *Session {
	return &Session{br: br, cl: cl}
}

// Ready — виден ли вход прямо сейчас, по последней проверке.
// Не ходит ни в банк, ни в браузер.
func (s *Session) Ready() bool { return s.alive.Load() }

// AliveFor — сколько вход держится. Ноль, если входа сейчас нет.
func (s *Session) AliveFor() time.Duration {
	if !s.alive.Load() {
		return 0
	}
	since := s.aliveSince.Load()
	if since == 0 {
		return 0
	}
	return time.Since(time.Unix(0, since))
}

// Watch следит за входом и сообщает, когда он появляется или пропадает.
//
// Это замена схеме «команда /login»: человек логинится тогда, когда ему
// удобно, а бот просто замечает результат. Требовать нажать кнопку до входа
// означало бы, что естественный порядок действий — открыл окно и вошёл —
// приводит к сообщению об ошибке.
//
// Наличие токена проверяется чтением страницы, без единого запроса в банк:
// пока вход не сделан, никакого трафика нет вовсе. Запрос уходит один — чтобы
// подтвердить вход в момент его появления.
func (s *Session) Watch(ctx context.Context, onChange func(alive bool), onBroken func(error)) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	var (
		lastErr    string
		lastReport time.Time
		sawToken   bool
	)

	for {
		ready, err := s.cl.TokenState()
		if err != nil {
			if onBroken != nil {
				onBroken(err)
			}
			return
		}

		switch {
		case ready && !s.alive.Load():
			if !sawToken {
				sawToken = true
				log.Print("токен на странице появился, проверяю доступ к банку")
			}

			// Молчаливое ожидание тут недопустимо: если токен есть, а запрос
			// не проходит, без текста ошибки не отличить истёкший токен от
			// неверного адреса ручки.
			if err := s.cl.Check(); err != nil {
				if msg := err.Error(); msg != lastErr || time.Since(lastReport) > time.Minute {
					lastErr, lastReport = msg, time.Now()
					log.Printf("токен есть, но запрос не проходит: %v", err)
				}
				break
			}

			lastErr = ""
			s.aliveSince.Store(time.Now().UnixNano())
			s.alive.Store(true)
			if onChange != nil {
				onChange(true)
			}

		case !ready && s.alive.Load():
			sawToken = false
			log.Printf("вход пропал, прожив %s", s.AliveFor().Round(time.Second))
			s.alive.Store(false)
			if onChange != nil {
				onChange(false)
			}

		case !ready:
			// Раз в полминуты напоминаем, что ждём именно входа, —
			// иначе молчащий лог неотличим от зависшего бота.
			if time.Since(lastReport) > 30*time.Second {
				lastReport = time.Now()
				tabs := s.br.TabPaths()
				if len(tabs) == 0 {
					log.Print("вход пока не виден — и ни одной вкладки банка не открыто")
				} else {
					log.Printf("вход пока не виден — жду (вкладки банка: %v)", tabs)
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Alive — жива ли сессия прямо сейчас.
func (s *Session) Alive() bool { return s.cl.Alive() }

// Ensure открывает кабинет и ждёт, пока сессия станет пригодной для запросов.
//
// Ждать обязательно. Токен ВТБ лежит в sessionStorage, то есть исчезает вместе
// с браузером: при старте бота его там нет, и он появляется только после того,
// как SPA загрузится и восстановит сессию по кукам. Проверка сразу после
// domcontentloaded всегда показывала бы «сессия недействительна», хотя вход
// в профиле есть.
func (s *Session) Ensure(ctx context.Context, timeout time.Duration) error {
	if err := s.br.Open("/home"); err != nil {
		return err
	}

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		// Сначала бесплатная проверка страницы, и только потом — запрос
		// в банк. Так неавторизованных обращений не будет вовсе.
		if s.cl.TokenReady() && s.cl.Alive() {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		if time.Now().After(deadline) {
			return ErrNotAuthenticated
		}
	}
}

// IsAuthError — удобная проверка для вызывающего кода.
func IsAuthError(err error) bool { return errors.Is(err, ErrNotAuthenticated) }

// IsBrowserClosed сообщает, что окна браузера больше нет.
//
// Отдельный случай: если закрыть окно руками, все дальнейшие запросы будут
// падать одинаково, и без такой проверки бот раз в минуту писал бы в лог
// «target closed», делая вид, что просто не повезло с сетью.
func IsBrowserClosed(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "target closed") ||
		strings.Contains(msg, "browser has been closed") ||
		strings.Contains(msg, "transport closed")
}
