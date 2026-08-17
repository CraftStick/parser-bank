package bank

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Direction — направление операции.
type Direction string

const (
	DirectionIn      Direction = "in"      // зачисление
	DirectionOut     Direction = "out"     // списание
	DirectionUnknown Direction = "unknown" // не удалось определить
)

// Account — счёт или карта.
type Account struct {
	ID       string
	Name     string
	Number   string // как правило маскированный: •• 4321
	Balance  float64
	Currency string
	IsCard   bool
}

// Last4 возвращает последние четыре цифры номера в виде «•• 4321».
// Пусто, если цифр в номере меньше четырёх.
func (a Account) Last4() string {
	var digits []rune
	for _, r := range a.Number {
		if r >= '0' && r <= '9' {
			digits = append(digits, r)
		}
	}
	if len(digits) < 4 {
		return ""
	}
	return "•• " + string(digits[len(digits)-4:])
}

func (a Account) String() string {
	title := a.Name
	if title == "" {
		title = a.ID
	}
	if a.Number != "" {
		title = fmt.Sprintf("%s (%s)", title, a.Number)
	}
	return fmt.Sprintf("%s — %s", title, Money(a.Balance, a.Currency))
}

// Transaction — операция по счёту.
type Transaction struct {
	ID        string // устойчивый ключ, см. Fingerprint
	Ref       string // идентификатор из ответа банка, справочно
	Time      time.Time
	Amount    float64 // всегда положительное; направление в Direction
	Currency  string
	Direction Direction
	Title     string // назначение/контрагент
	Card      string // маскированная карта или счёт, по которому прошла операция
	Raw       string // исходный JSON — на случай, если понадобится поле, которого нет в маппинге
}

// Fingerprint — ключ, по которому операция узнаётся между опросами.
//
// Считается по содержимому, а не по идентификатору из ответа, потому что
// доверять последнему нельзя: ВТБ выдаёт в поле operationId новый UUID при
// каждом запросе. С таким ключом каждая операция выглядела бы новой, и бот
// присылал бы уведомление об одном и том же поступлении раз в минуту, вечно.
//
// Плата — две буквально одинаковые операции (та же секунда, сумма,
// контрагент и карта) сольются в одну. Это редкость, и она безобиднее
// бесконечных повторов; к тому же в пределах одного ответа такие операции
// разводятся порядковым номером.
func (t Transaction) Fingerprint() string {
	h := sha256.New()
	fmt.Fprintf(h, "%d|%.2f|%s|%s|%s|%s",
		t.Time.Unix(), t.Amount, t.Currency, t.Direction, t.Title, t.Card)
	return hex.EncodeToString(h.Sum(nil))[:24]
}

// Money форматирует сумму по-человечески: 12 345,67 ₽
func Money(v float64, currency string) string {
	s := fmt.Sprintf("%.2f", v)
	intPart, frac, _ := strings.Cut(s, ".")

	neg := strings.HasPrefix(intPart, "-")
	intPart = strings.TrimPrefix(intPart, "-")

	var b strings.Builder
	for i, d := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(d)
	}

	out := b.String() + "," + frac
	if neg {
		out = "-" + out
	}
	return out + " " + CurrencySymbol(currency)
}

// CurrencySymbol приводит код валюты к символу, если он известен.
func CurrencySymbol(code string) string {
	switch strings.ToUpper(code) {
	case "RUB", "RUR", "643", "":
		return "₽"
	case "USD", "840":
		return "$"
	case "EUR", "978":
		return "€"
	case "CNY", "156":
		return "¥"
	default:
		return strings.ToUpper(code)
	}
}
