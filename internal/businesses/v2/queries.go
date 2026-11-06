package v2

import (
	"av-platform/business/internal/platform/control"
	"context"
	"strings"
)

func (s *Service) identities(ctx context.Context, scope, id, purpose string) (map[string]string, error) {
	rows, e := s.P.Store.DB.QueryContext(ctx, s.P.Store.Q("SELECT g.subject_id,g.identity FROM av_rtc_grants g JOIN av_room_bindings b ON b.id=g.binding_id WHERE g.scope=? AND g.resource_id=? AND g.status='active' AND b.purpose=? ORDER BY g.created,g.id"), scope, id, purpose)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var who, identity string
		if e = rows.Scan(&who, &identity); e != nil {
			return nil, e
		}
		out[who] = identity
	}
	return out, rows.Err()
}
func (s *Service) meetingList(ctx context.Context, p control.Principal) (any, error) {
	if !p.IsUser() || !strings.HasPrefix(p.Scope, "org:") {
		return nil, control.Fail(403, "organization_required")
	}
	rows, e := s.P.Store.DB.QueryContext(ctx, s.P.Store.Q("SELECT r.id,r.title,r.state,r.owner,r.version,m.number,m.starts,m.duration,m.host FROM av_resources r JOIN av_meetings m ON m.scope=r.scope AND m.id=r.id WHERE r.scope=? AND (r.owner=? OR EXISTS(SELECT 1 FROM av_participants p WHERE p.scope=r.scope AND p.resource_id=r.id AND p.subject_id=?)) ORDER BY r.created DESC,r.id LIMIT 200"), p.Scope, p.ID, p.ID)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, title, state, owner, number, host string
		var version, starts int64
		var duration int
		if e = rows.Scan(&id, &title, &state, &owner, &version, &number, &starts, &duration, &host); e != nil {
			return nil, e
		}
		items = append(items, map[string]any{"id": id, "title": title, "state": state, "owner": owner, "version": version, "number": number, "starts_at": starts, "duration_minutes": duration, "host": host})
	}
	return map[string]any{"items": items, "limit": 200, "offset": 0}, rows.Err()
}
func (s *Service) liveRooms(ctx context.Context, p control.Principal) (any, error) {
	if !p.IsUser() || !strings.HasPrefix(p.Scope, "org:") {
		return nil, control.Fail(403, "organization_required")
	}
	rows, e := s.P.Store.DB.QueryContext(ctx, s.P.Store.Q("SELECT r.id,r.title,r.owner,l.current_broadcast,COALESCE(b.state,'ended') FROM av_resources r JOIN av_live_rooms l ON l.scope=r.scope AND l.id=r.id LEFT JOIN av_resources b ON b.scope=l.scope AND b.id=l.current_broadcast WHERE r.scope=? ORDER BY r.created DESC,r.id LIMIT 200"), p.Scope)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, title, owner, broadcast, state string
		if e = rows.Scan(&id, &title, &owner, &broadcast, &state); e != nil {
			return nil, e
		}
		items = append(items, map[string]any{"id": id, "title": title, "owner": owner, "broadcast_id": broadcast, "state": state})
	}
	return map[string]any{"items": items, "limit": 200, "offset": 0}, rows.Err()
}

func (s *Service) liveAudience(ctx context.Context, scope, id, stageResource string, stageIdentities map[string]string) ([]map[string]any, int, bool, error) {
	rows, e := s.P.Store.DB.QueryContext(ctx, s.P.Store.Q("SELECT p.subject_id,p.role,p.state,m.muted FROM av_participants p JOIN av_live_members m ON m.scope=p.scope AND m.resource_id=p.resource_id AND m.subject_id=p.subject_id WHERE p.scope=? AND p.resource_id=? AND p.state IN ('active','accepted') AND m.banned=0 ORDER BY p.updated,p.subject_id LIMIT 1000"), scope, id)
	if e != nil {
		return nil, 0, false, e
	}
	out := []map[string]any{}
	for rows.Next() {
		var who, role, state string
		var muted int
		if e = rows.Scan(&who, &role, &state, &muted); e != nil {
			rows.Close()
			return nil, 0, false, e
		}
		out = append(out, map[string]any{"subject_id": who, "name": who, "role": role, "state": state, "muted": muted != 0})
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, 0, false, e
	}
	programIdentities, e := s.identities(ctx, scope, id, "program")
	if e != nil {
		return nil, 0, false, e
	}
	program, pe := s.P.Present(ctx, scope, id, "program")
	stage, se := s.P.Present(ctx, scope, stageResource, "stage")
	online := 0
	for _, v := range out {
		who := v["subject_id"].(string)
		identity := programIdentities[who]
		if identity == "" {
			identity = stageIdentities[who]
		}
		v["identity"] = identity
		connected := program[programIdentities[who]] || stage[stageIdentities[who]]
		v["online"] = connected
		if connected {
			online++
		}
	}
	return out, online, pe == nil && se == nil, nil
}
func (s *Service) broadcastBoard(ctx context.Context, scope, id string) ([]map[string]any, error) {
	rows, e := s.P.Store.DB.QueryContext(ctx, s.P.Store.Q("SELECT sender,SUM(amount) FROM av_live_gifts WHERE scope=? AND broadcast_id=? GROUP BY sender ORDER BY SUM(amount) DESC,sender LIMIT 20"), scope, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var who string
		var points int64
		if e = rows.Scan(&who, &points); e != nil {
			return nil, e
		}
		out = append(out, map[string]any{"subject_id": who, "name": who, "points": points})
	}
	return out, rows.Err()
}
