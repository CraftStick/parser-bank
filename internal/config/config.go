// Package config собирает настройки бота из окружения и файла .env.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Runtime определяет, где крутится бот. От этого зависят дефолты запуска
// браузера: на макбуке окно видно и логиниться можно руками, на VPS окна нет.
type Runtime string

// SessionKeepAlive — предельный интервал опроса, при котором сессия ещё жива.
//
// ВТБ разлогинивает после 3–5 минут бездействия. Две минуты дают запас даже к
// нижней границе: пропущенный из-за сетевой ошибки опрос не будет стоить входа.
const SessionKeepAlive = 2 * time.Minute

const (
	RuntimeLocal Runtime = "local"
	RuntimeVPS   Runtime = "vps"
)

type Config struct {
	// Telegram
	TGToken string
	OwnerID int64

	// Банк. BankHost пуст, если включён автоподбор зеркала — тогда домен
	// выбирается на старте.
	BankHost   string
	AutoMirror bool
	Mirrors    []string

	// Запуск
	Runtime    Runtime
	Headless   bool
	ProfileDir string
	Proxy      string
	Locale     string
	Timezone   string

	// BrowserChannel — использовать установленный в системе браузер
	// ("chrome", "msedge") вместо скачиваемого Chromium. Пусто — Chromium.
	BrowserChannel string

	// Данные
	DBPath        string
	EndpointsPath string
	RecordDir     string

	// Поведение
	PollInterval time.Duration
	HistoryLimit int

	// NotifyMaxAge — предельный возраст зачисления, о котором ещё стоит
	// уведомлять. Защищает от разбора всей истории после простоя.
	NotifyMaxAge time.Duration

	// Перевод себе. Выключен, пока не задан получатель.
	Transfer TransferConfig
}

// TransferConfig описывает перевод себе в другой банк.
//
// Получатель зафиксирован здесь, а не берётся из команды: это защита от того,
// что доступ к боту или к чату кто-то получит. Из чата можно управлять только
// суммой, и та ограничена лимитом; отправить деньги на чужой счёт нельзя в
// принципе — реквизиты жёстко прописаны в этом файле.
type TransferConfig struct {
	Enabled   bool
	StepsPath string // JSON с шагами заполнения формы

	Phone     string  // номер получателя (свой), в формате +7XXXXXXXXXX
	Bank      string  // банк-получатель, как он назван в форме СБП
	Comment   string  // необязательное сообщение получателю
	MinAmount float64 // нижняя граница перевода по СБП
	MaxAmount float64 // потолок одного перевода
	OTPWait   time.Duration

	// Armed включает реальную отправку. Пока false, бот заполняет форму и
	// останавливается перед подтверждением — это безопасный режим отладки:
	// видно, что бот ввёл, но ничего не уходит и SMS не приходит.
	Armed bool
}

// Load читает .env (если есть) и переменные окружения. Переменные окружения
// приоритетнее файла, так что на VPS можно ничего не менять в .env.
func Load() (*Config, error) {
	loadDotEnv(".env")

	rt := Runtime(strings.ToLower(env("RUNTIME", string(RuntimeLocal))))
	if rt != RuntimeLocal && rt != RuntimeVPS {
		return nil, fmt.Errorf("RUNTIME=%q: допустимы только %q или %q", rt, RuntimeLocal, RuntimeVPS)
	}

	cfg := &Config{
		TGToken:        env("TG_BOT_TOKEN", ""),
		Runtime:        rt,
		ProfileDir:     env("PROFILE_DIR", "./data/profile"),
		Proxy:          env("PROXY", ""),
		Locale:         env("LOCALE", "ru-RU"),
		Timezone:       env("TIMEZONE", "Europe/Moscow"),
		BrowserChannel: env("BROWSER_CHANNEL", ""),
		DBPath:         env("DB_PATH", "./data/parserbank.db"),
		EndpointsPath:  env("ENDPOINTS_PATH", "./endpoints.json"),
		RecordDir:      env("RECORD_DIR", "./data/discovery"),
	}

	// BANK_HOST=auto — подобрать живое зеркало самостоятельно. Иначе домен
	// фиксированный, и при его смерти бот будет честно ругаться, а не молча
	// уезжать на другой домен.
	if host := env("BANK_HOST", "online.sbpvtb.ru"); strings.EqualFold(host, "auto") {
		cfg.AutoMirror = true
	} else {
		cfg.BankHost = host
	}
	cfg.Mirrors = splitList(env("BANK_MIRRORS", ""))

	// На локальной машине окно браузера по умолчанию видимое: так проще
	// логиниться и видеть, что происходит. На VPS окна нет — но headless
	// антифрод замечает, поэтому там правильнее headful под Xvfb (см. README),
	// и дефолт остаётся видимым. Явный HEADLESS перекрывает оба случая.
	cfg.Headless = envBool("HEADLESS", false)

	var err error
	if cfg.OwnerID, err = envInt64("TG_OWNER_ID", 0); err != nil {
		return nil, err
	}
	if cfg.PollInterval, err = envDuration("POLL_INTERVAL", 60*time.Second); err != nil {
		return nil, err
	}
	if cfg.PollInterval < 15*time.Second {
		return nil, fmt.Errorf("POLL_INTERVAL=%s: слишком часто, банк это заметит; минимум 15s", cfg.PollInterval)
	}
	// Верхняя граница жёстче нижней, и она не про вежливость.
	//
	// ВТБ сбрасывает сессию после 3–5 минут бездействия. Опрос — единственное,
	// что банк видит как активность: реже двух минут, и бот сам себя лишит
	// входа, а восстанавливать его придётся руками в окне браузера.
	if cfg.PollInterval > SessionKeepAlive {
		return nil, fmt.Errorf(
			"POLL_INTERVAL=%s: реже %s нельзя — ВТБ сбрасывает сессию после 3–5 минут "+
				"бездействия, и опрос это единственное, что её держит",
			cfg.PollInterval, SessionKeepAlive)
	}
	if cfg.NotifyMaxAge, err = envDuration("NOTIFY_MAX_AGE", 24*time.Hour); err != nil {
		return nil, err
	}
	if err := cfg.loadTransfer(); err != nil {
		return nil, err
	}
	// Хранилище давно не только про дедупликацию: по нему листается история,
	// и держать десяток записей больше незачем — база всё равно крошечная.
	if cfg.HistoryLimit, err = envInt("HISTORY_LIMIT", 1000); err != nil {
		return nil, err
	}
	if cfg.HistoryLimit < 1 {
		return nil, fmt.Errorf("HISTORY_LIMIT должен быть больше нуля")
	}

	return cfg, nil
}

