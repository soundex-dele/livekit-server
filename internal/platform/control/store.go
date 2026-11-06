package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"platform-foundation/server/persistence"
	"platform-foundation/server/taskqueue"
	"strings"
	"time"
)

const resourceColumns = "id,scope,kind,owner,title,state,version,created,updated"

func scanResource(row interface{ Scan(...any) error }) (Resource, error) {
	var r Resource
	e := row.Scan(&r.ID, &r.Scope, &r.Kind, &r.Owner, &r.Title, &r.State, &r.Version, &r.Created, &r.Updated)
	if errors.Is(e, sql.ErrNoRows) {
		e = Fail(404, "resource_not_found")
	}
	return r, e
}
func ResourceTx(ctx context.Context, t *persistence.Tx, id string) (Resource, error) {
	return scanResource(t.Row(ctx, "SELECT "+resourceColumns+" FROM av_resources WHERE scope=? AND id=?", t.Scope, id))
}
func (s *Platform) ResourceTx(ctx context.Context, t *persistence.Tx, id string) (Resource, error) {
	return ResourceTx(ctx, t, id)
}
func (s *Platform) GetResource(ctx context.Context, p Principal, id string) (Resource, error) {
	r, e := scanResource(s.Store.DB.QueryRowContext(ctx, s.Store.Q("SELECT "+resourceColumns+" FROM av_resources WHERE scope=? AND id=?"), p.Scope, id))
	if e != nil {
		return r, e
	}
	return r, p.AuthorizeResource(r)
}
func InsertResource(ctx context.Context, t *persistence.Tx, r Resource) error {
	if r.Scope != t.Scope || !validScope(r.Scope) {
		return Fail(403, "invalid_scope")
	}
	if r.Version == 0 {
		r.Version = 1
	}
	_, e := t.Exec(ctx, "INSERT INTO av_resources("+resourceColumns+") VALUES(?,?,?,?,?,?,?,?,?)", r.ID, r.Scope, r.Kind, r.Owner, r.Title, r.State, r.Version, r.Created, r.Updated)
	return e
}
func (s *Platform) InsertResource(ctx context.Context, t *persistence.Tx, r Resource) error {
	return InsertResource(ctx, t, r)
}
func UpdateResource(ctx context.Context, t *persistence.Tx, r Resource) error {
	res, e := t.Exec(ctx, "UPDATE av_resources SET title=?,state=?,version=version+1,updated=? WHERE scope=? AND id=? AND version=?", r.Title, r.State, r.Updated, t.Scope, r.ID, r.Version)
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e == nil && n != 1 {
		return Fail(409, "version_conflict")
	}
	return e
}
func (s *Platform) UpdateResource(ctx context.Context, t *persistence.Tx, r Resource) error {
	return UpdateResource(ctx, t, r)
}

