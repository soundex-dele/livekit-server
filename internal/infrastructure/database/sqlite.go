package database

import (
	"database/sql"
	_ "modernc.org/sqlite"
)

// OpenSQLite configures a single-process database. Schema ownership stays with each business.
func OpenSQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=3000;"); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
