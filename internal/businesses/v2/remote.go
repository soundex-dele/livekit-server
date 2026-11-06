package v2

import (
	"av-platform/business/internal/platform/control"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"platform-foundation/server/persistence"
)

type controlGrant struct {
	ID                 string `json:"id"`
	Scope              string `json:"-"`
	Resource           string `json:"resource_id"`
	Host               string `json:"host"`
	Controller         string `json:"controller"`
	State              string `json:"state"`
	Epoch              int64  `json:"epoch"`
	Expires            int64  `json:"expires"`
	Created            int64  `json:"created"`
	HostIdentity       string `json:"host_identity,omitempty"`
	ControllerIdentity string `json:"controller_identity,omitempty"`
	ControllerName     string `json:"controller_name,omitempty"`
}

const grantCols = "id,scope,resource_id,host,controller,state,epoch,expires,created"

func scanControl(row interface{ Scan(...any) error }) (controlGrant, error) {
	var g controlGrant
	e := row.Scan(&g.ID, &g.Scope, &g.Resource, &g.Host, &g.Controller, &g.State, &g.Epoch, &g.Expires, &g.Created)
	if errors.Is(e, sql.ErrNoRows) {
		e = control.Fail(404, "grant_not_found")
	}
	return g, e
}
func leaseKey(g controlGrant) string   { return fmt.Sprintf("control:%s:%d", g.ID, g.Epoch) }
func leaseValue(g controlGrant) string { return fmt.Sprintf("%s|%s|%d", g.Host, g.Controller, g.Epoch) }

func (s *Service) assist(ctx context.Context, p control.Principal, method string, path []string, c command, key string) (any, error) {
	if len(path) == 1 && method == "POST" {
		switch path[0] {
		case "sessions":
			return s.pairCreate(ctx, p, c, "assist")
		case "redeem":
			return s.redeem(ctx, c, "assist")
		}
	}
	if len(path) < 2 || path[0] != "sessions" {
		return nil, control.Fail(404, "not_found")
	}
	id := path[1]
	if len(path) == 2 && method == "GET" {
		r, e := s.resource(ctx, p, id, "assist")
		if e != nil {
			return nil, e
		}
		if _, e = s.participant(ctx, p, id); e != nil {
			return nil, e
		}
		var g controlGrant
		g, e = scanControl(s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT "+grantCols+" FROM av_control_grants WHERE scope=? AND resource_id=? ORDER BY created DESC LIMIT 1"), p.Scope, id))
		if e != nil {
			var n int
			if x := s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT COUNT(*) FROM av_control_grants WHERE scope=? AND resource_id=?"), p.Scope, id).Scan(&n); x != nil {
				return nil, x
			}
			if n == 0 {
				return map[string]any{"session": r}, nil
			}
			return nil, e
		}
		if e = s.controlDetails(ctx, &g); e != nil {
			return nil, e
		}
		return map[string]any{"session": r, "control_grant": g}, nil
	}
	if len(path) != 3 || method != "POST" {
		return nil, control.Fail(404, "not_found")
	}
	action := path[2]
	if action == "heartbeat" {
		return s.heartbeat(ctx, p, id)
	}
	return s.P.Mutate(ctx, p, "assist:"+id+":"+action, key, c, func(t *persistence.Tx) (any, error) {
		r, e := s.inResource(ctx, t, p, id, "assist")
		if e != nil {
			return nil, e
		}
		m, e := participantTx(ctx, t, id, p.ID)
		if e != nil || m.State != "active" {
			return nil, control.Fail(403, "not_a_participant")
		}
		var host, controller string
		var epoch int64
		if e = t.Row(ctx, "SELECT host,controller,epoch FROM av_assist WHERE scope=? AND id=?", p.Scope, id).Scan(&host, &controller, &epoch); e != nil {
			return nil, e
		}
		switch action {
		case "stop":
			if _, e = t.Exec(ctx, "UPDATE av_control_grants SET state='revoked' WHERE scope=? AND resource_id=? AND state IN ('pending','active')", p.Scope, id); e != nil {
				return nil, e
			}
			return s.finish(ctx, t, p, r, "ended")
		case "request":
			if p.ID != controller || r.State != "paired" && r.State != "active" {
				return nil, control.Fail(403, "controller_required")
			}
			var count int
			if e = t.Row(ctx, "SELECT COUNT(*) FROM av_control_grants WHERE scope=? AND resource_id=? AND state IN ('pending','active')", p.Scope, id).Scan(&count); e != nil {
				return nil, e
			}
			if count != 0 {
				return nil, control.Fail(409, "grant_already_exists")
			}
			g := controlGrant{ID: control.ID(), Scope: p.Scope, Resource: id, Host: host, Controller: controller, State: "pending", Epoch: epoch + 1, Expires: s.now().Add(time.Minute).UnixMilli(), Created: s.now().UnixMilli()}
			if _, e = t.Exec(ctx, "INSERT INTO av_control_grants("+grantCols+") VALUES(?,?,?,?,?,?,?,?,?)", g.ID, g.Scope, g.Resource, g.Host, g.Controller, g.State, g.Epoch, g.Expires, g.Created); e != nil {
				return nil, e
			}
			if e = s.emit(ctx, t, p, r, "control.requested", g); e != nil {
				return nil, e
			}
			return g, nil
		case "connect":
			g, e := scanControl(t.Row(ctx, "SELECT "+grantCols+" FROM av_control_grants WHERE scope=? AND resource_id=? AND state='active' AND expires>? ORDER BY created DESC LIMIT 1", p.Scope, id, s.now().UnixMilli()))
			if e != nil {
				return nil, e
			}
			if c.Epoch != 0 && c.Epoch != g.Epoch {
				return nil, control.Fail(409, "stale_epoch")
			}
			caps := control.Capabilities{Publish: p.ID == host, Subscribe: p.ID == controller, Data: true}
			if p.ID == host {
				caps.Sources = []string{"screen_share"}
			}
			rtc, e := s.P.Grant(ctx, t, r, p.ID, m.Role, "assist", caps)
			if e != nil {
				return nil, e
			}
			if e = s.controlIdentitiesTx(ctx, t, &g); e != nil {
				return nil, e
			}
			return map[string]any{"grant_id": rtc.ID, "control_grant_id": g.ID, "epoch": g.Epoch, "participant_identity": rtc.Identity, "host_identity": g.HostIdentity, "controller_identity": g.ControllerIdentity}, nil
		default:
			return nil, control.Fail(404, "not_found")
		}
	})
}

