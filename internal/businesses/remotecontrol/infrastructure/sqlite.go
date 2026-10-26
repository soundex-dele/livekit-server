package infrastructure

import (
	"av-platform/business/internal/businesses/remotecontrol/domain"
	"av-platform/business/internal/infrastructure/database"
	"av-platform/business/migrations"
	"context"
	"database/sql"
	"encoding/json"
)

type SQLite struct{ db *sql.DB }

func OpenSQLite(path string) (*SQLite, error) {
	db, err := database.OpenSQLite(path)
	if err != nil {
		return nil, err
	}
	if _, err = db.Exec(migrations.RemoteControl); err != nil {
		db.Close()
		return nil, err
	}
	return &SQLite{db}, nil
}
func (s *SQLite) Load(ctx context.Context) ([]domain.Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM control_sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.Session
	for rows.Next() {
		var raw string
		var item domain.Session
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
func (s *SQLite) Save(ctx context.Context, item domain.Session, action string) error {
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
