// Package store хранит увиденные операции в SQLite.
//
// Хранилище нужно ровно для одного: отличить новую операцию от уже показанной.
// Поэтому история намеренно короткая — держим последние HISTORY_LIMIT записей,
// остальное подчищаем.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/valerakrut/parserbank/internal/bank"
	_ "modernc.org/sqlite" // чистый Go, без cgo — важно для сборки под VPS
)

const schema = `
CREATE TABLE IF NOT EXISTS transactions (
	id        TEXT PRIMARY KEY,
	ts        INTEGER NOT NULL,
	amount    REAL    NOT NULL,
	currency  TEXT    NOT NULL DEFAULT '',
	direction TEXT    NOT NULL DEFAULT '',
	title     TEXT    NOT NULL DEFAULT '',
	card      TEXT    NOT NULL DEFAULT '',
	raw       TEXT    NOT NULL DEFAULT '',
	seen_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_transactions_ts ON transactions(ts DESC);

CREATE TABLE IF NOT EXISTS meta (
	k TEXT PRIMARY KEY,
	v TEXT NOT NULL
);
`

type Store struct {
	db    *sql.DB
	limit int
}

// Open открывает базу и накатывает схему.
func Open(path string, limit int) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("создать каталог базы: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("открыть базу %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("база %s недоступна: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("создать схему: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{db: db, limit: limit}, nil
}

// schemaVersion повышается, когда меняется способ вычисления ключа операции.
const schemaVersion = "2"

// migrate сбрасывает историю, если ключи считались по-старому.
//
// Записи с ключами прошлой версии ни с чем не совпадут, и каждая операция
// будет выглядеть новой. Дешевле забыть их и прогреться заново, чем один раз
// прислать владельцу десяток уведомлений о том, что он и так видел.
func migrate(db *sql.DB) error {
	var got string
	err := db.QueryRow(`SELECT v FROM meta WHERE k = 'schema'`).Scan(&got)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("прочитать версию схемы: %w", err)
	}
	if got == schemaVersion {
		return nil
	}

	if _, err := db.Exec(`DELETE FROM transactions`); err != nil {
		return fmt.Errorf("очистить историю: %w", err)
	}
	if _, err := db.Exec(`DELETE FROM meta WHERE k = 'bootstrapped'`); err != nil {
		return fmt.Errorf("сбросить признак прогрева: %w", err)
	}
	if _, err := db.Exec(`
		INSERT INTO meta (k, v) VALUES ('schema', ?)
		ON CONFLICT(k) DO UPDATE SET v = excluded.v`, schemaVersion); err != nil {
		return fmt.Errorf("записать версию схемы: %w", err)
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

// Save записывает операцию. Возвращает true, если она видится впервые.
func (s *Store) Save(tx bank.Transaction) (bool, error) {
	res, err := s.db.Exec(`
		INSERT INTO transactions (id, ts, amount, currency, direction, title, card, raw, seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		tx.ID, tx.Time.Unix(), tx.Amount, tx.Currency,
		string(tx.Direction), tx.Title, tx.Card, tx.Raw, time.Now().Unix())
	if err != nil {
		return false, fmt.Errorf("сохранить операцию %s: %w", tx.ID, err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// Last возвращает последние n операций, свежие первыми.
func (s *Store) Last(n int) ([]bank.Transaction, error) { return s.Page(0, n) }

// Count — сколько операций сохранено.
func (s *Store) Count() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM transactions`).Scan(&n); err != nil {
		return 0, fmt.Errorf("посчитать историю: %w", err)
	}
	return n, nil
}

// Page возвращает страницу истории, свежие первыми.
func (s *Store) Page(offset, limit int) ([]bank.Transaction, error) {
	rows, err := s.db.Query(`
		SELECT id, ts, amount, currency, direction, title, card
		FROM transactions ORDER BY ts DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("прочитать историю: %w", err)
	}
	defer rows.Close()

	var out []bank.Transaction
	for rows.Next() {
		var (
			tx  bank.Transaction
			ts  int64
			dir string
		)
		if err := rows.Scan(&tx.ID, &ts, &tx.Amount, &tx.Currency, &dir, &tx.Title, &tx.Card); err != nil {
			return nil, fmt.Errorf("разобрать строку истории: %w", err)
		}
		tx.Time = time.Unix(ts, 0)
		tx.Direction = bank.Direction(dir)
		out = append(out, tx)
	}
	return out, rows.Err()
}

// Prune оставляет в базе только последние limit операций.
func (s *Store) Prune() error {
	_, err := s.db.Exec(`
		DELETE FROM transactions WHERE id NOT IN (
			SELECT id FROM transactions ORDER BY ts DESC LIMIT ?
		)`, s.limit)
	if err != nil {
		return fmt.Errorf("подчистить историю: %w", err)
	}
	return nil
}

// Bootstrapped показывает, был ли уже первый успешный опрос.
//
// Нужен, чтобы на самом первом запуске не прилететь пачкой уведомлений обо
// всей истории: первый проход сохраняет операции молча.
func (s *Store) Bootstrapped() (bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM meta WHERE k = 'bootstrapped'`).Scan(&v)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("прочитать meta: %w", err)
	}
	return v == "1", nil
}

func (s *Store) SetBootstrapped() error {
	_, err := s.db.Exec(`
		INSERT INTO meta (k, v) VALUES ('bootstrapped', '1')
		ON CONFLICT(k) DO UPDATE SET v = '1'`)
	if err != nil {
		return fmt.Errorf("записать meta: %w", err)
	}
	return nil
}
