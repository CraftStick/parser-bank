// Package tg — телеграм-интерфейс: команды владельца и уведомления о
// поступлениях.
package tg

import (
	"context"
	"fmt"
	"html"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/valerakrut/parserbank/internal/bank"
	"github.com/valerakrut/parserbank/internal/config"
	"github.com/valerakrut/parserbank/internal/store"
	tele "gopkg.in/telebot.v4"
)

// pageSize — сколько операций показывать за раз. Больше десятка в одном
// сообщении читать невозможно, поэтому история листается.
const pageSize = 5

// Кнопки объявлены на уровне пакета: telebot связывает обработчик с кнопкой
// по полю Unique, и оно должно совпадать при регистрации и при отрисовке.
var (
	btnHistory  = tele.Btn{Unique: "history"}
	btnBalance  = tele.Btn{Unique: "balance"}
	btnStatus   = tele.Btn{Unique: "status"}
	btnMenu     = tele.Btn{Unique: "menu"}
	btnBackfill = tele.Btn{Unique: "backfill"}
	btnNoop     = tele.Btn{Unique: "noop"}

	btnPay        = tele.Btn{Unique: "pay"}
	btnPayConfirm = tele.Btn{Unique: "pay_confirm"}
	btnPayCancel  = tele.Btn{Unique: "pay_cancel"}
)

type Deps struct {
	Cfg     *config.Config
	Browser *bank.Browser
	Client  *bank.Client
	Store   *store.Store
	Session *bank.Session
	Mirrors *bank.MirrorFinder
	Steps   *bank.TransferSteps // nil, если перевод выключен
}

type Bot struct {
	b     *tele.Bot
	owner tele.ChatID
	d     Deps

	// startedAt — с какого момента процесс жив. Вход в банк умирает вместе с
	// ним, так что аптайм — это ещё и возраст последнего ручного входа.
	startedAt time.Time

	// Догрузка истории идёт долго и ходит в банк — двум сразу нельзя.
	backfillMu  sync.Mutex
	backfilling bool

	// Единственное сообщение-панель, которое перерисовывается вместо того,
	// чтобы плодить новые. Живёт в памяти: после перезапуска бот просто
	// заведёт новую панель, восстанавливать нечего.
	//
	// panelStale означает, что после панели в чат уже успели прилететь другие
	// сообщения — уведомления о поступлениях или предупреждения. Править
	// панель на месте тогда нельзя: ответ появится где-то выше, вне поля
	// зрения. В этом случае старая панель удаляется и рисуется новая, внизу.
	panelMu    sync.Mutex
	panelID    int
	panelStale bool

	// Идущий перевод. Одновременно только один.
	payMu   sync.Mutex
	pending *pendingPay
	// awaitAmount — нажата кнопка «Перевести себе», ждём сумму сообщением.
	awaitAmount bool
	// payMsgID — карточка перевода: одно сообщение, которое переписывается на
	// каждом шаге. 0 — карточки ещё нет.
	payMsgID int

	// Состояние входа в банк — тоже одна строка на весь его жизненный цикл:
	// «жду входа» → «вижу вход» → «вход пропал». Три сообщения подряд об одном
	// и том же читаются как лента, хотя это одно меняющееся состояние.
	loginMu    sync.Mutex
	loginMsgID int
}

// payStage — чего бот ждёт от человека по идущему переводу.
type payStage int

const (
	// stageApproval — форма отправлена на сверку, ждём нажатия «Подтвердить».
	// Деньги ещё на месте.
	stageApproval payStage = iota
	// stageCode — финальная кнопка нажата, банк запросил код из SMS.
	stageCode
)

// pendingPay — идущий перевод. Одновременно только один.
type pendingPay struct {
	amount   float64
	transfer *bank.Transfer
	deadline time.Time
	stage    payStage
}

func (b *Bot) storedMsg(id int) tele.StoredMessage {
	return tele.StoredMessage{
		MessageID: strconv.Itoa(id),
		ChatID:    int64(b.owner),
	}
}

// setPanel запоминает сообщение как панель. Оно заведомо последнее в чате.
func (b *Bot) setPanel(id int) {
	b.panelMu.Lock()
	b.panelID, b.panelStale = id, false
	b.panelMu.Unlock()
}

// markStale отмечает, что панель больше не последняя в чате.
func (b *Bot) markStale() {
	b.panelMu.Lock()
	b.panelStale = true
	b.panelMu.Unlock()
}

func (b *Bot) panelState() (id int, stale bool) {
	b.panelMu.Lock()
	defer b.panelMu.Unlock()
	return b.panelID, b.panelStale
}

// notModified — телеграм отвергает правку, если текст и кнопки не изменились.
// Для человека это просто повторное нажатие той же кнопки, а не ошибка.
func notModified(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not modified")
}

