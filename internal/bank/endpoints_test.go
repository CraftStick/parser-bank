package bank

import (
	"errors"
	"testing"
	"time"
)

func TestIsBrowserClosed(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("playwright: target closed: Target page, context or browser has been closed"), true},
		{errors.New("could not send message: transport closed"), true},
		{errors.New("browser has been closed"), true},
		{errors.New("dial tcp: connection refused"), false},
		{ErrNotAuthenticated, false},
	}

	for _, c := range cases {
		if got := IsBrowserClosed(c.err); got != c.want {
			t.Errorf("IsBrowserClosed(%v) = %v, ожидалось %v", c.err, got, c.want)
		}
	}
}

func TestParseOffset(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"-30d", -30 * 24 * time.Hour, false},
		{"0d", 0, false},
		{"-12h", -12 * time.Hour, false},
		{"+45m", 45 * time.Minute, false},
		{" -1d ", -24 * time.Hour, false},
		{"", 0, true},
		{"30", 0, true},
		{"-30y", 0, true},
		{"вчера", 0, true},
	}

	for _, c := range cases {
		got, err := parseOffset(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseOffset(%q): ошибка = %v, ожидалась: %v", c.in, err, c.wantErr)
			continue
		}
		if err == nil && got != c.want {
			t.Errorf("parseOffset(%q) = %v, ожидалось %v", c.in, got, c.want)
		}
	}
}

func TestTimeParamRender(t *testing.T) {
	now := time.Date(2026, 8, 13, 22, 30, 0, 0, time.UTC)

	cases := []struct {
		name   string
		param  TimeParam
		want   string
		hasErr bool
	}{
		{
			name:  "месяц назад в формате банка",
			param: TimeParam{Offset: "-30d", Layout: "2006-01-02T15:04:05.000-07:00"},
			want:  "2026-07-14T22:30:00.000+00:00",
		},
		{
			name:  "текущий момент",
			param: TimeParam{Offset: "0d", Layout: "2006-01-02"},
			want:  "2026-08-13",
		},
		{
			name:  "без layout — RFC3339",
			param: TimeParam{Offset: "0d"},
			want:  "2026-08-13T22:30:00Z",
		},
		{
			name:   "битый offset",
			param:  TimeParam{Offset: "давно", Layout: "2006-01-02"},
			hasErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.param.Render(now)
			if (err != nil) != c.hasErr {
				t.Fatalf("Render: ошибка = %v, ожидалась: %v", err, c.hasErr)
			}
			if err == nil && got != c.want {
				t.Errorf("Render = %q, ожидалось %q", got, c.want)
			}
		})
	}
}

// Диапазон должен ехать вместе с текущим моментом. Если он застынет,
// бот будет вечно спрашивать один и тот же прошедший период и не увидит
// ни одного нового поступления.
func TestTimeParamMovesWithNow(t *testing.T) {
	p := TimeParam{Offset: "-30d", Layout: "2006-01-02"}

	first, err := p.Render(time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	second, err := p.Render(time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if first == second {
		t.Errorf("значение не изменилось за месяц: %q", first)
	}
	if first != "2026-07-14" || second != "2026-08-14" {
		t.Errorf("получено %q и %q", first, second)
	}
}
