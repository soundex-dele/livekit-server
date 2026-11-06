package v2

import (
	"av-platform/business/internal/platform/control"
	"context"
	"database/sql"
	"errors"
	"platform-foundation/server/persistence"
	"time"
)

func (s *Service) pk(ctx context.Context, t *persistence.Tx, p control.Principal, r control.Resource, b broadcast, action, target string) (any, error) {
	id := b.PK
	if action == "pk-request" {
		if id != "" || target == r.ID || target == "" {
			return nil, control.Fail(409, "pk_unavailable")
		}
		other, e := s.P.ResourceTx(ctx, t, target)
		if e != nil {
			return nil, e
		}
		if other.Kind != "broadcast" || isTerminal(other.State) {
			return nil, control.Fail(409, "target_unavailable")
		}
		ob, e := broadcastTx(ctx, t, target)
		if e != nil {
			return nil, e
		}
		if ob.Host == b.Host || ob.PK != "" {
			return nil, control.Fail(409, "pk_unavailable")
		}
		var count int
		if e = t.Row(ctx, "SELECT COUNT(*) FROM av_live_members WHERE scope=? AND resource_id IN (?,?) AND stage IN ('pending','approved')", t.Scope, r.ID, target).Scan(&count); e != nil {
			return nil, e
		}
		if count != 0 {
			return nil, control.Fail(409, "stage_busy")
		}
		id = control.ID()
		if _, e = t.Exec(ctx, "INSERT INTO av_live_pk(id,scope,left_id,right_id,state,expires,created) VALUES(?,?,?,?,'pending',?,?)", id, t.Scope, r.ID, target, s.now().Add(time.Minute).UnixMilli(), s.now().UnixMilli()); e != nil {
			return nil, e
		}
		if _, e = t.Exec(ctx, "UPDATE av_broadcasts SET pk_id=? WHERE scope=? AND id IN (?,?)", id, t.Scope, r.ID, target); e != nil {
			return nil, e
		}
		for _, resource := range []control.Resource{r, other} {
			if e = s.emit(ctx, t, p, resource, "live.pk-requested", map[string]string{"pk_id": id, "left_id": r.ID, "right_id": target}); e != nil {
				return nil, e
			}
		}
		return map[string]any{"id": id, "state": "pending"}, nil
	}
	if id == "" {
		return nil, control.Fail(409, "no_pk")
	}
	var left, right, state string
	var expires int64
	if e := t.Row(ctx, "SELECT left_id,right_id,state,expires FROM av_live_pk WHERE scope=? AND id=?", t.Scope, id).Scan(&left, &right, &state, &expires); e != nil {
		return nil, e
	}
	if action == "pk-accept" {
		if r.ID != right || state != "pending" || expires <= s.now().UnixMilli() {
			return nil, control.Fail(409, "pk_not_pending")
		}
		if _, e := t.Exec(ctx, "UPDATE av_live_pk SET state='active',expires=? WHERE scope=? AND id=?", s.now().Add(5*time.Minute).UnixMilli(), t.Scope, id); e != nil {
			return nil, e
		}
		for _, pair := range [][2]string{{left, right}, {right, left}} {
			remote, e := broadcastTx(ctx, t, pair[1])
			if e != nil {
				return nil, e
			}
			if e = putParticipant(ctx, t, pair[0], remote.Host, "guest", "active", s.now().UnixMilli()); e != nil {
				return nil, e
			}
			if _, e = t.Exec(ctx, "INSERT INTO av_live_members(scope,resource_id,subject_id,stage) VALUES(?,?,?,'pk') ON CONFLICT(scope,resource_id,subject_id) DO UPDATE SET stage='pk'", t.Scope, pair[0], remote.Host); e != nil {
				return nil, e
			}
			resource, e := s.P.ResourceTx(ctx, t, pair[0])
			if e != nil {
				return nil, e
			}
			if e = s.emit(ctx, t, p, resource, "live.pk-active", map[string]string{"pk_id": id}); e != nil {
				return nil, e
			}
		}
		return map[string]any{"id": id, "state": "active", "left_id": left, "right_id": right}, nil
	}
	if action == "pk-reject" && r.ID != right || action == "pk-cancel" && r.ID != left {
		return nil, control.Fail(403, "pk_party_required")
	}
	if action != "pk-end" && state != "pending" {
		return nil, control.Fail(409, "pk_not_pending")
	}
	if e := s.finishPK(ctx, t, id, "ended", ""); e != nil {
		return nil, e
	}
	return map[string]any{"id": id, "state": "ended"}, nil
}
func (s *Service) finishPK(ctx context.Context, t *persistence.Tx, id, state, winner string) error {
	var left, right, current string
	var ls, rs int64
	e := t.Row(ctx, "SELECT left_id,right_id,state,left_score,right_score FROM av_live_pk WHERE scope=? AND id=?", t.Scope, id).Scan(&left, &right, &current, &ls, &rs)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	if current == "ended" {
		return nil
	}
	if winner == "" {
		if ls > rs {
			winner = left
		} else if rs > ls {
			winner = right
		} else {
			winner = "draw"
		}
	}
	if _, e = t.Exec(ctx, "UPDATE av_live_pk SET state=?,winner=? WHERE scope=? AND id=?", state, winner, t.Scope, id); e != nil {
		return e
	}
	if _, e = t.Exec(ctx, "UPDATE av_broadcasts SET pk_id='' WHERE scope=? AND pk_id=?", t.Scope, id); e != nil {
		return e
	}
	for _, pair := range [][2]string{{left, right}, {right, left}} {
		other, e := broadcastTx(ctx, t, pair[1])
		if e != nil {
			return e
		}
		if _, e = t.Exec(ctx, "UPDATE av_live_members SET stage='' WHERE scope=? AND resource_id=? AND subject_id=? AND stage='pk'", t.Scope, pair[0], other.Host); e != nil {
			return e
		}
		if e = putParticipant(ctx, t, pair[0], other.Host, "viewer", "active", s.now().UnixMilli()); e != nil {
			return e
		}
		if e = s.P.Revoke(ctx, t, pair[0], other.Host); e != nil {
			return e
		}
		r, e := s.P.ResourceTx(ctx, t, pair[0])
		if e != nil {
			return e
		}
		if e = s.P.Emit(ctx, t, "system", r.ID, "live.pk-ended", map[string]string{"pk_id": id, "winner": winner}); e != nil {
			return e
		}
	}
	return nil
}