func New(d Deps) (*Bot, error) {
	if err := d.Cfg.RequireBot(); err != nil {
		return nil, err
	}

	tb, err := tele.NewBot(tele.Settings{
		Token:     d.Cfg.TGToken,
		Poller:    &tele.LongPoller{Timeout: 10 * time.Second},
		ParseMode: tele.ModeHTML,
		OnError: func(err error, c tele.Context) {
			log.Printf("telegram: %v", err)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("подключиться к Telegram: %w", err)
	}

	bot := &Bot{b: tb, owner: tele.ChatID(d.Cfg.OwnerID), d: d, startedAt: time.Now()}
	bot.routes()
	return bot, nil
}

func (b *Bot) routes() {
	// Бот отвечает только владельцу. Токен рано или поздно куда-нибудь
	// утечёт — например, в скриншот, — и без этой проверки в кабинет сможет
	// заглянуть любой, кто найдёт бота поиском.
	b.b.Use(func(next tele.HandlerFunc) tele.HandlerFunc {
		return func(c tele.Context) error {
			if c.Sender() == nil || c.Sender().ID != b.d.Cfg.OwnerID {
				log.Printf("отклонён чужой запрос от %v", c.Sender())
				return nil
			}
			return next(c)
		}
	})

	b.b.Handle("/start", b.showMenu)
	b.b.Handle("/help", b.showMenu)
	b.b.Handle("/balance", b.showBalance)
	b.b.Handle("/last", b.cmdHistory)
	b.b.Handle("/history", b.cmdHistory)
	b.b.Handle("/status", b.showStatus)
	b.b.Handle("/mirrors", b.showMirrors)
	b.b.Handle("/login", b.cmdLogin)
	b.b.Handle("/backfill", b.cmdBackfill)
	b.b.Handle("/pay", b.cmdPay)
	b.b.Handle("/cancel", b.cmdCancel)
	b.b.Handle("/inspect", b.cmdInspect)

	// Подтверждение и код приходят обычным сообщением, пока перевод идёт.
	b.b.Handle(tele.OnText, b.onText)

	b.b.Handle(&btnMenu, b.showMenu)
	b.b.Handle(&btnBalance, b.showBalance)
	b.b.Handle(&btnStatus, b.showStatus)
	b.b.Handle(&btnHistory, b.showHistory)
	b.b.Handle(&btnBackfill, b.cmdBackfill)
	b.b.Handle(&btnNoop, func(c tele.Context) error { return c.Respond() })
	b.b.Handle(&btnPay, b.onPayButton)
	b.b.Handle(&btnPayConfirm, b.onPayConfirm)
	b.b.Handle(&btnPayCancel, b.onPayCancel)
}

func (b *Bot) Start() { b.b.Start() }
func (b *Bot) Stop()  { b.b.Stop() }

// Notify сообщает владельцу о событии, которое нельзя пропустить: пропавшем
// входе, закрытом окне, завершённой догрузке.
//
// Именно новое сообщение, а не правка панели: правка не даёт уведомления на
// телефон, и предупреждение осталось бы незамеченным. Кнопок под ним нет —
// иначе после пары таких сообщений чат превращается в лестницу из одинаковых
// рядов кнопок.
func (b *Bot) Notify(text string) {
	err := b.notify(text)
	if err == nil {
		return
	}

	log.Printf("не смог отправить сообщение владельцу: %v", err)

	// Телеграм не даёт боту написать первым: пока человек не открыл диалог,
	// чата просто не существует. Ошибка выглядит загадочно, поэтому
	// подсказываем, что делать.
	if strings.Contains(err.Error(), "chat not found") {
		log.Printf("откройте диалог с ботом в Telegram и нажмите Start — "+
			"без этого он не может написать первым (TG_OWNER_ID=%d)", b.d.Cfg.OwnerID)
	}
}

// NotifyLogin показывает состояние входа в банк — одним сообщением, которое
// переписывается, а не новым на каждый чих.
//
// alarm=true для плохих новостей: там сообщение не правится, а пересоздаётся
// внизу чата. Правка не даёт уведомления на телефон, и «вход пропал»
// осталось бы незамеченным — а это ровно то, ради чего сообщение и нужно.
// В чате при этом всё равно остаётся одна строка: старая удаляется.
func (b *Bot) NotifyLogin(text string, alarm bool) {
	b.loginMu.Lock()
	defer b.loginMu.Unlock()

	if b.loginMsgID != 0 {
		if !alarm {
			_, err := b.b.Edit(b.storedMsg(b.loginMsgID), text)
			if err == nil || notModified(err) {
				return
			}
			log.Printf("строка входа недоступна, создаю новую: %v", err)
		} else if err := b.b.Delete(b.storedMsg(b.loginMsgID)); err != nil {
			log.Printf("не удалось убрать прошлую строку входа: %v", err)
		}
		b.loginMsgID = 0
	}

	sent, err := b.b.Send(b.owner, text)
	if err != nil {
		log.Printf("не смог отправить сообщение владельцу: %v", err)
		if strings.Contains(err.Error(), "chat not found") {
			log.Printf("откройте диалог с ботом в Telegram и нажмите Start — "+
				"без этого он не может написать первым (TG_OWNER_ID=%d)", b.d.Cfg.OwnerID)
		}
		return
	}
	b.loginMsgID = sent.ID
	b.markStale()
}

// NotifyIncoming сообщает о новом зачислении.
func (b *Bot) NotifyIncoming(tx bank.Transaction) {
	var sb strings.Builder
	sb.WriteString("💰 <b>Пришли деньги</b>\n\n")
	fmt.Fprintf(&sb, "<b>+%s</b>\n", esc(bank.Money(tx.Amount, tx.Currency)))

	if tx.Title != "" {
		sb.WriteString(quote(tx.Title))
	}
	if tx.Card != "" {
		fmt.Fprintf(&sb, "Карта: %s\n", esc(tx.Card))
	}
	if !tx.Time.IsZero() {
		fmt.Fprintf(&sb, "<i>%s</i>", tx.Time.Format("02.01.2006 в 15:04"))
	}

	// Поступления — единственное, что копится в переписке отдельными
	// сообщениями: это записи, которые хочется листать и искать потом.
	// Кнопок под ними нет намеренно, иначе навигация затёрла бы саму запись.
	if err := b.notify(sb.String()); err != nil {
		log.Printf("не смог отправить уведомление о поступлении: %v", err)
	}
}

// mainMenu — постоянный набор кнопок под сообщениями.
func mainMenu() *tele.ReplyMarkup {
	m := &tele.ReplyMarkup{}
	m.Inline(
		m.Row(
			m.Data("💰 Баланс", btnBalance.Unique),
			m.Data("📜 История", btnHistory.Unique, "0"),
		),
		m.Row(
			m.Data("⚙️ Статус", btnStatus.Unique),
			m.Data("↩️ Меню", btnMenu.Unique),
		),
		m.Row(
			m.Data("💸 Перевести себе в другой банк", btnPay.Unique),
		),
	)
	return m
}

func (b *Bot) showMenu(c tele.Context) error {
	text := strings.Join([]string{
		"<b>ParserBank</b> — следит за счётом и сообщает о поступлениях.",
		"",
		"Нажмите кнопку ниже или отправьте команду:",
		"",
		"💰 /balance — сколько сейчас на счетах",
		"📜 /last — история операций",
		"⚙️ /status — всё ли в порядке",
		"🌐 /mirrors — какие адреса банка работают",
		payLine(b.d.Cfg.Transfer.Enabled),
		"",
		"<blockquote><b><i>Вход в банк делается руками в открытом окне браузера — " +
			"я замечаю его сам.</i></b></blockquote>",
	}, "\n")

	return b.reply(c, text, mainMenu())
}

func (b *Bot) showBalance(c tele.Context) error {
	if !b.d.Session.Ready() {
		return b.reply(c, notLoggedIn(), mainMenu())
	}

	accounts, err := b.d.Client.Accounts()
	if err != nil {
		return b.reply(c, b.explain(err), mainMenu())
	}
	// Карты — необязательная ручка: если не описана, Cards вернёт пусто.
	cards, err := b.d.Client.Cards()
	if err != nil {
		log.Printf("карты: %v", err)
	}

	if len(accounts) == 0 && len(cards) == 0 {
		return b.reply(c, "Банк не вернул ни одного счёта.", mainMenu())
	}

	var sb strings.Builder
	sb.WriteString("💰 <b>Ваши счета</b>\n")
	for _, a := range append(accounts, cards...) {
		sb.WriteString("\n")
		sb.WriteString(accountLine(a))
	}

	return b.reply(c, sb.String(), mainMenu())
}

// accountLine форматирует один счёт или карту: название, последние 4 цифры,
// остаток. Полный номер не показываем — в чате он ни к чему.
func accountLine(a bank.Account) string {
	name := a.Name
	if name == "" {
		name = a.ID
	}
	if last := a.Last4(); last != "" {
		name = fmt.Sprintf("%s · %s", name, last)
	}
	return fmt.Sprintf("%s\n<b>%s</b>\n", esc(name), esc(bank.Money(a.Balance, a.Currency)))
}

// cmdHistory — текстовая команда /last: показывает первую страницу.
func (b *Bot) cmdHistory(c tele.Context) error {
	text, markup, err := b.historyPage(0)
	if err != nil {
		return b.reply(c, b.explain(err), mainMenu())
	}
	return b.reply(c, text, markup)
}

// showHistory — переход по страницам с кнопок.
func (b *Bot) showHistory(c tele.Context) error {
	page, _ := strconv.Atoi(c.Data())

	text, markup, err := b.historyPage(page)
	if err != nil {
		return b.reply(c, b.explain(err), mainMenu())
	}
	return b.reply(c, text, markup)
}

// historyPage собирает страницу истории и кнопки листания.
func (b *Bot) historyPage(page int) (string, *tele.ReplyMarkup, error) {
	total, err := b.d.Store.Count()
	if err != nil {
		return "", nil, err
	}
	if total == 0 {
		return "📜 <b>История пуста</b>\n\nПодождите первого опроса банка — " +
			"он бывает раз в " + b.d.Cfg.PollInterval.String() + ".", mainMenu(), nil
	}

	pages := (total + pageSize - 1) / pageSize
	page = min(max(page, 0), pages-1)

	txs, err := b.d.Store.Page(page*pageSize, pageSize)
	if err != nil {
		return "", nil, err
	}

	var sb strings.Builder
	sb.WriteString("📜 <b>История операций</b>\n")

	for i, tx := range txs {
		sign, mark := "−", "🔻"
		if tx.Direction == bank.DirectionIn {
			sign, mark = "+", "🟢"
		}

		// Нумерация сквозная по всей истории, а не по странице: так видно,
		// насколько глубоко забрался, без отдельной строки со счётчиком.
		sb.WriteString("\n")
		fmt.Fprintf(&sb, "%d. %s <b>%s%s</b>",
			page*pageSize+i+1, mark, sign, esc(bank.Money(tx.Amount, tx.Currency)))
		if !tx.Time.IsZero() {
			fmt.Fprintf(&sb, " · <i>%s</i>", tx.Time.Format("02.01 15:04"))
		}
		if tx.Title != "" {
			sb.WriteString("\n")
			sb.WriteString(quote(tx.Title))
		}
		sb.WriteString("\n")
	}

	return sb.String(), historyMarkup(page, pages), nil
}

// quote оформляет контрагента цитатой: в сплошном списке операций имя должно
// цепляться взглядом раньше, чем всё остальное.
func quote(s string) string {
	return "<blockquote><b><i>" + esc(s) + "</i></b></blockquote>"
}

// historyMarkup рисует стрелки листания и счётчик страниц.
func historyMarkup(page, pages int) *tele.ReplyMarkup {
	m := &tele.ReplyMarkup{}

	// Стрелка на краю списка никуда не ведёт, но убирать её нельзя:
	// кнопки поедут вбок и попадёшь не туда, куда целился.
	prev := m.Data("◀️", btnNoop.Unique)
	if page > 0 {
		prev = m.Data("◀️", btnHistory.Unique, strconv.Itoa(page-1))
	}
	next := m.Data("▶️", btnNoop.Unique)
	if page < pages-1 {
		next = m.Data("▶️", btnHistory.Unique, strconv.Itoa(page+1))
	}

	m.Inline(
		m.Row(prev, m.Data(fmt.Sprintf("%d/%d", page+1, pages), btnNoop.Unique), next),
		m.Row(
			m.Data("🔄 Обновить", btnHistory.Unique, strconv.Itoa(page)),
			m.Data("⏳ Загрузить старое", btnBackfill.Unique),
		),
		m.Row(m.Data("↩️ Меню", btnMenu.Unique)),
	)
	return m
}

func (b *Bot) showStatus(c tele.Context) error {
	var sb strings.Builder
	sb.WriteString("⚙️ <b>Состояние</b>\n\n")

	if b.d.Session.Ready() {
		// Сколько сессия держится — единственный способ узнать, на сколько её
		// хватает: банк этого нигде не объявляет.
		fmt.Fprintf(&sb, "Вход в банк: ✅ есть, держится %s\n",
			humanDuration(b.d.Session.AliveFor()))
	} else {
		sb.WriteString("Вход в банк: ❌ нет — зайдите в окне браузера\n")
	}

	// Вход живёт ровно столько, сколько процесс: перезапуск = вход руками.
	// Поэтому аптайм тут не украшение, а прогноз, когда идти логиниться.
	fmt.Fprintf(&sb, "Бот работает: %s\n", humanDuration(time.Since(b.startedAt)))

	host := b.d.Browser.Host()
	if host == "" {
		host = "не выбран"
	}
	fmt.Fprintf(&sb, "Адрес банка: <code>%s</code>", esc(host))
	if b.d.Cfg.AutoMirror {
		sb.WriteString(" (авто)")
	}

	fmt.Fprintf(&sb, "\nПроверяю счёт: раз в %s\n", b.d.Cfg.PollInterval)

	if n, err := b.d.Store.Count(); err == nil {
		fmt.Fprintf(&sb, "Операций сохранено: %d\n", n)
	}

	if b.d.Cfg.Transfer.Enabled {
		mode := "черновой (ничего не уходит)"
		if b.d.Cfg.Transfer.Armed {
			mode = "боевой"
		}
		fmt.Fprintf(&sb, "Перевод /pay: включён, режим %s\n", mode)
	}

	fmt.Fprintf(&sb, "\n<i>Сообщаю о поступлениях не старше %s.</i>", b.d.Cfg.NotifyMaxAge)

	return b.reply(c, sb.String(), mainMenu())
}

func (b *Bot) showMirrors(c tele.Context) error {
	current := b.d.Browser.Host()
	candidates := b.d.Mirrors.Candidates()

	// Резолвим DNS, а не стучимся в банк: WAF отдаёт «302 Security Redirect»
	// не-браузерным клиентам уже через несколько запросов, поэтому опрос всех
	// зеркал по команде — верный способ получить бан по IP.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resolvable := bank.Resolvable(ctx, candidates)

	live := make(map[string]bool, len(resolvable))
	for _, h := range resolvable {
		live[h] = true
	}

	var sb strings.Builder
	sb.WriteString("🌐 <b>Адреса банка</b>\n")
	fmt.Fprintf(&sb, "Сейчас работаю через <code>%s</code>\n\n", esc(orDash(current)))

	for _, h := range candidates {
		mark := "—"
		if live[h] {
			mark = "✅"
		}
		prefix := ""
		if h == current {
			prefix = "▶ "
		}
		fmt.Fprintf(&sb, "%s%s <code>%s</code>\n", prefix, mark, esc(h))
	}

	sb.WriteString("\n<i>✅ — адрес отвечает в DNS. ВТБ регулярно меняет их, " +
		"на другой я переключусь сам.</i>")
	return b.reply(c, sb.String(), mainMenu())
}

func (b *Bot) cmdLogin(c tele.Context) error {
	if b.d.Cfg.Headless {
		return b.reply(c, "Браузер запущен без окна — войти руками некуда.\n"+
			"Поставьте HEADLESS=false и перезапустите, либо подключитесь к машине по VNC.",
			mainMenu())
	}

	if err := b.d.Browser.Open("/home"); err != nil {
		return b.reply(c, "Не удалось открыть страницу банка: "+esc(err.Error()), mainMenu())
	}

	// Ответ уходит в ту же строку состояния входа, которую потом перепишет
	// наблюдатель: «войдите» и «вижу вход» — одна мысль, а не переписка.
	b.NotifyLogin("Открыл страницу банка в окне браузера. Войдите там — "+
		"я замечу сам, отправлять команду повторно не нужно.", false)
	return nil
}

// cmdBackfill догружает историю за прошлые месяцы.
//
// Банк отдаёт операции только за период, и обычный опрос смотрит последний
// месяц. Всё, что старше, приходится запрашивать отдельными окнами — по
// одному, с паузами: пачка запросов подряд это ровно то, на что реагирует
// антифрод.
func (b *Bot) cmdBackfill(c tele.Context) error {
	if !b.d.Session.Ready() {
		return b.reply(c, notLoggedIn(), mainMenu())
	}

	months := 6
	if args := c.Args(); len(args) > 0 {
		if n, err := strconv.Atoi(args[0]); err == nil && n > 0 {
			months = min(n, 24)
		}
	}

	b.backfillMu.Lock()
	if b.backfilling {
		b.backfillMu.Unlock()
		return b.reply(c, "Догрузка уже идёт, подождите.", mainMenu())
	}
	b.backfilling = true
	b.backfillMu.Unlock()

	if err := b.reply(c, fmt.Sprintf("⏳ Догружаю историю за %d мес. Это займёт около %d сек.",
		months, months*4), nil); err != nil {
		log.Printf("telegram: %v", err)
	}

	go b.runBackfill(months)
	return nil
}

func (b *Bot) runBackfill(months int) {
	defer func() {
		b.backfillMu.Lock()
		b.backfilling = false
		b.backfillMu.Unlock()
	}()

	added, failed := 0, 0
	for k := 1; k <= months; k++ {
		// Окно сдвигаем на месяц назад за шаг: границы периода в конфиге
		// заданы смещениями от «сейчас», поэтому достаточно сдвинуть точку
		// отсчёта.
		anchor := time.Now().AddDate(0, -k, 0)

		txs, err := b.d.Client.TransactionsWindow(anchor)
		if err != nil {
			log.Printf("догрузка за %s: %v", anchor.Format("01.2006"), err)
			failed++
			continue
		}

		for _, tx := range txs {
			isNew, err := b.d.Store.Save(tx)
			if err != nil {
				log.Printf("догрузка, сохранение: %v", err)
				break
			}
			if isNew {
				added++
			}
		}

		time.Sleep(3 * time.Second)
	}

	if err := b.d.Store.Prune(); err != nil {
		log.Printf("догрузка, очистка: %v", err)
	}

	total, _ := b.d.Store.Count()
	msg := fmt.Sprintf("✅ Готово. Новых операций: <b>%d</b>, всего в истории: %d.", added, total)
	if failed > 0 {
		msg += fmt.Sprintf("\n<i>Не удалось получить %d период(ов) — попробуйте позже.</i>", failed)
	}
	b.Notify(msg)
}

// cmdPay готовит перевод себе на заранее заданный счёт.
//
// Из чата приходит только сумма. Получатель — из конфига: даже с доступом к
// боту деньги не увести никуда, кроме прописанного счёта. После подготовки
// бот ждёт код из SMS обычным сообщением.
func (b *Bot) cmdPay(c tele.Context) error {
	if msg := b.payUnavailable(); msg != "" {
		return c.Send(msg)
	}

	b.adoptPanel(c)

	args := c.Args()
	if len(args) == 0 {
		return b.openPayCard()
	}
	b.startPay(args[0])
	return nil
}

// onPayButton — кнопка «Перевести себе» в меню. Суммы у неё нет, поэтому бот
// спрашивает её и ждёт ответа следующим сообщением.
func (b *Bot) onPayButton(c tele.Context) error {
	if err := c.Respond(); err != nil {
		log.Printf("telegram: %v", err)
	}
	if msg := b.payUnavailable(); msg != "" {
		return b.reply(c, msg, mainMenu())
	}
	b.adoptPanel(c)
	return b.openPayCard()
}

// adoptPanel делает карточкой перевода то сообщение, в котором человек его
// начал: нажатую панель меню, а для команды — ту же панель, если она ещё
// внизу чата. Иначе перевод открывался бы новым сообщением под меню, хотя
// начался нажатием в нём.
func (b *Bot) adoptPanel(c tele.Context) {
	id := 0
	if cb := c.Callback(); cb != nil && cb.Message != nil {
		id = cb.Message.ID
	} else if pid, stale := b.panelState(); pid != 0 && !stale {
		id = pid
	}

	if id != 0 {
		b.setPanel(id)
	}
	b.payMu.Lock()
	b.payMsgID = id
	b.payMu.Unlock()
}

// openPayCard заводит карточку перевода и спрашивает сумму.
func (b *Bot) openPayCard() error {
	b.payMu.Lock()
	if b.pending != nil {
		b.payMu.Unlock()
		// Отдельным сообщением: в карточке сейчас живой перевод, и переписать
		// её значило бы стереть кнопки, которыми его заканчивают.
		return b.notify("Один перевод уже идёт. Закончите его или отправьте /cancel.")
	}
	b.awaitAmount = true
	b.payMu.Unlock()

	return b.showPay(b.askAmountText(), nil)
}

// finishPay показывает итог перевода и возвращает меню: карточка одна, и
// уходить из неё человеку больше некуда.
func (b *Bot) finishPay(text string) error {
	return b.showPay(text, mainMenu())
}

// showPay держит весь перевод в одном сообщении: каждый шаг заменяет
// предыдущий, а не добавляет новый. Диалог о деньгах должен читаться как одна
// карточка, а не как лента из пяти реплик.
//
// Если сообщение править нельзя (удалено, слишком старое), карточка заводится
// заново — потерять шаг хуже, чем показать новое сообщение.
func (b *Bot) showPay(text string, markup *tele.ReplyMarkup) error {
	b.payMu.Lock()
	id := b.payMsgID
	b.payMu.Unlock()

	if id != 0 {
		var err error
		if markup != nil {
			_, err = b.b.Edit(b.storedMsg(id), text, markup)
		} else {
			_, err = b.b.Edit(b.storedMsg(id), text)
		}
		if err == nil || notModified(err) {
			return nil
		}
		log.Printf("карточка перевода недоступна, создаю новую: %v", err)
	}

	var (
		sent *tele.Message
		err  error
	)
	if markup != nil {
		sent, err = b.b.Send(b.owner, text, markup)
	} else {
		sent, err = b.b.Send(b.owner, text)
	}
	if err != nil {
		log.Printf("telegram: %v", err)
		return err
	}

	b.payMu.Lock()
	b.payMsgID = sent.ID
	b.payMu.Unlock()
	b.markStale()
	return nil
}

// payUnavailable возвращает причину, по которой перевод сейчас невозможен,
// или пустую строку.
func (b *Bot) payUnavailable() string {
	if !b.d.Cfg.Transfer.Enabled || b.d.Steps == nil {
		return "Перевод не настроен. Задайте получателя в .env " +
			"(TRANSFER_PHONE, TRANSFER_BANK, TRANSFER_MAX_AMOUNT) и опишите форму в transfer.json."
	}
	if !b.d.Session.Ready() {
		return notLoggedIn()
	}
	return ""
}

func (b *Bot) askAmountText() string {
	t := b.d.Cfg.Transfer

	limits := fmt.Sprintf("Минимум %s.", moneyShort(t.MinAmount))
	// Потолок показывается, только если он задан: «максимум 0 ₽» — бессмыслица.
	if t.MaxAmount > 0 {
		limits = fmt.Sprintf("Минимум %s, максимум %s.",
			moneyShort(t.MinAmount), moneyShort(t.MaxAmount))
	}

	return fmt.Sprintf("Сколько перевести себе (%s, номер тел. %s)?\n%s\n"+
		"<i>Отменить — /cancel.</i>",
		esc(t.Bank), esc(maskPhone(t.Phone)), esc(limits))
}

// startPay разбирает сумму и запускает подготовку. Всё, что нужно сказать
// человеку, уходит в ту же карточку — и для команды, и для суммы, присланной
// после кнопки.
func (b *Bot) startPay(raw string) {
	t := b.d.Cfg.Transfer

	amount, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(raw), ",", "."), 64)
	switch {
	case err != nil || amount <= 0:
		// Сумму ждём дальше: человек просто опечатался.
		b.showPay("Не понял сумму.\n"+b.askAmountText(), nil)
		return
	case t.AmountTooSmall(amount):
		b.showPay(fmt.Sprintf("Меньше %s банк не переведёт.\n%s",
			esc(moneyShort(t.MinAmount)), b.askAmountText()), nil)
		return
	case !t.AmountAllowed(amount):
		b.showPay(fmt.Sprintf("Сумма больше лимита (%s). Поднимите TRANSFER_MAX_AMOUNT, если нужно.\n%s",
			esc(moneyShort(t.MaxAmount)), b.askAmountText()), nil)
		return
	}

	b.payMu.Lock()
	if b.pending != nil {
		b.payMu.Unlock()
		b.notify("Один перевод уже идёт. Закончите его или отправьте /cancel.")
		return
	}
	b.awaitAmount = false
	b.payMu.Unlock()

	b.showPay(fmt.Sprintf("⏳ Готовлю перевод <b>%s</b> себе (%s, номер тел. %s)…",
		esc(bank.Money(amount, "RUB")), esc(t.Bank), esc(maskPhone(t.Phone))), nil)

	go b.preparePay(amount)
}

