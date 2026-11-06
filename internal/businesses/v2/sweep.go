package v2

import (
	"av-platform/business/internal/platform/control"
	"context"
	"database/sql"
	"errors"
	"platform-foundation/server/persistence"
)

// Sweep reconciles observations with durable state. Observations happen before
// short scope transactions. Unknown media/Redis state never ends a conference
// or broadcast; starting an API or worker is not itself a lifecycle event.
func (s *Service) Sweep(ctx context.Context) error {
	if h, ok := s.P.Ephemeral.(control.Health); ok {
		if e := h.Ping(ctx); e != nil {
			return e
		}
	}
	rows, e := s.P.Store.DB.QueryContext(ctx, "SELECT id,scope,kind,owner,title,state,version,created,updated FROM av_resources WHERE kind IN ('cast','assist','meeting','broadcast') AND state NOT IN ('ended','expired','cancelled','failed') ORDER BY id")
	if e != nil {
		return e
	}
	var resources []control.Resource
	for rows.Next() {
		var r control.Resource
		if e = rows.Scan(&r.ID, &r.Scope, &r.Kind, &r.Owner, &r.Title, &r.State, &r.Version, &r.Created, &r.Updated); e != nil {
			rows.Close()
			return e
		}
		resources = append(resources, r)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, r := range resources {
		if e = ctx.Err(); e != nil {
			return e
		}
		if !s.moduleForKind(r.Kind) {
			if e = s.closeDisabled(ctx, r); e != nil {
				return e
			}
			continue
		}
		switch r.Kind {
		case "cast", "assist":
			e = s.sweepPair(ctx, r)
		case "meeting":
			e = s.sweepMeeting(ctx, r)
		case "broadcast":
			e = s.sweepBroadcast(ctx, r)
		}
		if e != nil {
			return e
		}
	}
	if s.moduleForKind("broadcast") {
		return s.sweepPK(ctx)
	}
	return nil
}

func (s *Service) closeDisabled(ctx context.Context, observed control.Resource) error {
	return s.P.Store.Within(ctx, observed.Scope, func(t *persistence.Tx) error {
		r, e := s.P.ResourceTx(ctx, t, observed.ID)
		if e != nil || isTerminal(r.State) {
			return e
		}
		actor := control.Principal{ID: "system", Scope: r.Scope}
		if r.Kind == "assist" {
			if _, e = t.Exec(ctx, "UPDATE av_control_grants SET state='revoked' WHERE scope=? AND resource_id=? AND state IN ('pending','active')", t.Scope, r.ID); e != nil {
				return e
			}
		}
		if r.Kind == "broadcast" {
			b, e := broadcastTx(ctx, t, r.ID)
			if e != nil {
				return e
			}
			_, e = s.endBroadcast(ctx, t, actor, r, b)
			return e
		}
		_, e = s.finish(ctx, t, actor, r, "ended")
		return e
	})
}
func (s *Service) presence(ctx context.Context, scope, id, subject string) (bool, error) {
	if subject == "" {
		return false, nil
	}
	_, e := s.P.Ephemeral.Get(ctx, presenceKey(scope, id, subject))
	if errors.Is(e, control.ErrMissing) {
		return false, nil
	}
	return e == nil, e
}
func (s *Service) sweepPair(ctx context.Context, observed control.Resource) error {
	now := s.now()
	var a, b string
	var missing int64
	table := "av_cast"
	columns := "receiver,sender,missing"
	if observed.Kind == "assist" {
		table = "av_assist"
		columns = "host,controller,missing"
	}
	if e := s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT "+columns+" FROM "+table+" WHERE scope=? AND id=?"), observed.Scope, observed.ID).Scan(&a, &b, &missing); e != nil {
		return e
	}
	aliveA, e := s.presence(ctx, observed.Scope, observed.ID, a)
	if e != nil {
		return e
	}
	aliveB, e := s.presence(ctx, observed.Scope, observed.ID, b)
	if e != nil {
		return e
	}
	alive := aliveA && (b == "" || aliveB)
	published := false
	if observed.Kind == "cast" && observed.State == "paired" {
		published, _ = s.P.Published(ctx, observed.Scope, observed.ID, "cast")
	}
	var grant controlGrant
	leasePresent := false
	if observed.Kind == "assist" {
		var err error
		grant, err = scanControl(s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT "+grantCols+" FROM av_control_grants WHERE scope=? AND resource_id=? AND state IN ('active','pending') ORDER BY created DESC LIMIT 1"), observed.Scope, observed.ID))
		if err == nil && grant.State == "active" {
			value, err := s.P.Ephemeral.Get(ctx, leaseKey(grant))
			if err != nil && !errors.Is(err, control.ErrMissing) {
				return err
			}
			leasePresent = err == nil && value == leaseValue(grant)
		} else if err != nil {
			var n int
			if e = s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT COUNT(*) FROM av_control_grants WHERE scope=? AND resource_id=? AND state IN ('active','pending')"), observed.Scope, observed.ID).Scan(&n); e != nil {
				return e
			}
			if n > 0 {
				return err
			}
			grant = controlGrant{}
		}
	}
	return s.P.Store.Within(ctx, observed.Scope, func(t *persistence.Tx) error {
		r, e := s.P.ResourceTx(ctx, t, observed.ID)
		if e != nil {
			return e
		}
		if r.Version != observed.Version || isTerminal(r.State) {
			return nil
		}
		actor := control.Principal{ID: "system", Scope: r.Scope}
		if r.State == "waiting" {
			if now.UnixMilli() < r.Created+300000 {
				return nil
			}
			_, e = s.finish(ctx, t, actor, r, "expired")
			return e
		}
		if observed.Kind == "assist" && grant.ID != "" && (grant.Expires <= now.UnixMilli() || grant.State == "active" && !leasePresent) {
			res, e := t.Exec(ctx, "UPDATE av_control_grants SET state='expired' WHERE scope=? AND id=? AND state=? AND epoch=?", t.Scope, grant.ID, grant.State, grant.Epoch)
			if e != nil {
				return e
			}
			n, _ := res.RowsAffected()
			if n == 1 {
				if e = s.P.Revoke(ctx, t, r.ID, ""); e != nil {
					return e
				}
				if r.State == "active" {
					r.State = "paired"
					r.Updated = now.UnixMilli()
					if e = s.P.UpdateResource(ctx, t, r); e != nil {
						return e
					}
					r.Version++
				}
				if e = s.emit(ctx, t, actor, r, "control.expired", map[string]any{"grant_id": grant.ID, "epoch": grant.Epoch}); e != nil {
					return e
				}
			}
		}
		if published && r.State == "paired" {
			r.State = "streaming"
			r.Updated = now.UnixMilli()
			if e = s.P.UpdateResource(ctx, t, r); e != nil {
				return e
			}
			r.Version++
			if e = s.emit(ctx, t, actor, r, "cast.streaming", map[string]string{"state": "streaming"}); e != nil {
				return e
			}
		}
		if alive {
			if missing != 0 {
				_, e = t.Exec(ctx, "UPDATE "+table+" SET missing=0 WHERE scope=? AND id=?", t.Scope, r.ID)
			}
			return e
		}
		if missing == 0 {
			_, e = t.Exec(ctx, "UPDATE "+table+" SET missing=? WHERE scope=? AND id=? AND missing=0", now.UnixMilli(), t.Scope, r.ID)
			return e
		}
		if now.UnixMilli()-missing >= 30000 {
			if observed.Kind == "assist" {
				if _, e = t.Exec(ctx, "UPDATE av_control_grants SET state='revoked' WHERE scope=? AND resource_id=? AND state IN ('active','pending')", t.Scope, r.ID); e != nil {
					return e
				}
			}
			_, e = s.finish(ctx, t, actor, r, "expired")
			return e
		}
		return nil
	})
}

