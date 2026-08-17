package bank

import (
	"slices"
	"testing"

	"github.com/valerakrut/parserbank/internal/config"
)

func TestBrowserArgs(t *testing.T) {
	local := browserArgs(&config.Config{Runtime: config.RuntimeLocal})
	vps := browserArgs(&config.Config{Runtime: config.RuntimeVPS})

	// Эти два нужны везде: без первого банк видит автоматизацию, без второго
	// вкладка падает на маленьком /dev/shm — вместе с сессией.
	for _, must := range []string{
		"--disable-blink-features=AutomationControlled",
		"--disable-dev-shm-usage",
	} {
		if !slices.Contains(local, must) {
			t.Errorf("локально нет флага %s", must)
		}
		if !slices.Contains(vps, must) {
			t.Errorf("на VPS нет флага %s", must)
		}
	}

	// Экономия памяти — только на сервере: локально она стоит отзывчивости.
	if slices.Contains(local, "--renderer-process-limit=1") {
		t.Error("серверные ограничения просочились в локальный режим")
	}
	if !slices.Contains(vps, "--renderer-process-limit=1") {
		t.Error("на VPS не ограничено число процессов рендерера")
	}
}

func TestAliveForWithoutLogin(t *testing.T) {
	// Пока входа нет, возраст сессии — ноль, а не время с запуска.
	if d := (&Session{}).AliveFor(); d != 0 {
		t.Errorf("без входа AliveFor = %s", d)
	}
}
