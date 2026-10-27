package infrastructure

import (
	"av-platform/business/internal/businesses/screencast/domain"
	"av-platform/business/internal/infrastructure/database"
	"av-platform/business/migrations"
	"context"
	"database/sql"
	"encoding/json"
)

type SQLite struct{ db *sql.DB }

func OpenSQLite(path string) (*SQLite, error) {
	db, e := database.OpenSQLite(path)
	if e != nil {
		return nil, e
	}
	if _, e = db.Exec(migrations.ScreenCast); e != nil {
		db.Close()
		return nil, e
	}
	return &SQLite{db}, nil
}
func (s *SQLite) Close() error { return s.db.Close() }
func (s *SQLite) Load(ctx context.Context) ([]domain.Session, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT payload FROM screencast_sessions")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var all []domain.Session
	for rows.Next() {
		var raw string
		var v domain.Session
		if e = rows.Scan(&raw); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(raw), &v); e != nil {
			return nil, e
		}
		all = append(all, v)
	}
	return all, rows.Err()
}
func (s *SQLite) Save(ctx context.Context, v domain.Session) error {
	raw, e := json.Marshal(v)
	if e != nil {
		return e
	}
	_, e = s.db.ExecContext(ctx, `INSERT INTO screencast_sessions(id,payload) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload`, v.ID, string(raw))
	return e
}