func (s *Service) controlGrant(ctx context.Context, p control.Principal, method string, path []string, c command, key string) (any, error) {
	started := s.now()
	if method != "POST" || len(path) != 2 {
		return nil, control.Fail(404, "not_found")
	}
	id, action := path[0], path[1]
	if action == "renew" {
		return s.renew(ctx, p, id, c.Epoch)
	}
	var changed controlGrant
	out, e := s.P.Mutate(ctx, p, "control:"+id+":"+action, key, c, func(t *persistence.Tx) (any, error) {
		g, e := scanControl(t.Row(ctx, "SELECT "+grantCols+" FROM av_control_grants WHERE scope=? AND id=?", p.Scope, id))
		if e != nil {
			return nil, e
		}
		r, e := s.inResource(ctx, t, p, g.Resource, "assist")
		if e != nil {
			return nil, e
		}
		if p.ID != g.Host && p.ID != g.Controller {
			return nil, control.Fail(403, "not_a_participant")
		}
		if c.Epoch != 0 && c.Epoch != g.Epoch {
			return nil, control.Fail(409, "stale_epoch")
		}
		switch action {
		case "approve":
			if p.ID != g.Host || !c.LocalConsent {
				return nil, control.Fail(403, "local_consent_required")
			}
			if g.State != "pending" || g.Expires <= s.now().UnixMilli() || isTerminal(r.State) {
				return nil, control.Fail(409, "request_expired")
			}
			g.State = "active"
			g.Expires = s.now().Add(time.Hour).UnixMilli()
			res, e := t.Exec(ctx, "UPDATE av_assist SET epoch=? WHERE scope=? AND id=? AND epoch<?", g.Epoch, p.Scope, g.Resource, g.Epoch)
			if e != nil {
				return nil, e
			}
			n, _ := res.RowsAffected()
			if n != 1 {
				return nil, control.Fail(409, "stale_epoch")
			}
			r.State = "active"
		case "reject":
			if p.ID != g.Host || g.State != "pending" {
				return nil, control.Fail(403, "host_required")
			}
			g.State = "rejected"
		case "revoke":
			g.State = "revoked"
			if e = s.P.Revoke(ctx, t, g.Resource, ""); e != nil {
				return nil, e
			}
			if !isTerminal(r.State) {
				r.State = "paired"
			}
		default:
			return nil, control.Fail(404, "not_found")
		}
		if _, e = t.Exec(ctx, "UPDATE av_control_grants SET state=?,expires=? WHERE scope=? AND id=?", g.State, g.Expires, p.Scope, g.ID); e != nil {
			return nil, e
		}
		r.Updated = s.now().UnixMilli()
		if e = s.P.UpdateResource(ctx, t, r); e != nil {
			return nil, e
		}
		if action == "approve" {
			for _, peer := range []struct {
				subject, role string
				caps          control.Capabilities
			}{{g.Host, "host", control.Capabilities{Publish: true, Data: true, Sources: []string{"screen_share"}}}, {g.Controller, "controller", control.Capabilities{Subscribe: true, Data: true}}} {
				if _, e = s.P.Grant(ctx, t, r, peer.subject, peer.role, "assist", peer.caps); e != nil {
					return nil, e
				}
			}
			if e = s.controlIdentitiesTx(ctx, t, &g); e != nil {
				return nil, e
			}
		}
		if e = s.emit(ctx, t, p, r, "control."+action, map[string]any{"grant_id": g.ID, "epoch": g.Epoch, "state": g.State}); e != nil {
			return nil, e
		}
		changed = g
		return g, nil
	})
	if e != nil {
		return nil, e
	}
	if action == "approve" {
		remaining := 10*time.Second - s.now().Sub(started)
		var ok bool
		if remaining > 0 {
			ok, e = s.P.Ephemeral.Create(ctx, leaseKey(changed), leaseValue(changed), remaining)
		}
		if e != nil || !ok {
			// An approval without a successfully created lease cannot be recovered
			// by renew. Mark it expired so the user may explicitly approve again.
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancel()
			_ = s.P.Store.Within(cleanup, p.Scope, func(t *persistence.Tx) error {
				_, x := t.Exec(cleanup, "UPDATE av_control_grants SET state='expired' WHERE scope=? AND id=? AND epoch=? AND state='active'", p.Scope, changed.ID, changed.Epoch)
				if x != nil {
					return x
				}
				return s.P.Revoke(cleanup, t, changed.Resource, "")
			})
			if e != nil {
				return nil, e
			}
			return nil, control.Fail(409, "lease_unavailable")
		}
		return map[string]any{"grant": changed, "lease_remaining_ms": remaining.Milliseconds(), "epoch": changed.Epoch}, nil
	}
	if changed.ID != "" {
		_ = s.P.Ephemeral.Delete(ctx, leaseKey(changed))
	}
	return out, nil
}