func (b *Bot) preparePay(amount float64) {
	t := b.d.Cfg.Transfer
	tr := bank.NewTransfer(b.d.Browser, b.d.Steps, t.Armed)

	req := bank.TransferRequest{
		Amount:  amount,
		Phone:   t.Phone,
		Bank:    t.Bank,
		Comment: t.Comment,
	}
	if err := tr.Prepare(req); err != nil {
		b.finishPay(b.withSnapshot("❌ Не удалось подготовить перевод:\n" + esc(err.Error())))
		return
	}

	if !t.Armed {
		// Кнопку не жмём, но проверяем, что она найдена и активна: иначе её
		// селектор впервые проверится на боевом переводе.
		check := "✅ Кнопка «Продолжить» на месте — не нажата."
		if err := tr.CheckSubmit(); err != nil {
			check = "⚠️ " + esc(err.Error())
		}

		b.finishPay(fmt.Sprintf("🧪 <b>Черновой режим</b>\n\nФорма перевода <b>%s</b> заполнена — "+
			"посмотрите окно браузера. Ничего не отправлено.\n\n%s\n\n"+
			"Когда заполнение выверено, включите TRANSFER_ARMED=true для реальной отправки.",
			esc(bank.Money(amount, "RUB")), check))
		return
	}

	// «Продолжить» — ещё не деньги: банк показывает экран сверки и ждёт
	// финального нажатия.
	if err := tr.Submit(); err != nil {
		b.finishPay(b.withSnapshot("❌ Не удалось перейти к подтверждению:\n" + esc(err.Error())))
		return
	}

	b.payMu.Lock()
	b.pending = &pendingPay{
		amount:   amount,
		transfer: tr,
		deadline: time.Now().Add(t.OTPWait),
		stage:    stageApproval,
	}
	b.payMu.Unlock()

	// Через OTPWait незавершённый перевод забывается, чтобы вчерашнее «да» не
	// прилетело в сегодняшнюю операцию.
	go b.expirePay(t.OTPWait)

	b.showPay(b.payRequestText(amount), payRequestMarkup())
}

