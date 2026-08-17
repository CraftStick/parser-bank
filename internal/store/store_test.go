package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/CraftStick/parser-bank/internal/bank"
)

func open(t *testing.T, limit int) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), limit)
	if err != nil {
		t.Fatalf("открыть базу: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func tx(id string, minutesAgo int, dir bank.Direction) bank.Transaction {
	return bank.Transaction{
		ID:        id,
		Time:      time.Now().Add(-time.Duration(minutesAgo) * time.Minute),
		Amount:    100,
		Currency:  "RUB",
		Direction: dir,
		Title:     "тест",
	}
}

func TestSaveReportsOnlyNew(t *testing.T) {
	st := open(t, 10)

	isNew, err := st.Save(tx("op-1", 1, bank.DirectionIn))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !isNew {
		t.Error("первое сохранение должно считаться новым")
	}

	// Повторный опрос вернёт ту же операцию — она уже не новая,
	// иначе бот присылал бы уведомление о ней каждую минуту.
	isNew, err = st.Save(tx("op-1", 1, bank.DirectionIn))
	if err != nil {
		t.Fatalf("save повторно: %v", err)
	}
	if isNew {
		t.Error("повторное сохранение не должно считаться новым")
	}
}

func TestLastReturnsNewestFirst(t *testing.T) {
	st := open(t, 10)

	for _, c := range []struct {
		id      string
		minutes int
	}{{"старая", 60}, {"свежая", 1}, {"средняя", 30}} {
		if _, err := st.Save(tx(c.id, c.minutes, bank.DirectionIn)); err != nil {
			t.Fatalf("save %s: %v", c.id, err)
		}
	}

	got, err := st.Last(3)
	if err != nil {
		t.Fatalf("last: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("получено %d операций, ожидалось 3", len(got))
	}

	want := []string{"свежая", "средняя", "старая"}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("позиция %d: %q, ожидалось %q", i, got[i].ID, id)
		}
	}
}

func TestLastRespectsLimit(t *testing.T) {
	st := open(t, 10)
	for _, id := range []string{"a", "b", "c"} {
		if _, err := st.Save(tx(id, 1, bank.DirectionIn)); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	got, err := st.Last(2)
	if err != nil {
		t.Fatalf("last: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("получено %d, ожидалось 2", len(got))
	}
}

func TestPruneKeepsNewest(t *testing.T) {
	st := open(t, 2)

	for i, id := range []string{"самая-старая", "средняя", "свежая"} {
		if _, err := st.Save(tx(id, 30-i*10, bank.DirectionIn)); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}

	if err := st.Prune(); err != nil {
		t.Fatalf("prune: %v", err)
	}

	got, err := st.Last(10)
	if err != nil {
		t.Fatalf("last: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("после prune осталось %d операций, ожидалось 2", len(got))
	}
	for _, g := range got {
		if g.ID == "самая-старая" {
			t.Error("prune оставил самую старую операцию")
		}
	}
}

// При смене способа вычисления ключа старые записи ни с чем не совпадут,
// и каждая операция окажется «новой». Проще забыть их и прогреться заново.
func TestMigrationClearsHistoryOnSchemaChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	st, err := Open(path, 10)
	if err != nil {
		t.Fatalf("открыть базу: %v", err)
	}
	if _, err := st.Save(tx("старый-ключ", 5, bank.DirectionIn)); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := st.SetBootstrapped(); err != nil {
		t.Fatalf("set: %v", err)
	}
	st.Close()

	// Имитируем базу, оставшуюся от прошлой версии ключей.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("открыть напрямую: %v", err)
	}
	if _, err := db.Exec(`UPDATE meta SET v = '1' WHERE k = 'schema'`); err != nil {
		t.Fatalf("понизить версию: %v", err)
	}
	db.Close()

	st, err = Open(path, 10)
	if err != nil {
		t.Fatalf("переоткрыть базу: %v", err)
	}
	defer st.Close()

	got, err := st.Last(10)
	if err != nil {
		t.Fatalf("last: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("история от прошлой версии не очищена: %d записей", len(got))
	}

	ok, err := st.Bootstrapped()
	if err != nil {
		t.Fatalf("bootstrapped: %v", err)
	}
	if ok {
		t.Error("признак прогрева пережил смену схемы — история прилетит владельцу пачкой")
	}
}

func TestMigrationKeepsDataOnSameVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	st, err := Open(path, 10)
	if err != nil {
		t.Fatalf("открыть базу: %v", err)
	}
	if _, err := st.Save(tx("op-1", 5, bank.DirectionIn)); err != nil {
		t.Fatalf("save: %v", err)
	}
	st.Close()

	st, err = Open(path, 10)
	if err != nil {
		t.Fatalf("переоткрыть базу: %v", err)
	}
	defer st.Close()

	got, err := st.Last(10)
	if err != nil {
		t.Fatalf("last: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("данные потеряны при обычном перезапуске: %d записей", len(got))
	}
}

func TestBootstrappedFlag(t *testing.T) {
	st := open(t, 10)

	// Флаг нужен, чтобы на первом запуске не прилететь пачкой уведомлений
	// обо всей истории сразу.
	ok, err := st.Bootstrapped()
	if err != nil {
		t.Fatalf("bootstrapped: %v", err)
	}
	if ok {
		t.Error("свежая база не должна считаться прогретой")
	}

	if err := st.SetBootstrapped(); err != nil {
		t.Fatalf("set: %v", err)
	}

	if ok, err = st.Bootstrapped(); err != nil || !ok {
		t.Errorf("после SetBootstrapped: ok=%v err=%v", ok, err)
	}

	// Повторный вызов не должен падать на конфликте ключа.
	if err := st.SetBootstrapped(); err != nil {
		t.Errorf("повторный SetBootstrapped: %v", err)
	}
}

func TestRoundTripPreservesFields(t *testing.T) {
	st := open(t, 10)

	want := bank.Transaction{
		ID:        "op-42",
		Time:      time.Now().Truncate(time.Second),
		Amount:    1500.5,
		Currency:  "RUB",
		Direction: bank.DirectionIn,
		Title:     "Иванов И.И.",
		Card:      "•• 4321",
	}
	if _, err := st.Save(want); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := st.Last(1)
	if err != nil {
		t.Fatalf("last: %v", err)
	}
	if len(got) != 1 {
		t.Fatal("операция не прочиталась")
	}

	g := got[0]
	if g.ID != want.ID || g.Amount != want.Amount || g.Currency != want.Currency ||
		g.Direction != want.Direction || g.Title != want.Title || g.Card != want.Card {
		t.Errorf("данные не совпали:\nполучено %+v\nожидалось %+v", g, want)
	}
	if !g.Time.Equal(want.Time) {
		t.Errorf("время: получено %v, ожидалось %v", g.Time, want.Time)
	}
}
