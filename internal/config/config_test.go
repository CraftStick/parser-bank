package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withDotEnv кладёт .env в рабочий каталог теста и подчищает окружение.
func withDotEnv(t *testing.T, content string) {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600); err != nil {
		t.Fatalf("записать .env: %v", err)
	}
	t.Chdir(dir)
}

func TestLoadDotEnvIgnoresEmptyValues(t *testing.T) {
	// Пустая строка не должна занимать ключ: иначе значение, дописанное ниже,
	// молча теряется, а бот падает с «не задан TG_BOT_TOKEN».
	withDotEnv(t, "TG_BOT_TOKEN=\nTG_BOT_TOKEN=настоящий-токен\n")
	t.Setenv("TG_BOT_TOKEN", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TGToken != "настоящий-токен" {
		t.Errorf("TGToken = %q, ожидался настоящий-токен", cfg.TGToken)
	}
}

func TestEnvBeatsDotEnv(t *testing.T) {
	withDotEnv(t, "BANK_HOST=online.из-файла.ru\n")
	t.Setenv("BANK_HOST", "online.из-окружения.ru")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BankHost != "online.из-окружения.ru" {
		t.Errorf("BankHost = %q — окружение должно быть приоритетнее файла", cfg.BankHost)
	}
}

func TestAutoMirror(t *testing.T) {
	withDotEnv(t, "")
	t.Setenv("BANK_HOST", "auto")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.AutoMirror {
		t.Error("BANK_HOST=auto не включил автоподбор зеркала")
	}
	if cfg.BankHost != "" {
		t.Errorf("BankHost = %q, при auto он должен быть пустым", cfg.BankHost)
	}
}

func TestMirrorsList(t *testing.T) {
	withDotEnv(t, "")
	t.Setenv("BANK_MIRRORS", "online.a.ru, online.b.ru ,, online.c.ru")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"online.a.ru", "online.b.ru", "online.c.ru"}
	if len(cfg.Mirrors) != len(want) {
		t.Fatalf("Mirrors = %v, ожидалось %v", cfg.Mirrors, want)
	}
	for i := range want {
		if cfg.Mirrors[i] != want[i] {
			t.Errorf("Mirrors[%d] = %q, ожидалось %q", i, cfg.Mirrors[i], want[i])
		}
	}
}

func TestPollIntervalFloor(t *testing.T) {
	// Слишком частый опрос — прямая дорога к вниманию антифрода,
	// поэтому он отсекается на уровне конфига.
	withDotEnv(t, "")
	t.Setenv("POLL_INTERVAL", "3s")

	if _, err := Load(); err == nil {
		t.Error("POLL_INTERVAL=3s должен быть отвергнут")
	}
}

func TestRequireBot(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"всё есть", Config{TGToken: "t", OwnerID: 1}, false},
		{"нет токена", Config{OwnerID: 1}, true},
		{"нет владельца", Config{TGToken: "t"}, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.cfg.RequireBot(); (err != nil) != c.wantErr {
				t.Errorf("RequireBot() = %v, ожидалась ошибка: %v", err, c.wantErr)
			}
		})
	}
}

func TestPollIntervalKeepsSessionAlive(t *testing.T) {
	// Слишком редкий опрос — не «экономия», а потерянный вход: банк
	// разлогинивает после 3–5 минут бездействия.
	t.Setenv("POLL_INTERVAL", "5m")
	t.Setenv("TG_TOKEN", "x")
	t.Setenv("TG_OWNER_ID", "1")

	_, err := Load()
	if err == nil {
		t.Fatal("пятиминутный опрос принят, хотя сессия столько не живёт")
	}
	if !strings.Contains(err.Error(), "POLL_INTERVAL") {
		t.Errorf("ошибка не про интервал опроса: %v", err)
	}
}