// payRequestText — карточка запроса на перевод. Показывается, когда банк уже
// открыл экран сверки: остаётся одно нажатие, и оно за человеком.
func (b *Bot) payRequestText(amount float64) string {
	t := b.d.Cfg.Transfer
	return fmt.Sprintf(
		"🔎 <b>Запрос на перевод:</b> %s (%s, номер тел. %s)\n\n"+
			"<blockquote><b><i>Подтвердите перевод кнопкой ниже.</i></b>\n"+
			"<i>Банк проводит этот перевод без кода из SMS — деньги уйдут сразу.</i></blockquote>",
		esc(bank.Money(amount, "RUB")), esc(t.Bank), esc(maskPhone(t.Phone)))
}

func payRequestMarkup() *tele.ReplyMarkup {
	m := &tele.ReplyMarkup{}
	m.Inline(m.Row(
		m.Data("✅ Подтвердить", btnPayConfirm.Unique),
		m.Data("✖️ Отменить", btnPayCancel.Unique),
	))
	return m
}

// onText ведёт диалог по переводу: сумма после кнопки меню и код из SMS, если
// банк его спросит. Вне перевода молчит, чтобы не реагировать на случайные
// сообщения.
//
// Подтверждения тут нет: финальное нажатие подтверждается только кнопкой под
// запросом. Слово «да», набранное в чате, слишком легко отправить не глядя.
func (b *Bot) onText(c tele.Context) error {
	b.payMu.Lock()
	p := b.pending
	waiting := b.awaitAmount
	b.payMu.Unlock()

	text := strings.TrimSpace(c.Text())

	if p == nil {
		if !waiting {
			return nil
		}
		if msg := b.payUnavailable(); msg != "" {
			b.payMu.Lock()
			b.awaitAmount = false
			b.payMu.Unlock()
			return b.showPay(msg, nil)
		}
		b.startPay(text)
		return nil
	}

	switch p.stage {
	case stageApproval:
		// Карточку не трогаем: под ней живые кнопки, и переписать её сейчас
		// значило бы отобрать у человека способ ответить.
		return c.Send("Перевод ждёт подтверждения — нажмите <b>✅ Подтвердить</b> " +
			"в карточке выше или отправьте /cancel.")

	case stageCode:
		if !isOTP(text) {
			return c.Send("Это не похоже на код из SMS (нужны только цифры). " +
				"Пришлите код или отправьте /cancel.")
		}
		b.showPay(b.payRequestText(p.amount)+"\n\n⏳ Подтверждаю перевод…", nil)
		go b.confirmPay(p, text)
	}
	return nil
}

