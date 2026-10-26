package remote

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	_ "modernc.org/sqlite"
)

// Store isolates persistence and SQL dialect from the authorization state machine.
// A deployment runs one API process per SQLite database.
type Store interface {
	Load(context.Context) ([]Session, error)
	Save(context.Context, Session, string) error
	Close() error
}

type SQLite struct{ db *sql.DB }

func OpenSQLite(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=3000;
CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY);
CREATE TABLE IF NOT EXISTS control_sessions(id TEXT PRIMARY KEY, payload TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS audit_events(id INTEGER PRIMARY KEY, session_id TEXT NOT NULL, action TEXT NOT NULL, occurred_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP);
INSERT OR IGNORE INTO schema_migrations(version) VALUES(1);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &SQLite{db}, nil
}
func (s *SQLite) Load(ctx context.Context) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM control_sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Session
	for rows.Next() {
		var raw string
		var item Session
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &item); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
func (s *SQLite) Save(ctx context.Context, item Session, action string) error {
	raw, err := json.Marshal(item)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO control_sessions(id,payload) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload`, item.ID, string(raw))
	if err != nil {
		return err
	}
	if action != "" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(session_id,action) VALUES(?,?)`, item.ID, action); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *SQLite) Close() error { return s.db.Close() }

var ErrConflict = errors.New("state conflict")
