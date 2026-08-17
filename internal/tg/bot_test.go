package tg

import (
	"testing"
	"time"
)

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		30 * time.Second:             "30 сек",
		90 * time.Second:             "1 мин",
		45 * time.Minute:             "45 мин",
		2*time.Hour + 15*time.Minute: "2 ч 15 мин",
		26 * time.Hour:               "1 дн 2 ч",
		3*24*time.Hour + 4*time.Hour: "3 дн 4 ч",
	}
	for in, want := range cases {
		if got := humanDuration(in); got != want {
			t.Errorf("humanDuration(%s) = %q, ожидалось %q", in, got, want)
		}
	}
}

func TestMoneyShort(t *testing.T) {
	// Границы читаются без копеек, но не ценой потери самих копеек.
	if got := moneyShort(10); got != "10 ₽" {
		t.Errorf("moneyShort(10) = %q", got)
	}
	if got := moneyShort(1500.5); got != "1 500,50 ₽" {
		t.Errorf("moneyShort(1500.5) = %q", got)
	}
}