// sendPay жмёт финальную кнопку. Дальше одно из двух: банк просит код (тогда
// ждём его) или сразу показывает исход.
func (b *Bot) sendPay(p *pendingPay) {
	if err := p.transfer.Send(); err != nil {
		b.clearPending()
		b.finishPay(b.withSnapshot("❌ Не удалось нажать финальную кнопку:\n" + esc(err.Error()) +
			"\n\n<i>Проверьте окно браузера: деньги, скорее всего, на месте, но убедитесь.</i>"))
		return
	}

	// Кнопка нажата — с этого момента исход решает банк, а не бот.
	if p.transfer.CodeRequested(15 * time.Second) {
		b.payMu.Lock()
		if b.pending == p {
			p.stage = stageCode
			p.deadline = time.Now().Add(b.d.Cfg.Transfer.OTPWait)
		}
		b.payMu.Unlock()
		go b.expirePay(b.d.Cfg.Transfer.OTPWait)

		b.showPay(b.payRequestText(p.amount)+
			"\n\n🔐 Банк запросил код из SMS. Пришлите его одним сообщением.\n"+
			"<i>Отменить — /cancel.</i>", nil)
		return
	}

	b.clearPending()
	b.reportOutcome(p)
}

func (b *Bot) confirmPay(p *pendingPay, code string) {
	defer b.clearPending()

	if err := p.transfer.EnterCode(code); err != nil {
		b.finishPay(b.withSnapshot("❌ Перевод не прошёл:\n" + esc(err.Error()) +
			"\n\n<i>Проверьте окно браузера — деньги могли и уйти, и нет.</i>"))
		return
	}
	b.reportOutcome(p)
}