// Mutate saves the outcome in the very same transaction as the resource change.
// Credential-issuing handlers deliberately do not use this response cache.
func (s *Platform) Mutate(ctx context.Context, p Principal, action, key string, input any, fn func(*persistence.Tx) (any, error)) (any, error) {
	if !validScope(p.Scope) || p.ID == "" {
		return nil, Fail(403, "organization_required")
	}
	if len(key) > 128 {
		return nil, Fail(400, "invalid_idempotency_key")
	}
	body, e := json.Marshal(input)
	if e != nil {
		return nil, e
	}
	fingerprint := Hash(string(body))
	var result any
	e = s.Store.Within(ctx, p.Scope, func(t *persistence.Tx) error {
		if key != "" {
			var h, raw string
			err := t.Row(ctx, "SELECT hash,response FROM av_idempotency WHERE scope=? AND subject_id=? AND action=? AND key=?", p.Scope, p.ID, action, key).Scan(&h, &raw)
			if err == nil {
				if h != fingerprint {
					return Fail(409, "idempotency_conflict")
				}
				return json.Unmarshal([]byte(raw), &result)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		var err error
		result, err = fn(t)
		if err != nil {
			return err
		}
		if key == "" {
			return nil
		}
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		_, err = t.Exec(ctx, "INSERT INTO av_idempotency(scope,subject_id,action,key,hash,response,created) VALUES(?,?,?,?,?,?,?)", p.Scope, p.ID, action, key, fingerprint, string(raw), s.Now().UnixMilli())
		return err
	})
	return result, e
}
func (s *Platform) Emit(ctx context.Context, t *persistence.Tx, actor, resource, event string, payload any) error {
	if actor == "" {
		actor = "system"
	}
	raw, e := json.Marshal(payload)
	if e != nil {
		return e
	}
	if _, e = t.Exec(ctx, "INSERT INTO av_event_heads(scope,resource_id,sequence) VALUES(?,?,0) ON CONFLICT(scope,resource_id) DO NOTHING", t.Scope, resource); e != nil {
		return e
	}
	if _, e = t.Exec(ctx, "UPDATE av_event_heads SET sequence=sequence+1 WHERE scope=? AND resource_id=?", t.Scope, resource); e != nil {
		return e
	}
	var seq int64
	if e = t.Row(ctx, "SELECT sequence FROM av_event_heads WHERE scope=? AND resource_id=?", t.Scope, resource).Scan(&seq); e != nil {
		return e
	}
	id := ID()
	if _, e = t.Exec(ctx, "INSERT INTO av_events(id,scope,resource_id,sequence,type,payload,created) VALUES(?,?,?,?,?,?,?)", id, t.Scope, resource, seq, event, string(raw), s.Now().UnixMilli()); e != nil {
		return e
	}
	if !strings.HasSuffix(event, "chat") {
		if e = t.Audit(ctx, actor, event, resource, ""); e != nil {
			return e
		}
	}
	packet, _ := json.Marshal(map[string]any{"scope": t.Scope, "resource_id": resource, "sequence": seq})
	_, e = s.Queue.Submit(ctx, t, actor, taskqueue.Spec{Kind: "event", Type: "av.notify", Key: id, Payload: packet, MaxAttempts: 10})
	return e
}
func (s *Platform) IssueGuest(ctx context.Context, t *persistence.Tx, scope, resource, role string) (string, Principal, error) {
	if scope != t.Scope || !validScope(scope) {
		return "", Principal{}, Fail(403, "invalid_scope")
	}
	raw := ID()
	p := Principal{ID: "guest:" + ID(), Scope: scope, ResourceID: resource, Kind: "guest"}
	_, e := t.Exec(ctx, "INSERT INTO av_credentials(hash,scope,resource_id,subject_id,role,expires) VALUES(?,?,?,?,?,?)", Hash(raw), scope, resource, p.ID, role, s.Now().Add(24*time.Hour).UnixMilli())
	return raw, p, e
}
func (s *Platform) Guest(ctx context.Context, credential string) (Principal, error) {
	var p Principal
	var expires int64
	var revoked int
	p.Kind = "guest"
	e := s.Store.DB.QueryRowContext(ctx, s.Store.Q("SELECT scope,resource_id,subject_id,expires,revoked FROM av_credentials WHERE hash=?"), Hash(credential)).Scan(&p.Scope, &p.ResourceID, &p.ID, &expires, &revoked)
	if errors.Is(e, sql.ErrNoRows) || revoked != 0 || expires <= s.Now().UnixMilli() {
		return Principal{}, Fail(401, "invalid_resource_credential")
	}
	return p, e
}
func (s *Platform) CanObserve(ctx context.Context, p Principal, id string) error {
	r, e := s.GetResource(ctx, p, id)
	if e != nil {
		return e
	}
	if r.Owner == p.ID || p.IsUser() && p.Can("av.manage") {
		return nil
	}
	var state string
	e = s.Store.DB.QueryRowContext(ctx, s.Store.Q("SELECT state FROM av_participants WHERE scope=? AND resource_id=? AND subject_id=?"), p.Scope, id, p.ID).Scan(&state)
	if e != nil || state == "removed" || state == "left" || state == "rejected" {
		return Fail(403, "participant_required")
	}
	return nil
}
func (s *Platform) Events(ctx context.Context, p Principal, id string, after int64) ([]Event, error) {
	if e := s.CanObserve(ctx, p, id); e != nil {
		return nil, e
	}
	rows, e := s.Store.DB.QueryContext(ctx, s.Store.Q("SELECT id,resource_id,sequence,type,payload,created FROM av_events WHERE scope=? AND resource_id=? AND sequence>? ORDER BY sequence LIMIT 200"), p.Scope, id, after)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var ev Event
		var raw string
		if e = rows.Scan(&ev.ID, &ev.ResourceID, &ev.Sequence, &ev.Type, &raw, &ev.Created); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(raw), &ev.Payload); e != nil {
			return nil, e
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}