func (s *Service) sweepMeeting(ctx context.Context, observed control.Resource) error {
	now := s.now()
	m, e := scanMeeting(s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT "+meetingCols+" FROM av_meetings WHERE scope=? AND id=?"), observed.Scope, observed.ID))
	if e != nil {
		return e
	}
	if observed.State == "scheduled" {
		if now.UnixMilli() < m.Starts+int64(m.Duration)*60000 {
			return nil
		}
		return s.P.Store.Within(ctx, observed.Scope, func(t *persistence.Tx) error {
			r, e := s.P.ResourceTx(ctx, t, observed.ID)
			if e != nil || r.Version != observed.Version || r.State != "scheduled" {
				return e
			}
			_, e = s.finish(ctx, t, control.Principal{ID: "system", Scope: r.Scope}, r, "expired")
			return e
		})
	}
	media, e := s.P.Present(ctx, observed.Scope, observed.ID, "conference")
	if e != nil {
		return nil
	} // Unknown is not offline.
	identities, e := s.identities(ctx, observed.Scope, observed.ID, "conference")
	if e != nil {
		return e
	}
	rows, e := s.P.Store.DB.QueryContext(ctx, s.P.Store.Q("SELECT subject_id,role,state,policy_version,updated FROM av_participants WHERE scope=? AND resource_id=? AND state IN ('active','accepted','waiting','disconnected') ORDER BY updated,subject_id"), observed.Scope, observed.ID)
	if e != nil {
		return e
	}
	var members []participant
	for rows.Next() {
		var v participant
		if e = rows.Scan(&v.Subject, &v.Role, &v.State, &v.Policy, &v.Updated); e != nil {
			rows.Close()
			return e
		}
		members = append(members, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	alive := map[string]bool{}
	for _, v := range members {
		p, e := s.presence(ctx, observed.Scope, observed.ID, v.Subject)
		if e != nil {
			return e
		}
		alive[v.Subject] = p || media[identities[v.Subject]]
	}
	return s.P.Store.Within(ctx, observed.Scope, func(t *persistence.Tx) error {
		r, e := s.P.ResourceTx(ctx, t, observed.ID)
		if e != nil {
			return e
		}
		if r.Version != observed.Version || isTerminal(r.State) {
			return nil
		}
		m, e := scanMeeting(t.Row(ctx, "SELECT "+meetingCols+" FROM av_meetings WHERE scope=? AND id=?", t.Scope, r.ID))
		if e != nil {
			return e
		}
		changed := false
		for _, old := range members {
			v, e := participantTx(ctx, t, r.ID, old.Subject)
			if e != nil {
				return e
			}
			if v.State != old.State || v.Updated != old.Updated {
				continue
			}
			if alive[v.Subject] {
				if v.State == "disconnected" && v.Updated+120000 > now.UnixMilli() {
					if _, e = t.Exec(ctx, "UPDATE av_participants SET state='active',updated=? WHERE scope=? AND resource_id=? AND subject_id=?", now.UnixMilli(), t.Scope, r.ID, v.Subject); e != nil {
						return e
					}
					changed = true
				}
				continue
			}
			if v.State == "waiting" {
				if v.Updated+120000 <= now.UnixMilli() {
					if e = putParticipant(ctx, t, r.ID, v.Subject, v.Role, "expired", now.UnixMilli()); e != nil {
						return e
					}
					changed = true
				}
				continue
			}
			if now.UnixMilli()-v.Updated < 30000 {
				continue
			}
			if v.State != "disconnected" {
				if _, e = t.Exec(ctx, "UPDATE av_participants SET state='disconnected',updated=? WHERE scope=? AND resource_id=? AND subject_id=?", now.UnixMilli(), t.Scope, r.ID, v.Subject); e != nil {
					return e
				}
				changed = true
			} else if v.Updated+120000 <= now.UnixMilli() {
				if e = putParticipant(ctx, t, r.ID, v.Subject, v.Role, "expired", now.UnixMilli()); e != nil {
					return e
				}
				if e = s.P.Revoke(ctx, t, r.ID, v.Subject); e != nil {
					return e
				}
				changed = true
			}
			if m.Sharing == v.Subject {
				m.Sharing = ""
				if _, e = t.Exec(ctx, "UPDATE av_participants SET policy_version=policy_version+1 WHERE scope=? AND resource_id=? AND subject_id=?", t.Scope, r.ID, v.Subject); e != nil {
					return e
				}
				if e = s.P.Revoke(ctx, t, r.ID, v.Subject); e != nil {
					return e
				}
				changed = true
			}
		}
		if alive[m.Host] {
			if m.Missing != 0 {
				m.Missing = 0
				changed = true
			}
		} else if m.Missing == 0 {
			m.Missing = now.UnixMilli()
			changed = true
		} else if m.Missing+120000 <= now.UnixMilli() {
			var next string
			e = t.Row(ctx, "SELECT subject_id FROM av_participants WHERE scope=? AND resource_id=? AND subject_id<>? AND state IN ('active','accepted') ORDER BY updated,subject_id LIMIT 1", t.Scope, r.ID, m.Host).Scan(&next)
			if errors.Is(e, sql.ErrNoRows) {
				_, e = s.finish(ctx, t, control.Principal{ID: "system", Scope: r.Scope}, r, "ended")
				return e
			}
			if e != nil {
				return e
			}
			old := m.Host
			if _, e = t.Exec(ctx, "UPDATE av_participants SET role='member',policy_version=policy_version+1 WHERE scope=? AND resource_id=? AND subject_id=?", t.Scope, r.ID, old); e != nil {
				return e
			}
			if _, e = t.Exec(ctx, "UPDATE av_participants SET role='host',policy_version=policy_version+1 WHERE scope=? AND resource_id=? AND subject_id=?", t.Scope, r.ID, next); e != nil {
				return e
			}
			if e = s.P.Revoke(ctx, t, r.ID, old); e != nil {
				return e
			}
			if e = s.P.Revoke(ctx, t, r.ID, next); e != nil {
				return e
			}
			m.Host, m.Missing = next, 0
			changed = true
		}
		if !changed {
			return nil
		}
		if _, e = t.Exec(ctx, "UPDATE av_meetings SET host=?,host_missing=?,sharing=? WHERE scope=? AND id=?", m.Host, m.Missing, m.Sharing, t.Scope, r.ID); e != nil {
			return e
		}
		r.Updated = now.UnixMilli()
		if e = s.P.UpdateResource(ctx, t, r); e != nil {
			return e
		}
		return s.emit(ctx, t, control.Principal{ID: "system", Scope: r.Scope}, r, "meeting.presence", map[string]any{"host": m.Host, "sharing_member": m.Sharing})
	})
}

func (s *Service) sweepBroadcast(ctx context.Context, observed control.Resource) error {
	published, e := s.P.Published(ctx, observed.Scope, observed.ID, "program")
	if e != nil {
		return nil
	}
	now := s.now()
	return s.P.Store.Within(ctx, observed.Scope, func(t *persistence.Tx) error {
		r, e := s.P.ResourceTx(ctx, t, observed.ID)
		if e != nil {
			return e
		}
		if r.Version != observed.Version || isTerminal(r.State) {
			return nil
		}
		b, e := broadcastTx(ctx, t, r.ID)
		if e != nil {
			return e
		}
		var missing int64
		if e = t.Row(ctx, "SELECT missing FROM av_broadcasts WHERE scope=? AND id=?", t.Scope, r.ID).Scan(&missing); e != nil {
			return e
		}
		state := r.State
		if published {
			if missing != 0 {
				if _, e = t.Exec(ctx, "UPDATE av_broadcasts SET missing=0 WHERE scope=? AND id=?", t.Scope, r.ID); e != nil {
					return e
				}
			}
			r.State = "live"
		} else {
			if missing == 0 {
				missing = now.UnixMilli()
				if _, e = t.Exec(ctx, "UPDATE av_broadcasts SET missing=? WHERE scope=? AND id=?", missing, t.Scope, r.ID); e != nil {
					return e
				}
			}
			if now.UnixMilli()-missing >= 60000 {
				_, e = s.endBroadcast(ctx, t, control.Principal{ID: "system", Scope: r.Scope}, r, b)
				return e
			}
			if r.State == "live" {
				r.State = "reconnecting"
			}
		}
		if state == r.State {
			return nil
		}
		r.Updated = now.UnixMilli()
		if e = s.P.UpdateResource(ctx, t, r); e != nil {
			return e
		}
		return s.emit(ctx, t, control.Principal{ID: "system", Scope: r.Scope}, r, "live.state", map[string]string{"state": r.State})
	})
}
func (s *Service) sweepPK(ctx context.Context) error {
	rows, e := s.P.Store.DB.QueryContext(ctx, "SELECT id,scope FROM av_live_pk WHERE state IN ('pending','active') ORDER BY id")
	if e != nil {
		return e
	}
	type item struct{ id, scope string }
	var items []item
	for rows.Next() {
		var v item
		if e = rows.Scan(&v.id, &v.scope); e != nil {
			rows.Close()
			return e
		}
		items = append(items, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, v := range items {
		e = s.P.Store.Within(ctx, v.scope, func(t *persistence.Tx) error {
			var state, left, right string
			var expires, lmissing, rmissing int64
			if e := t.Row(ctx, "SELECT p.state,p.left_id,p.right_id,p.expires,l.missing,r.missing FROM av_live_pk p JOIN av_broadcasts l ON l.scope=p.scope AND l.id=p.left_id JOIN av_broadcasts r ON r.scope=p.scope AND r.id=p.right_id WHERE p.scope=? AND p.id=?", t.Scope, v.id).Scan(&state, &left, &right, &expires, &lmissing, &rmissing); e != nil {
				return e
			}
			if state != "pending" && state != "active" {
				return nil
			}
			winner := ""
			now := s.now().UnixMilli()
			lostLeft := lmissing != 0 && lmissing+30000 <= now
			lostRight := rmissing != 0 && rmissing+30000 <= now
			if state == "active" && (lostLeft || lostRight) {
				if lostLeft && !lostRight {
					winner = right
				} else if lostRight && !lostLeft {
					winner = left
				} else {
					winner = "draw"
				}
			} else if expires > now {
				return nil
			}
			return s.finishPK(ctx, t, v.id, "ended", winner)
		})
		if e != nil {
			return e
		}
	}
	return nil
}