func (s *Service) controlIdentitiesTx(ctx context.Context, t *persistence.Tx, g *controlGrant) error {
	for _, peer := range []struct {
		subject string
		dst     *string
	}{{g.Host, &g.HostIdentity}, {g.Controller, &g.ControllerIdentity}} {
		e := t.Row(ctx, "SELECT identity FROM av_rtc_grants WHERE scope=? AND resource_id=? AND subject_id=? AND status='active' ORDER BY created DESC LIMIT 1", g.Scope, g.Resource, peer.subject).Scan(peer.dst)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
	}
	return t.Row(ctx, "SELECT controller_name FROM av_assist WHERE scope=? AND id=?", g.Scope, g.Resource).Scan(&g.ControllerName)
}
func (s *Service) controlDetails(ctx context.Context, g *controlGrant) error {
	for _, peer := range []struct {
		subject string
		dst     *string
	}{{g.Host, &g.HostIdentity}, {g.Controller, &g.ControllerIdentity}} {
		e := s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT identity FROM av_rtc_grants WHERE scope=? AND resource_id=? AND subject_id=? AND status='active' ORDER BY created DESC LIMIT 1"), g.Scope, g.Resource, peer.subject).Scan(peer.dst)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
	}
	return s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT controller_name FROM av_assist WHERE scope=? AND id=?"), g.Scope, g.Resource).Scan(&g.ControllerName)
}

func (s *Service) renew(ctx context.Context, p control.Principal, id string, epoch int64) (any, error) {
	start := s.now()
	g, e := scanControl(s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT "+grantCols+" FROM av_control_grants WHERE scope=? AND id=?"), p.Scope, id))
	if e != nil {
		return nil, e
	}
	if p.ID != g.Host || p.ResourceID != "" && p.ResourceID != g.Resource {
		return nil, control.Fail(403, "host_required")
	}
	if epoch != g.Epoch || g.State != "active" || g.Expires <= start.UnixMilli() {
		return nil, control.Fail(403, "control_grant_inactive")
	}
	r, e := s.resource(ctx, p, g.Resource, "assist")
	if e != nil {
		return nil, e
	}
	if r.State != "active" {
		return nil, control.Fail(409, "control_grant_inactive")
	}
	if !s.online(ctx, g.Scope, g.Resource, g.Host) || !s.online(ctx, g.Scope, g.Resource, g.Controller) {
		return nil, control.Fail(409, "peer_offline")
	}
	remaining := 10*time.Second - s.now().Sub(start)
	if cap := time.UnixMilli(g.Expires).Sub(s.now()); cap < remaining {
		remaining = cap
	}
	if remaining <= 0 {
		return nil, control.Fail(409, "lease_expired")
	}
	ok, e := s.P.Ephemeral.Renew(ctx, leaseKey(g), leaseValue(g), remaining)
	if e != nil {
		return nil, e
	}
	if !ok {
		return nil, control.Fail(409, "lease_expired")
	}
	return map[string]any{"grant_id": g.ID, "epoch": g.Epoch, "lease_remaining_ms": remaining.Milliseconds(), "scope": []string{"screen", "keyboard", "mouse"}}, nil
}