// reportOutcome читает со страницы, чем всё кончилось, и дописывает итог в ту
// же карточку.
func (b *Bot) reportOutcome(p *pendingPay) {
	ok, err := p.transfer.Outcome()
	switch {
	case err != nil:
		b.finishPay(b.withSnapshot("⚠️ " + esc(err.Error()) +
			"\n\n<i>Финальная кнопка нажата — проверьте окно браузера и историю.</i>"))
	case ok:
		b.finishPay(fmt.Sprintf("✅ Перевод <b>%s</b> отправлен (%s, номер тел. %s).",
			esc(bank.Money(p.amount, "RUB")),
			esc(b.d.Cfg.Transfer.Bank), esc(maskPhone(b.d.Cfg.Transfer.Phone))))
	default:
		b.finishPay("Готово, но подтверждения от банка не видно. Проверьте окно браузера.")
	}
}

// withSnapshot дописывает к сообщению снимок страницы: по нему видно, на каком
// экране застряли и как называются его элементы. Иначе диагностика вслепую.
func (b *Bot) withSnapshot(msg string) string {
	snap, err := b.d.Browser.Inspect()
	if err != nil {
		return msg
	}
	log.Print(formatSnapshotPlain(snap))
	return msg + "\n\n" + formatSnapshotHTML(snap)
}

