package v2

import (
	"av-platform/business/internal/platform/control"
	"context"
	"database/sql"
	"errors"
	"time"

	"platform-foundation/server/persistence"
)

func (s *Service) pairCreate(ctx context.Context, p control.Principal, c command, kind string) (any, error) {
	perm, role := "av.screen-cast.create", "receiver"
	if kind == "assist" {
		perm, role = "av.remote-control.create", "host"
	}
	if e := permission(p, perm); e != nil {
		return nil, e
	}
	if c.Title == "" {
		c.Title = c.Name
	}
	if c.Title == "" {
		c.Title = kind
	}
	if !validTitle(c.Title) {
		return nil, control.Fail(400, "invalid_title")
	}
	code, e := number(10)
	if e != nil {
		return nil, e
	}
	now := s.now()
	r := control.Resource{ID: control.ID(), Scope: p.Scope, Kind: kind, Owner: p.ID, Title: c.Title, State: "waiting", Version: 1, Created: now.UnixMilli(), Updated: now.UnixMilli()}
	e = s.P.Store.Within(ctx, p.Scope, func(t *persistence.Tx) error {
		if e := s.P.InsertResource(ctx, t, r); e != nil {
			return e
		}
		if e := putParticipant(ctx, t, r.ID, p.ID, role, "active", now.UnixMilli()); e != nil {
			return e
		}
		if _, e := t.Exec(ctx, "INSERT INTO av_pairings(code_hash,scope,resource_id,kind,expires) VALUES(?,?,?,?,?)", control.Hash(code), p.Scope, r.ID, kind, now.Add(5*time.Minute).UnixMilli()); e != nil {
			return e
		}
		if kind == "cast" {
			_, e = t.Exec(ctx, "INSERT INTO av_cast(scope,id,receiver) VALUES(?,?,?)", p.Scope, r.ID, p.ID)
		} else {
			_, e = t.Exec(ctx, "INSERT INTO av_assist(scope,id,host) VALUES(?,?,?)", p.Scope, r.ID, p.ID)
		}
		if e != nil {
			return e
		}
		return s.emit(ctx, t, p, r, "session.created", map[string]any{"kind": kind})
	})
	if e != nil {
		return nil, e
	}
	if e = s.P.Ephemeral.Set(ctx, presenceKey(p.Scope, r.ID, p.ID), "1", 30*time.Second); e != nil {
		return nil, e
	}
	return map[string]any{"id": r.ID, "resource_id": r.ID, "state": r.State, "pairing_code": code, "expires_at": now.Add(5 * time.Minute).UTC().Format(time.RFC3339)}, nil
}

func (s *Service) redeem(ctx context.Context, c command, kind string) (any, error) {
	if len(c.Code) != 10 {
		return nil, control.Fail(400, "invalid_code")
	}
	var scope, id string
	var expires int64
	var redeemed int
	e := s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT scope,resource_id,expires,redeemed FROM av_pairings WHERE code_hash=? AND kind=?"), control.Hash(c.Code), kind).Scan(&scope, &id, &expires, &redeemed)
	if errors.Is(e, sql.ErrNoRows) || redeemed != 0 || expires <= s.now().UnixMilli() {
		return nil, control.Fail(404, "pairing_unavailable")
	}
	if e != nil {
		return nil, e
	}
	role := "sender"
	if kind == "assist" {
		role = "controller"
	}
	var token string
	var guest control.Principal
	e = s.P.Store.Within(ctx, scope, func(t *persistence.Tx) error {
		r, e := s.P.ResourceTx(ctx, t, id)
		if e != nil {
			return e
		}
		if r.State != "waiting" {
			return control.Fail(409, "pairing_unavailable")
		}
		res, e := t.Exec(ctx, "UPDATE av_pairings SET redeemed=1 WHERE code_hash=? AND redeemed=0 AND expires>?", control.Hash(c.Code), s.now().UnixMilli())
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return control.Fail(409, "pairing_unavailable")
		}
		token, guest, e = s.P.IssueGuest(ctx, t, scope, id, role)
		if e != nil {
			return e
		}
		if e = putParticipant(ctx, t, id, guest.ID, role, "active", s.now().UnixMilli()); e != nil {
			return e
		}
		if kind == "cast" {
			_, e = t.Exec(ctx, "UPDATE av_cast SET sender=?,paired=? WHERE scope=? AND id=?", guest.ID, s.now().UnixMilli(), scope, id)
		} else {
			if len(c.Name) > 120 {
				return control.Fail(400, "invalid_name")
			}
			_, e = t.Exec(ctx, "UPDATE av_assist SET controller=?,controller_name=? WHERE scope=? AND id=?", guest.ID, c.Name, scope, id)
		}
		if e != nil {
			return e
		}
		r.State = "paired"
		r.Updated = s.now().UnixMilli()
		if e = s.P.UpdateResource(ctx, t, r); e != nil {
			return e
		}
		return s.emit(ctx, t, guest, r, "session.paired", map[string]string{"role": role})
	})
	if e != nil {
		return nil, e
	}
	if e = s.P.Ephemeral.Set(ctx, presenceKey(scope, id, guest.ID), "1", 30*time.Second); e != nil {
		return nil, e
	}
	return map[string]any{"id": id, "resource_id": id, "subject_id": guest.ID, "role": role, "state": "paired", "credential": token}, nil
}

func (s *Service) cast(ctx context.Context, p control.Principal, method string, path []string, c command, key string) (any, error) {
	if len(path) == 1 && path[0] == "redeem" && method == "POST" {
		return s.redeem(ctx, c, "cast")
	}
	if len(path) == 1 && path[0] == "sessions" {
		if method == "POST" {
			return s.pairCreate(ctx, p, c, "cast")
		}
		if method == "GET" {
			return s.list(ctx, p, "cast")
		}
	}
	if len(path) < 2 || path[0] != "sessions" {
		return nil, control.Fail(404, "not_found")
	}
	id := path[1]
	if len(path) == 2 && method == "GET" {
		r, e := s.resource(ctx, p, id, "cast")
		if e != nil {
			return nil, e
		}
		if _, e = s.participant(ctx, p, id); e != nil {
			return nil, e
		}
		return r, nil
	}
	if len(path) != 3 || method != "POST" {
		return nil, control.Fail(404, "not_found")
	}
	action := path[2]
	if action == "heartbeat" {
		return s.heartbeat(ctx, p, id)
	}
	return s.P.Mutate(ctx, p, "cast:"+id+":"+action, key, c, func(t *persistence.Tx) (any, error) {
		r, e := s.inResource(ctx, t, p, id, "cast")
		if e != nil {
			return nil, e
		}
		m, e := participantTx(ctx, t, id, p.ID)
		if e != nil || m.State != "active" {
			return nil, control.Fail(403, "not_a_participant")
		}
		switch action {
		case "stop":
			return s.finish(ctx, t, p, r, "ended")
		case "connect":
			if r.State != "paired" && r.State != "streaming" {
				return nil, control.Fail(409, "not_paired")
			}
			caps := control.Capabilities{Publish: m.Role == "sender", Subscribe: m.Role == "receiver", Sources: []string{"screen_share", "screen_share_audio"}}
			g, e := s.P.Grant(ctx, t, r, p.ID, m.Role, "cast", caps)
			if e != nil {
				return nil, e
			}
			return map[string]any{"grant_id": g.ID, "participant_identity": g.Identity, "role": m.Role}, nil
		default:
			return nil, control.Fail(404, "not_found")
		}
	})
}
