// Package v2 contains independently routed business use cases. Database and
// media mechanisms are supplied by control; business policy remains here.
package v2

import (
	"av-platform/business/internal/platform/control"
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"

	"platform-foundation/server/persistence"
)

type Service struct {
	P       *control.Platform
	modules map[string]bool
}

func New(p *control.Platform) *Service {
	names := make([]string, 0, len(businessModules))
	for _, module := range businessModules {
		names = append(names, module.name)
	}
	return NewWithModules(p, names)
}

type Route struct {
	Method, Path      string
	Public, Sensitive bool
}

func Routes() []Route {
	return []Route{
		{"GET", "screen-cast/sessions", false, false}, {"POST", "screen-cast/sessions", false, true},
		{"POST", "screen-cast/redeem", true, true}, {"GET", "screen-cast/sessions/{id}", false, false},
		{"POST", "screen-cast/sessions/{id}/{action}", false, false},
		{"POST", "remote-control/sessions", false, true}, {"POST", "remote-control/redeem", true, true},
		{"GET", "remote-control/sessions/{id}", false, false}, {"POST", "remote-control/sessions/{id}/{action}", false, false},
		{"POST", "control-grants/{id}/{action}", false, false},
		{"GET", "conference/meetings", false, false}, {"POST", "conference/meetings", false, false},
		{"POST", "conference/join", true, true}, {"GET", "conference/meetings/{id}", false, false},
		{"POST", "conference/meetings/{id}/{action}", false, false},
		{"GET", "live-studio/rooms", false, false}, {"POST", "live-studio/rooms", false, false},
		{"GET", "live-studio/rooms/{id}", true, false}, {"POST", "live-studio/rooms/{id}/start", false, false},
		{"POST", "live-studio/broadcasts/{id}/watch", true, true}, {"GET", "live-studio/broadcasts/{id}", false, false},
		{"POST", "live-studio/broadcasts/{id}/{action}", false, false},
		{"GET", "live-studio/wallet", false, false}, {"GET", "live-studio/board", false, false},
	}
}
func Public(method, path string) bool {
	path = strings.Trim(path, "/")
	return method == "POST" && (path == "screen-cast/redeem" || path == "remote-control/redeem" || path == "conference/join" || strings.HasPrefix(path, "live-studio/broadcasts/") && strings.HasSuffix(path, "/watch")) || method == "GET" && strings.HasPrefix(path, "live-studio/rooms/")
}
func Sensitive(method, path string) bool {
	path = strings.Trim(path, "/")
	return method == "POST" && (Public(method, path) || path == "screen-cast/sessions" || path == "remote-control/sessions" || strings.HasSuffix(path, "/renew") || strings.HasSuffix(path, "/approve"))
}

type command struct {
	Title        string `json:"title"`
	Name         string `json:"name"`
	Code         string `json:"code"`
	Password     string `json:"password"`
	Number       string `json:"number"`
	Starts       string `json:"starts_at"`
	Duration     int    `json:"duration_minutes"`
	Scheduled    bool   `json:"scheduled"`
	Waiting      *bool  `json:"waiting_room"`
	Target       string `json:"target"`
	Enabled      bool   `json:"enabled"`
	Content      string `json:"content"`
	Amount       int64  `json:"amount"`
	LocalConsent bool   `json:"local_consent"`
	Epoch        int64  `json:"epoch"`
	ResourceID   string `json:"resource_id"`
	Stage        bool   `json:"stage"`
}

func (s *Service) Handle(ctx context.Context, p control.Principal, method, path string, body json.RawMessage, key string) (any, error) {
	path = strings.Trim(path, "/")
	if Sensitive(method, path) && key != "" {
		return nil, control.Fail(400, "idempotency_not_supported")
	}
	var c command
	if len(body) > 0 && string(body) != "null" {
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if e := decoder.Decode(&c); e != nil {
			return nil, control.Fail(400, "invalid_json")
		}
		if decoder.Decode(new(any)) != io.EOF {
			return nil, control.Fail(400, "invalid_json")
		}
	}
	parts := strings.Split(path, "/")
	handler, enabled := s.handler(parts[0])
	if handler == nil {
		return nil, control.Fail(404, "not_found")
	}
	if !enabled {
		return nil, control.Fail(403, "module_disabled")
	}
	return handler(s, ctx, p, method, parts[1:], c, key)
}

func (s *Service) now() time.Time {
	if s.P.Now != nil {
		return s.P.Now()
	}
	return time.Now()
}
func number(digits int) (string, error) {
	max := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(digits-1)), nil)
	n, e := rand.Int(rand.Reader, new(big.Int).Mul(max, big.NewInt(9)))
	if e != nil {
		return "", e
	}
	return new(big.Int).Add(n, max).String(), nil
}
func permission(p control.Principal, name string) error {
	if !p.IsUser() || !strings.HasPrefix(p.Scope, "org:") || !p.Can(name) {
		return control.Fail(403, "forbidden")
	}
	return nil
}
func validTitle(v string) bool { return strings.TrimSpace(v) != "" && utf8.RuneCountInString(v) <= 200 }
func isTerminal(state string) bool {
	return state == "ended" || state == "expired" || state == "cancelled" || state == "failed"
}
func (s *Service) resource(ctx context.Context, p control.Principal, id, kind string) (control.Resource, error) {
	r, e := s.P.GetResource(ctx, p, id)
	if e != nil {
		return r, e
	}
	if r.Kind != kind {
		return r, control.Fail(404, "not_found")
	}
	return r, nil
}
func (s *Service) inResource(ctx context.Context, t *persistence.Tx, p control.Principal, id, kind string) (control.Resource, error) {
	r, e := s.P.ResourceTx(ctx, t, id)
	if e != nil {
		return r, e
	}
	if r.Kind != kind || r.Scope != p.Scope || p.ResourceID != "" && p.ResourceID != id {
		return r, control.Fail(404, "not_found")
	}
	return r, nil
}