// RequireBot проверяет то, без чего бот не поднимется. Recorder-у эти поля
// не нужны, поэтому проверка вынесена из Load.
func (c *Config) RequireBot() error {
	if c.TGToken == "" {
		return fmt.Errorf("не задан TG_BOT_TOKEN")
	}
	if c.OwnerID == 0 {
		return fmt.Errorf("не задан TG_OWNER_ID — без него бот отвечал бы кому угодно")
	}
	return nil
}

// loadTransfer читает настройки перевода. Перевод считается включённым только
// когда заданы и получатель, и файл шагов, — иначе команда /pay недоступна.
func (c *Config) loadTransfer() error {
	t := TransferConfig{
		StepsPath: env("TRANSFER_STEPS_PATH", "./transfer.json"),
		Phone:     env("TRANSFER_PHONE", ""),
		Bank:      env("TRANSFER_BANK", ""),
		Comment:   env("TRANSFER_COMMENT", ""),
		Armed:     envBool("TRANSFER_ARMED", false),
	}

	var err error
	// Нижнюю границу задаёт банк: перевод по СБП меньше десяти рублей он не
	// проведёт. Лучше сказать это в чате, чем упереться в форму.
	if t.MinAmount, err = envFloat("TRANSFER_MIN_AMOUNT", 10); err != nil {
		return err
	}
	if t.MaxAmount, err = envFloat("TRANSFER_MAX_AMOUNT", 0); err != nil {
		return err
	}
	if t.OTPWait, err = envDuration("TRANSFER_OTP_WAIT", 3*time.Minute); err != nil {
		return err
	}

	// Лимит необязателен: получатель всё равно зафиксирован, деньги уходят
	// только на свой счёт, а сумму видно в SMS перед вводом кода.
	// MaxAmount <= 0 означает «без ограничения».
	t.Enabled = t.Phone != "" && t.Bank != ""

	c.Transfer = t
	return nil
}

// AmountAllowed проверяет сумму против лимита. Лимит 0 — без ограничения.
func (t TransferConfig) AmountAllowed(amount float64) bool {
	return t.MaxAmount <= 0 || amount <= t.MaxAmount
}

// AmountTooSmall — сумма ниже той, что примет банк.
func (t TransferConfig) AmountTooSmall(amount float64) bool {
	return t.MinAmount > 0 && amount < t.MinAmount
}

func envFloat(key string, def float64) (float64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: не число", key, v)
	}
	return f, nil
}

// splitList разбирает список через запятую.
func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: не число", key, v)
	}
	return n, nil
}

func envInt64(key string, def int64) (int64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: не число", key, v)
	}
	return n, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: ожидается длительность вида 45s, 2m", key, v)
	}
	return d, nil
}

// loadDotEnv — минималистичный парсер .env. Не перетирает уже заданные
// переменные окружения.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		// Пустое значение везде означает «не настроено», а не «настроено в
		// пустоту» — так же, как в env(). Без этого строка вида KEY= занимала
		// бы ключ и глушила значение, заданное ниже по файлу или в окружении.
		if val == "" {
			continue
		}
		if v, exists := os.LookupEnv(key); !exists || v == "" {
			os.Setenv(key, val)
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "предупреждение: %s прочитан не полностью: %v\n", path, err)
	}
}