// cmdInspect снимает поля и кнопки текущей страницы браузера.
//
// Нужен для настройки transfer.json. Работает в том же окне, где сделан вход,
// поэтому видит залогиненную форму — отдельной утилитой это не снять. Полный
// список печатается в терминал (оттуда удобно копировать), в чат — сводка.
func (b *Bot) cmdInspect(c tele.Context) error {
	snap, err := b.d.Browser.Inspect()
	if err != nil {
		return c.Send("Не удалось прочитать страницу: " + esc(err.Error()))
	}

	// Полный список — в терминал бота.
	log.Print(formatSnapshotPlain(snap))

	// В чат — то же, но компактно и с разметкой.
	return c.Send(formatSnapshotHTML(snap))
}

func formatSnapshotPlain(s bank.PageSnapshot) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n═══ поля страницы %s ═══\n", s.URL)
	fmt.Fprintf(&sb, "ПОЛЯ ВВОДА (%d):\n", len(s.Inputs))
	for i, in := range s.Inputs {
		fmt.Fprintf(&sb, "  %d. [%s] %s\n", i+1, in.Type, inputSelectors(in))
	}
	fmt.Fprintf(&sb, "КНОПКИ (%d):\n", len(s.Buttons))
	for _, btn := range s.Buttons {
		extra := ""
		if btn.Testid != "" {
			extra = fmt.Sprintf("  testid=%q (%s)", btn.Testid, btn.TestidAttr)
		}
		fmt.Fprintf(&sb, "  • %q%s\n", btn.Text, extra)
	}
	return sb.String()
}

func formatSnapshotHTML(s bank.PageSnapshot) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "🔎 <b>Страница</b> <code>%s</code>\n", esc(s.URL))

	fmt.Fprintf(&sb, "\n<b>Поля (%d):</b>\n", len(s.Inputs))
	if len(s.Inputs) == 0 {
		sb.WriteString("<i>полей нет</i>\n")
	}
	for i, in := range s.Inputs {
		fmt.Fprintf(&sb, "%d. <code>%s</code>\n", i+1, esc(inputSelectors(in)))
	}

	fmt.Fprintf(&sb, "\n<b>Кнопки (%d):</b>\n", len(s.Buttons))
	// Кнопок бывает много (список банков) — в чат показываем часть, полный
	// список остаётся в терминале.
	shown := s.Buttons
	if len(shown) > 25 {
		shown = shown[:25]
	}
	for _, btn := range shown {
		fmt.Fprintf(&sb, "• %s\n", esc(btn.Text))
	}
	if len(s.Buttons) > len(shown) {
		fmt.Fprintf(&sb, "<i>…ещё %d — весь список в терминале бота</i>\n", len(s.Buttons)-len(shown))
	}
	return sb.String()
}

func inputSelectors(in bank.InputField) string {
	var by []string
	if in.Placeholder != "" {
		by = append(by, fmt.Sprintf("placeholder=%q", in.Placeholder))
	}
	if in.Aria != "" {
		by = append(by, fmt.Sprintf("label=%q", in.Aria))
	}
	if in.Name != "" {
		by = append(by, fmt.Sprintf("name=%q", in.Name))
	}
	if in.Testid != "" {
		// Атрибут показывается рядом: написаний несколько, и в шаге важно
		// знать, каким подписан именно этот элемент.
		by = append(by, fmt.Sprintf("testid=%q (%s)", in.Testid, in.TestidAttr))
	}
	if in.Filled {
		by = append(by, "[уже заполнено]")
	}
	if in.Hidden {
		// Важная деталь для настройки: по такому полю ввод работает, но ждать
		// его «видимости» бессмысленно — и глазами на странице его не найти.
		by = append(by, "[скрыто, ввод через fill работает]")
	}
	if len(by) == 0 {
		return "(без опознавательных атрибутов)"
	}
	return strings.Join(by, "  ")
}

// onPayConfirm — единственный способ дать согласие на необратимый шаг.
func (b *Bot) onPayConfirm(c tele.Context) error {
	if err := c.Respond(); err != nil {
		log.Printf("telegram: %v", err)
	}

	b.payMu.Lock()
	p := b.pending
	b.payMu.Unlock()

	if p == nil || p.stage != stageApproval {
		return b.finishPay("<i>Запрос уже неактуален — подтверждать нечего.</i>")
	}

	// Кнопки уходят вместе с текстом: второе нажатие не должно попасть в тот
	// же перевод.
	b.showPay(b.payRequestText(p.amount)+"\n\n⏳ Нажимаю «Перевести»…", nil)
	go b.sendPay(p)
	return nil
}

func (b *Bot) onPayCancel(c tele.Context) error {
	if err := c.Respond(); err != nil {
		log.Printf("telegram: %v", err)
	}
	return b.cancelPay()
}

func (b *Bot) cmdCancel(c tele.Context) error {
	return b.cancelPay()
}