type participant struct {
	Subject string `json:"subject_id"`
	Role    string `json:"role"`
	State   string `json:"state"`
	Policy  int64  `json:"policy_version"`
	Updated int64  `json:"updated"`
}

func participantTx(ctx context.Context, t *persistence.Tx, id, subject string) (participant, error) {
	var p participant
	e := t.Row(ctx, "SELECT subject_id,role,state,policy_version,updated FROM av_participants WHERE scope=? AND resource_id=? AND subject_id=?", t.Scope, id, subject).Scan(&p.Subject, &p.Role, &p.State, &p.Policy, &p.Updated)
	return p, e
}
func (s *Service) participant(ctx context.Context, p control.Principal, id string) (participant, error) {
	var v participant
	e := s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT subject_id,role,state,policy_version,updated FROM av_participants WHERE scope=? AND resource_id=? AND subject_id=?"), p.Scope, id, p.ID).Scan(&v.Subject, &v.Role, &v.State, &v.Policy, &v.Updated)
	if errors.Is(e, sql.ErrNoRows) {
		e = control.Fail(403, "not_a_participant")
	}
	return v, e
}
func putParticipant(ctx context.Context, t *persistence.Tx, id, subject, role, state string, now int64) error {
	_, e := t.Exec(ctx, "INSERT INTO av_participants(scope,resource_id,subject_id,role,state,policy_version,updated) VALUES(?,?,?,?,?,1,?) ON CONFLICT(scope,resource_id,subject_id) DO UPDATE SET role=excluded.role,state=excluded.state,policy_version=av_participants.policy_version+1,updated=excluded.updated", t.Scope, id, subject, role, state, now)
	return e
}
func (s *Service) emit(ctx context.Context, t *persistence.Tx, p control.Principal, r control.Resource, action string, value any) error {
	return s.P.Emit(ctx, t, p.ID, r.ID, action, value)
}
func (s *Service) finish(ctx context.Context, t *persistence.Tx, p control.Principal, r control.Resource, state string) (any, error) {
	if !isTerminal(r.State) {
		r.State = state
		r.Updated = s.now().UnixMilli()
		if e := s.P.UpdateResource(ctx, t, r); e != nil {
			return nil, e
		}
		if e := s.P.Revoke(ctx, t, r.ID, ""); e != nil {
			return nil, e
		}
		if _, e := t.Exec(ctx, "UPDATE av_participants SET state='ended',policy_version=policy_version+1,updated=? WHERE scope=? AND resource_id=?", s.now().UnixMilli(), t.Scope, r.ID); e != nil {
			return nil, e
		}
		if e := s.emit(ctx, t, p, r, "session.ended", map[string]any{"state": state}); e != nil {
			return nil, e
		}
	}
	rows, e := t.SQL.QueryContext(ctx, t.Store.Q("SELECT id FROM av_media_operations WHERE scope=? AND resource_id=? AND action IN ('RemoveParticipant','DeleteRoom') ORDER BY created,id"), t.Scope, r.ID)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	operationIDs := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			return nil, e
		}
		operationIDs = append(operationIDs, id)
	}
	return map[string]any{"id": r.ID, "state": r.State, "accepted": true, "operation_ids": operationIDs}, rows.Err()
}
func presenceKey(scope, id, subject string) string {
	return "av:presence:" + scope + ":" + id + ":" + subject
}
func (s *Service) heartbeat(ctx context.Context, p control.Principal, id string) (any, error) {
	r, e := s.P.GetResource(ctx, p, id)
	if e != nil {
		return nil, e
	}
	if isTerminal(r.State) {
		return nil, control.Fail(409, "session_ended")
	}
	m, e := s.participant(ctx, p, id)
	if e != nil {
		return nil, e
	}
	if m.State != "active" && m.State != "accepted" && m.State != "disconnected" && m.State != "waiting" {
		return nil, control.Fail(403, "participant_inactive")
	}
	e = s.P.Ephemeral.Set(ctx, presenceKey(p.Scope, id, p.ID), fmt.Sprint(s.now().UnixMilli()), 30*time.Second)
	return map[string]any{"id": id, "state": r.State}, e
}
func (s *Service) online(ctx context.Context, scope, id, subject string) bool {
	v, e := s.P.Ephemeral.Get(ctx, presenceKey(scope, id, subject))
	return e == nil && v != ""
}
func (s *Service) list(ctx context.Context, p control.Principal, kind string) (any, error) {
	if !p.IsUser() || !strings.HasPrefix(p.Scope, "org:") {
		return nil, control.Fail(403, "forbidden")
	}
	rows, e := s.P.Store.DB.QueryContext(ctx, s.P.Store.Q("SELECT id,title,state,owner,version,created,updated FROM av_resources WHERE scope=? AND kind=? AND (owner=? OR id IN (SELECT resource_id FROM av_participants WHERE scope=? AND subject_id=?)) ORDER BY created DESC,id LIMIT 200"), p.Scope, kind, p.ID, p.Scope, p.ID)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, title, state, owner string
		var version, created, updated int64
		if e = rows.Scan(&id, &title, &state, &owner, &version, &created, &updated); e != nil {
			return nil, e
		}
		out = append(out, map[string]any{"id": id, "title": title, "state": state, "owner": owner, "version": version, "created": created, "updated": updated})
	}
	return map[string]any{"items": out, "limit": 200, "offset": 0}, rows.Err()
}
