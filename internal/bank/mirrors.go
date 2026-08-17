package bank

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/playwright-community/playwright-go"
)

// DefaultMirrors — домены-зеркала интернет-банка, вытащенные из CSP самого
// сайта. Банк держит их про запас и вводит в строй по мере того, как старые
// перестают открываться, поэтому часть списка в любой момент времени не
// резолвится — это нормально.
var DefaultMirrors = []string{
	"online.sbpvtb.ru",
	"online.vtbsbp.ru",
	"online.vneshtbank.ru",
	"online.spbvtb.ru",
	"online.vtbspb.ru",
	"online.novilynx.ru",
	"online.corebine.ru",
	"online.tracknest.ru",
	"online.nexosa.ru",
}

// MirrorFinder ищет живое зеркало.
//
// Проверка идёт в два шага, и порядок здесь важен. Сначала DNS: он не
// генерирует ни одного запроса к банку, поэтому им можно отсеять мёртвые
// домены бесплатно. И только выживших мы трогаем — живым браузером, а не
// http-клиентом.
//
// Причина второго шага измеримая: edge банка отдаёт «302 Security Redirect»
// не-браузерным клиентам уже через несколько запросов, а дальше просто рвёт
// соединение. Проба обычным http.Client работает ровно один раз, после чего
// приводит к бану IP — то есть ломает ровно то, что чинит.
type MirrorFinder struct {
	br         *Browser
	candidates []string
	cooldown   time.Duration

	mu        sync.Mutex
	lastProbe time.Time
	alive     []string
}

func NewMirrorFinder(br *Browser, candidates []string) *MirrorFinder {
	if len(candidates) == 0 {
		candidates = DefaultMirrors
	}
	return &MirrorFinder{
		br:         br,
		candidates: candidates,
		// Полный перебор — редкая операция. Чаще смысла нет: домены живут
		// неделями, а частые пробы сами по себе выглядят подозрительно.
		cooldown: 10 * time.Minute,
	}
}

// Alive возвращает зеркала, признанные живыми на последней пробе.
func (f *MirrorFinder) Alive() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.alive...)
}

// Candidates — полный список кандидатов.
func (f *MirrorFinder) Candidates() []string {
	return append([]string(nil), f.candidates...)
}

// Find возвращает первое зеркало, которое ответило как настоящий кабинет.
//
// Текущий хост проверяется первым: если он жив, никакого перебора не будет.
func (f *MirrorFinder) Find(ctx context.Context) (string, error) {
	f.mu.Lock()
	if time.Since(f.lastProbe) < f.cooldown && len(f.alive) > 0 {
		host := f.alive[0]
		f.mu.Unlock()
		return host, nil
	}
	f.mu.Unlock()

	// Текущий хост вперёд, дубликаты убираем.
	ordered := make([]string, 0, len(f.candidates)+1)
	seen := map[string]bool{}
	for _, h := range append([]string{f.br.Host()}, f.candidates...) {
		if h != "" && !seen[h] {
			seen[h] = true
			ordered = append(ordered, h)
		}
	}

	resolvable := Resolvable(ctx, ordered)
	if len(resolvable) == 0 {
		return "", fmt.Errorf("ни один из %d доменов не резолвится — похоже, проблема с сетью, а не с зеркалами", len(ordered))
	}

	var alive []string
	for i, host := range resolvable {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// Пауза между кандидатами: пачка запросов подряд — это ровно то,
		// на что реагирует WAF.
		if i > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}

		if f.probe(host) {
			log.Printf("зеркало %s отвечает", host)
			alive = append(alive, host)
			break // первого живого достаточно
		}
		log.Printf("зеркало %s не отвечает", host)
	}

	f.mu.Lock()
	f.lastProbe = time.Now()
	f.alive = alive
	f.mu.Unlock()

	if len(alive) == 0 {
		return "", fmt.Errorf("живых зеркал не найдено (проверено %d)", len(resolvable))
	}
	return alive[0], nil
}

// probe стучится в кандидата через браузер и проверяет, что ответил именно
// кабинет банка, а не заглушка провайдера и не страница WAF.
func (f *MirrorFinder) probe(host string) bool {
	resp, err := f.br.Context().Request().Get("https://"+host+"/home", playwright.APIRequestContextGetOptions{
		Timeout:          playwright.Float(15000),
		MaxRedirects:     playwright.Int(0),
		FailOnStatusCode: playwright.Bool(false),
	})
	if err != nil {
		return false
	}
	defer resp.Dispose()

	if resp.Status() != 200 {
		return false
	}

	// Кабинет отдаёт собственные служебные заголовки. По ним отличаем его
	// от чего угодно другого, что вернуло 200 на этот адрес.
	headers := resp.Headers()
	_, hasIndex := headers["page-index"]
	_, hasHost := headers["http-host"]
	return hasIndex || hasHost
}

// Resolvable оставляет только те домены, которые резолвятся. Обращений к
// банку не делает.
func Resolvable(ctx context.Context, hosts []string) []string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	type result struct {
		host string
		ok   bool
	}

	ch := make(chan result, len(hosts))
	for _, h := range hosts {
		go func(h string) {
			addrs, err := net.DefaultResolver.LookupHost(ctx, h)
			ch <- result{host: h, ok: err == nil && len(addrs) > 0}
		}(h)
	}

	ok := make(map[string]bool, len(hosts))
	for range hosts {
		r := <-ch
		ok[r.host] = r.ok
	}

	// Возвращаем в исходном порядке — он отражает приоритет.
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if ok[h] {
			out = append(out, h)
		}
	}
	return out
}
