// Package identity owns local accounts and revocable sessions. Media credentials
// are deliberately not accepted as application sessions.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

var ErrUnauthorized = errors.New("unauthorized")
var ErrInvalid = errors.New("invalid_account")
var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_]{3,32}$`)

type User struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Admin    bool   `json:"admin"`
	Disabled bool   `json:"disabled"`
}
type Service struct {
	DB  *sql.DB
	Now func() time.Time
}

const schema = `CREATE TABLE IF NOT EXISTS identity_users(id TEXT PRIMARY KEY,name TEXT NOT NULL UNIQUE,password TEXT NOT NULL,admin INTEGER NOT NULL DEFAULT 0,disabled INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS identity_sessions(hash TEXT PRIMARY KEY,user_id TEXT NOT NULL,expires INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS identity_session_user ON identity_sessions(user_id);`

func New(db *sql.DB) (*Service, error) { _, err := db.Exec(schema); return &Service{db, time.Now}, err }
func ID() string {
	b := make([]byte, 24)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func hash(token string) string { x := sha256.Sum256([]byte(token)); return hex.EncodeToString(x[:]) }
func passwordHash(password string) (string, error) {
	if len(password) < 10 || len(password) > 128 {
		return "", ErrInvalid
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, 2, 32*1024, 1, 32)
	return base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}
func checkPassword(encoded, password string) bool {
	p := strings.Split(encoded, "$")
	if len(p) != 2 || len(password) > 128 {
		return false
	}
	salt, e := base64.RawStdEncoding.DecodeString(p[0])
	if e != nil || len(salt) != 16 {
		return false
	}
	expected, e := base64.RawStdEncoding.DecodeString(p[1])
	if e != nil || len(expected) != 32 {
		return false
	}
	key := argon2.IDKey([]byte(password), salt, 2, 32*1024, 1, 32)
	return subtle.ConstantTimeCompare(key, expected) == 1
}
func (s *Service) Register(ctx context.Context, name, password string, admin bool) (User, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !usernamePattern.MatchString(name) {
		return User{}, ErrInvalid
	}
	encoded, err := passwordHash(password)
	if err != nil {
		return User{}, err
	}
	u := User{ID: ID(), Name: name, Admin: admin}
	_, err = s.DB.ExecContext(ctx, "INSERT INTO identity_users(id,name,password,admin) VALUES(?,?,?,?)", u.ID, u.Name, encoded, admin)
	if err != nil {
		return User{}, fmt.Errorf("account_unavailable")
	}
	return u, nil
}
func (s *Service) Login(ctx context.Context, name, password string) (User, string, error) {
	var u User
	var encoded string
	err := s.DB.QueryRowContext(ctx, "SELECT id,name,password,admin,disabled FROM identity_users WHERE name=?", strings.ToLower(strings.TrimSpace(name))).Scan(&u.ID, &u.Name, &encoded, &u.Admin, &u.Disabled)
	if err != nil { // Comparable password cost for unknown accounts.
		_ = argon2.IDKey([]byte(password), make([]byte, 16), 2, 32*1024, 1, 32)
		return User{}, "", ErrUnauthorized
	}
	if !checkPassword(encoded, password) || u.Disabled {
		return User{}, "", ErrUnauthorized
	}
	token, err := s.Session(ctx, u.ID)
	return u, token, err
}
func (s *Service) Session(ctx context.Context, user string) (string, error) {
	token := ID()
	_, err := s.DB.ExecContext(ctx, "INSERT INTO identity_sessions(hash,user_id,expires) VALUES(?,?,?)", hash(token), user, s.Now().Add(24*time.Hour).Unix())
	return token, err
}
func (s *Service) Authenticate(ctx context.Context, token string) (User, error) {
	var u User
	if token == "" {
		return u, ErrUnauthorized
	}
	err := s.DB.QueryRowContext(ctx, `SELECT u.id,u.name,u.admin,u.disabled FROM identity_users u JOIN identity_sessions s ON s.user_id=u.id WHERE s.hash=? AND s.expires>? AND u.disabled=0`, hash(token), s.Now().Unix()).Scan(&u.ID, &u.Name, &u.Admin, &u.Disabled)
	if err != nil {
		return User{}, ErrUnauthorized
	}
	return u, nil
}
func (s *Service) Logout(ctx context.Context, token string) error {
	_, err := s.DB.ExecContext(ctx, "DELETE FROM identity_sessions WHERE hash=?", hash(token))
	return err
}
func (s *Service) ChangePassword(ctx context.Context, u User, old, password string) error {
	var encoded string
	if err := s.DB.QueryRowContext(ctx, "SELECT password FROM identity_users WHERE id=?", u.ID).Scan(&encoded); err != nil {
		return err
	}
	if !checkPassword(encoded, old) {
		return ErrUnauthorized
	}
	return s.ResetPassword(ctx, u.ID, password)
}
func (s *Service) ResetPassword(ctx context.Context, id, password string) error {
	encoded, err := passwordHash(password)
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, "UPDATE identity_users SET password=? WHERE id=?", encoded, id)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return ErrInvalid
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM identity_sessions WHERE user_id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Service) Disable(ctx context.Context, id string, disabled bool) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE identity_users SET disabled=? WHERE id=?", disabled, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM identity_sessions WHERE user_id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}