// cancelPay снимает идущий перевод и честно называет его состояние.
func (b *Bot) cancelPay() error {
	b.payMu.Lock()
	p := b.pending
	waiting := b.awaitAmount
	b.pending = nil
	b.awaitAmount = false
	b.payMu.Unlock()

	switch {
	case p == nil && waiting:
		return b.finishPay("Хорошо, суммы не жду.")
	case p == nil:
		// Карточку не трогаем: в ней итог прошлого перевода, и затирать его
		// ответом на пустую отмену — терять запись о деньгах.
		return b.notify("Отменять нечего.")
	case p.stage == stageCode:
		// Здесь финальная кнопка уже нажата: бот всего лишь перестаёт ждать
		// код. Умолчать об этом — значит соврать про судьбу денег.
		return b.finishPay("✖️ Код вводить не буду. Но «Перевести» уже нажато — " +
			"проверьте окно браузера и историю.")
	default:
		return b.finishPay("✖️ Перевод отменён — «Перевести» не нажималось, деньги на месте.\n" +
			"<i>Форма в браузере осталась на экране сверки.</i>")
	}
}

func (b *Bot) expirePay(after time.Duration) {
	time.Sleep(after)
	b.payMu.Lock()
	p := b.pending
	if p == nil || time.Now().Before(p.deadline) {
		// Либо перевод уже завершён, либо срок продлили на следующем шаге —
		// тогда его закроет более поздний таймер.
		b.payMu.Unlock()
		return
	}
	stage := p.stage
	b.pending = nil
	b.payMu.Unlock()

	if stage == stageCode {
		b.finishPay("⌛️ Время на ввод кода вышло. «Перевести» было нажато — " +
			"проверьте окно браузера и историю.")
		return
	}
	b.finishPay("⌛️ Подтверждения не было, перевод забыт — «Перевести» не нажималось.\n" +
		"<i>Начать заново — /pay.</i>")
}

func (b *Bot) clearPending() {
	b.payMu.Lock()
	b.pending = nil
	b.payMu.Unlock()
}

// isOTP — код банка это короткая цифровая строка.
func isOTP(s string) bool {
	if len(s) < 4 || len(s) > 8 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// humanDuration печатает длительность по-человечески: «2 ч 15 мин», а не
// «2h15m3.4s». Секунды показываются только в первую минуту, дальше они шум.
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d сек", int(d.Seconds()))
	}

	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60

	switch {
	case days > 0:
		return fmt.Sprintf("%d дн %d ч", days, hours)
	case hours > 0:
		return fmt.Sprintf("%d ч %d мин", hours, mins)
	default:
		return fmt.Sprintf("%d мин", mins)
	}
}

// moneyShort — сумма без копеек, когда их нет: в разговоре о границах «10 ₽»
// читается лучше, чем «10,00 ₽».
func moneyShort(v float64) string {
	return strings.Replace(bank.Money(v, "RUB"), ",00 ", " ", 1)
}

// payLine показывает строку про перевод в меню только когда он настроен —
// иначе не дразнить командой, которая всё равно ответит «не настроено».
func payLine(enabled bool) string {
	if enabled {
		return "💸 /pay 5000 — перевести себе в другой банк"
	}
	return "<i>💸 перевод себе — выключен (см. .env)</i>"
}

// maskPhone прячет середину номера: +7 ••• ••• 45 67.
func maskPhone(p string) string {
	if len(p) < 4 {
		return p
	}
	return "••• " + p[len(p)-4:]
}

// reply показывает содержимое в панели — единственном сообщении, которое
// бот перерисовывает.
//
// Так чат не забивается: и кнопки, и текстовые команды обновляют одно и то же
// сообщение. Новые сообщения остаются только за тем, что действительно должно
// попасться на глаза, — за поступлениями и предупреждениями.
func (b *Bot) reply(c tele.Context, text string, markup *tele.ReplyMarkup) error {
	// Нажатие кнопки правит ровно то сообщение, под которым она была.
	if cb := c.Callback(); cb != nil {
		if err := c.Respond(); err != nil {
			log.Printf("telegram: %v", err)
		}
		if cb.Message != nil {
			b.setPanel(cb.Message.ID)
		}
		if err := c.Edit(text, markup); err != nil && !notModified(err) {
			return err
		}
		return nil
	}

	// Команда: правим панель на месте, пока она остаётся последней в чате.
	id, stale := b.panelState()
	if id != 0 && !stale {
		_, err := b.b.Edit(b.storedMsg(id), text, markup)
		if err == nil || notModified(err) {
			return nil
		}
		// Панель могли удалить или она слишком старая для правки.
		log.Printf("панель недоступна, создаю новую: %v", err)
	}

	// Панель уехала вверх — убираем её, чтобы в чате не осталось двух наборов
	// кнопок, и рисуем новую внизу.
	if id != 0 {
		if err := b.b.Delete(b.storedMsg(id)); err != nil {
			log.Printf("не удалось убрать прошлую панель: %v", err)
		}
	}
	return b.send(text, markup)
}

// send отправляет новое сообщение и делает его панелью.
func (b *Bot) send(text string, markup *tele.ReplyMarkup) error {
	sent, err := b.b.Send(b.owner, text, markup)
	if err != nil {
		return err
	}
	b.setPanel(sent.ID)
	return nil
}

// notify шлёт сообщение, которое панелью не становится: без кнопок и без
// притязаний на место внизу чата.
func (b *Bot) notify(text string) error {
	if _, err := b.b.Send(b.owner, text); err != nil {
		return err
	}
	b.markStale()
	return nil
}

func notLoggedIn() string {
	return "❌ <b>Я не вижу входа в банк</b>\n\n" +
		"Зайдите в кабинет в открытом окне браузера — я замечу это сам " +
		"и сразу сообщу."
}

// explain переводит ошибку в понятный владельцу текст.
func (b *Bot) explain(err error) string {
	if bank.IsAuthError(err) {
		return notLoggedIn()
	}
	return "Не получилось: " + esc(err.Error())
}

func esc(s string) string { return html.EscapeString(s) }

func orDash(s string) string {
	if s == "" {
		return "не выбран"
	}
	return s
}
