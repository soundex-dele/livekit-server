package v2

import (
	"av-platform/business/internal/platform/control"
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"platform-foundation/server/persistence"
)

type broadcast struct {
	Room, Host, PK string
	Started        int64
}

func broadcastTx(ctx context.Context, t *persistence.Tx, id string) (broadcast, error) {
	var b broadcast
	e := t.Row(ctx, "SELECT room_id,host,pk_id,started FROM av_broadcasts WHERE scope=? AND id=?", t.Scope, id).Scan(&b.Room, &b.Host, &b.PK, &b.Started)
	return b, e
}
func day(t time.Time) string { return t.In(time.FixedZone("UTC+8", 8*3600)).Format("2006-01-02") }
func (s *Service) live(ctx context.Context, p control.Principal, method string, path []string, c command, key string) (any, error) {
	if len(path) == 1 {
		switch path[0] {
		case "rooms":
			if method == "GET" {
				return s.liveRooms(ctx, p)
			}
			if method == "POST" {
				return s.createLiveRoom(ctx, p, c, key)
			}
		case "wallet":
			if method == "GET" {
				return s.wallet(ctx, p)
			}
		case "board":
			if method == "GET" {
				return s.board(ctx, p)
			}
		}
	}
	if len(path) < 2 {
		return nil, control.Fail(404, "not_found")
	}
	id := path[1]
	if path[0] == "rooms" {
		if len(path) == 2 && method == "GET" {
			return s.publicRoom(ctx, id)
		}
		if len(path) == 3 && path[2] == "start" && method == "POST" {
			return s.startBroadcast(ctx, p, id, c, key)
		}
		return nil, control.Fail(404, "not_found")
	}
	if path[0] != "broadcasts" {
		return nil, control.Fail(404, "not_found")
	}
	if len(path) == 2 && method == "GET" {
		return s.liveSnapshot(ctx, p, id)
	}
	if len(path) != 3 || method != "POST" {
		return nil, control.Fail(404, "not_found")
	}
	action := path[2]
	if action == "watch" {
		return s.watch(ctx, p, id)
	}
	if action == "heartbeat" {
		return s.heartbeat(ctx, p, id)
	}
	if action == "chat" {
		if !p.IsUser() {
			return nil, control.Fail(403, "login_required")
		}
		if utf8.RuneCountInString(c.Content) > 300 || strings.TrimSpace(c.Content) == "" {
			return nil, control.Fail(400, "invalid_chat")
		}
		ok, e := s.P.Ephemeral.Allow(ctx, "chat:"+p.Scope+":"+p.ID, 30, time.Minute)
		if e != nil {
			return nil, e
		}
		if !ok {
			return nil, control.Fail(429, "rate_limited")
		}
	}
	return s.P.Mutate(ctx, p, "live:"+id+":"+action, key, c, func(t *persistence.Tx) (any, error) {
		r, e := s.inResource(ctx, t, p, id, "broadcast")
		if e != nil {
			return nil, e
		}
		b, e := broadcastTx(ctx, t, id)
		if e != nil {
			return nil, e
		}
		v, e := participantTx(ctx, t, id, p.ID)
		if e != nil || v.State != "active" && v.State != "accepted" {
			return nil, control.Fail(403, "not_a_participant")
		}
		host := p.ID == b.Host
		if isTerminal(r.State) {
			if action == "end" {
				return map[string]any{"id": id, "state": r.State}, nil
			}
			return nil, control.Fail(409, "broadcast_ended")
		}
		var muted, banned int
		var stage string
		if e = t.Row(ctx, "SELECT muted,banned,stage FROM av_live_members WHERE scope=? AND resource_id=? AND subject_id=?", p.Scope, id, p.ID).Scan(&muted, &banned, &stage); e != nil {
			return nil, e
		}
		if banned != 0 {
			return nil, control.Fail(403, "viewer_banned")
		}
		target := c.Target
		switch action {
		case "connect":
			return s.liveGrant(ctx, t, r, b, p.ID, c.Stage)
		case "end":
			if !host && !p.Can("av.manage") {
				return nil, control.Fail(403, "host_required")
			}
			return s.endBroadcast(ctx, t, p, r, b)
		case "chat":
			if muted != 0 {
				return nil, control.Fail(403, "chat_muted")
			}
			chat := control.ID()
			if _, e = t.Exec(ctx, "INSERT INTO av_live_chat(id,scope,resource_id,subject_id,body,created) VALUES(?,?,?,?,?,?)", chat, p.Scope, id, p.ID, strings.TrimSpace(c.Content), s.now().UnixMilli()); e != nil {
				return nil, e
			}
			out := map[string]any{"id": chat, "subject_id": p.ID, "content": strings.TrimSpace(c.Content)}
			if e = s.emit(ctx, t, p, r, "live.chat", out); e != nil {
				return nil, e
			}
			return out, nil
		case "gift":
			return s.gift(ctx, t, p, r, b, c.Amount)
		case "stage-request":
			if !p.IsUser() || host {
				return nil, control.Fail(403, "member_required")
			}
			if b.PK != "" {
				return nil, control.Fail(409, "pk_active")
			}
			if stage != "" && stage != "rejected" {
				return nil, control.Fail(409, "stage_request_exists")
			}
			if _, e = t.Exec(ctx, "UPDATE av_live_members SET stage='pending' WHERE scope=? AND resource_id=? AND subject_id=?", p.Scope, id, p.ID); e != nil {
				return nil, e
			}
			target = p.ID
		case "stage-approve", "stage-reject":
			if !host {
				return nil, control.Fail(403, "host_required")
			}
			if b.PK != "" {
				return nil, control.Fail(409, "pk_active")
			}
			var st string
			if e = t.Row(ctx, "SELECT stage FROM av_live_members WHERE scope=? AND resource_id=? AND subject_id=? AND banned=0", p.Scope, id, target).Scan(&st); e != nil || st != "pending" {
				return nil, control.Fail(409, "not_waiting")
			}
			next := "rejected"
			if action == "stage-approve" {
				var count int
				if e = t.Row(ctx, "SELECT COUNT(*) FROM av_live_members WHERE scope=? AND resource_id=? AND stage='approved'", p.Scope, id).Scan(&count); e != nil {
					return nil, e
				}
				if count >= 3 {
					return nil, control.Fail(409, "stage_full")
				}
				next = "approved"
			}
			if _, e = t.Exec(ctx, "UPDATE av_live_members SET stage=? WHERE scope=? AND resource_id=? AND subject_id=?", next, p.Scope, id, target); e != nil {
				return nil, e
			}
			if action == "stage-approve" {
				if e = putParticipant(ctx, t, id, target, "guest", "active", s.now().UnixMilli()); e != nil {
					return nil, e
				}
				if e = s.P.Revoke(ctx, t, id, target); e != nil {
					return nil, e
				}
			}
		case "stage-leave", "stage-remove":
			if action == "stage-leave" {
				target = p.ID
			} else if !host {
				return nil, control.Fail(403, "host_required")
			}
			if target == b.Host {
				return nil, control.Fail(409, "cannot_remove_host")
			}
			v, e := participantTx(ctx, t, id, target)
			if e != nil || v.State != "active" && v.State != "accepted" {
				return nil, control.Fail(404, "participant_not_found")
			}
			if _, e = t.Exec(ctx, "UPDATE av_live_members SET stage='' WHERE scope=? AND resource_id=? AND subject_id=?", p.Scope, id, target); e != nil {
				return nil, e
			}
			if e = putParticipant(ctx, t, id, target, "viewer", "active", s.now().UnixMilli()); e != nil {
				return nil, e
			}
			if e = s.P.Revoke(ctx, t, id, target); e != nil {
				return nil, e
			}
		case "mute", "kick":
			if !host && !p.Can("av.manage") {
				return nil, control.Fail(403, "host_required")
			}
			if target == b.Host {
				return nil, control.Fail(409, "cannot_remove_host")
			}
			if _, e = participantTx(ctx, t, id, target); e != nil {
				return nil, control.Fail(404, "participant_not_found")
			}
			if action == "mute" {
				_, e = t.Exec(ctx, "UPDATE av_live_members SET muted=? WHERE scope=? AND resource_id=? AND subject_id=?", boolInt(c.Enabled), p.Scope, id, target)
			} else {
				_, e = t.Exec(ctx, "UPDATE av_live_members SET banned=1,stage='' WHERE scope=? AND resource_id=? AND subject_id=?", p.Scope, id, target)
				if e == nil {
					e = putParticipant(ctx, t, id, target, "viewer", "removed", s.now().UnixMilli())
				}
				if e == nil {
					e = s.P.Revoke(ctx, t, id, target)
				}
			}
			if e != nil {
				return nil, e
			}
		case "chat-delete":
			if !host && !p.Can("av.manage") {
				return nil, control.Fail(403, "host_required")
			}
			res, e := t.Exec(ctx, "UPDATE av_live_chat SET deleted=1 WHERE scope=? AND resource_id=? AND id=?", p.Scope, id, target)
			if e != nil {
				return nil, e
			}
			n, _ := res.RowsAffected()
			if n != 1 {
				return nil, control.Fail(404, "chat_not_found")
			}
		case "pk-request", "pk-accept", "pk-reject", "pk-cancel", "pk-end":
			if !host {
				return nil, control.Fail(403, "host_required")
			}
			return s.pk(ctx, t, p, r, b, action, target)
		case "leave":
			if host {
				return s.endBroadcast(ctx, t, p, r, b)
			}
			if e = putParticipant(ctx, t, id, p.ID, v.Role, "left", s.now().UnixMilli()); e != nil {
				return nil, e
			}
			if e = s.P.Revoke(ctx, t, id, p.ID); e != nil {
				return nil, e
			}
			if _, e = t.Exec(ctx, "UPDATE av_live_members SET stage='' WHERE scope=? AND resource_id=? AND subject_id=?", p.Scope, id, p.ID); e != nil {
				return nil, e
			}
			target = p.ID
		default:
			return nil, control.Fail(404, "not_found")
		}
		r.Updated = s.now().UnixMilli()
		if e = s.P.UpdateResource(ctx, t, r); e != nil {
			return nil, e
		}
		out := map[string]any{"id": id, "action": action, "target": target, "version": r.Version + 1}
		if e = s.emit(ctx, t, p, r, "live."+action, out); e != nil {
			return nil, e
		}
		return out, nil
	})
}

func (s *Service) createLiveRoom(ctx context.Context, p control.Principal, c command, key string) (any, error) {
	if e := permission(p, "av.live-studio.create"); e != nil {
		return nil, e
	}
	if !validTitle(c.Title) {
		return nil, control.Fail(400, "invalid_title")
	}
	return s.P.Mutate(ctx, p, "live:room:create", key, c, func(t *persistence.Tx) (any, error) {
		var existing string
		e := t.Row(ctx, "SELECT id FROM av_live_rooms WHERE scope=? AND owner=?", p.Scope, p.ID).Scan(&existing)
		if e == nil {
			return map[string]any{"id": existing}, nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
		r := control.Resource{ID: control.ID(), Scope: p.Scope, Kind: "live_room", Owner: p.ID, Title: c.Title, State: "idle", Version: 1, Created: s.now().UnixMilli(), Updated: s.now().UnixMilli()}
		if e = s.P.InsertResource(ctx, t, r); e != nil {
			return nil, e
		}
		if _, e = t.Exec(ctx, "INSERT INTO av_live_rooms(scope,id,owner) VALUES(?,?,?)", p.Scope, r.ID, p.ID); e != nil {
			return nil, e
		}
		if e = s.emit(ctx, t, p, r, "live.room-created", map[string]string{"id": r.ID}); e != nil {
			return nil, e
		}
		return r, nil
	})
}
func (s *Service) startBroadcast(ctx context.Context, p control.Principal, id string, c command, key string) (any, error) {
	if e := permission(p, "av.live-studio.create"); e != nil {
		return nil, e
	}
	return s.P.Mutate(ctx, p, "live:start:"+id, key, c, func(t *persistence.Tx) (any, error) {
		room, e := s.inResource(ctx, t, p, id, "live_room")
		if e != nil {
			return nil, e
		}
		if room.Owner != p.ID {
			return nil, control.Fail(403, "host_required")
		}
		var current string
		if e = t.Row(ctx, "SELECT current_broadcast FROM av_live_rooms WHERE scope=? AND id=?", p.Scope, id).Scan(&current); e != nil {
			return nil, e
		}
		if current != "" {
			r, e := s.P.ResourceTx(ctx, t, current)
			if e != nil {
				return nil, e
			}
			if !isTerminal(r.State) {
				return nil, control.Fail(409, "already_live")
			}
		}
		var count int
		if e = t.Row(ctx, "SELECT COUNT(*) FROM av_resources WHERE scope=? AND kind='broadcast' AND state IN ('starting','live','reconnecting')", p.Scope).Scan(&count); e != nil {
			return nil, e
		}
		if count >= 5 {
			return nil, control.Fail(409, "broadcast_quota_exceeded")
		}
		title := room.Title
		if c.Title != "" {
			if !validTitle(c.Title) {
				return nil, control.Fail(400, "invalid_title")
			}
			title = c.Title
		}
		r := control.Resource{ID: control.ID(), Scope: p.Scope, Kind: "broadcast", Owner: p.ID, Title: title, State: "starting", Version: 1, Created: s.now().UnixMilli(), Updated: s.now().UnixMilli()}
		if e = s.P.InsertResource(ctx, t, r); e != nil {
			return nil, e
		}
		if _, e = t.Exec(ctx, "INSERT INTO av_broadcasts(scope,id,room_id,host,started) VALUES(?,?,?,?,?)", p.Scope, r.ID, id, p.ID, s.now().UnixMilli()); e != nil {
			return nil, e
		}
		if _, e = t.Exec(ctx, "UPDATE av_live_rooms SET current_broadcast=? WHERE scope=? AND id=?", r.ID, p.Scope, id); e != nil {
			return nil, e
		}
		if e = putParticipant(ctx, t, r.ID, p.ID, "host", "active", s.now().UnixMilli()); e != nil {
			return nil, e
		}
		if _, e = t.Exec(ctx, "INSERT INTO av_live_members(scope,resource_id,subject_id,stage) VALUES(?,?,?,'host')", p.Scope, r.ID, p.ID); e != nil {
			return nil, e
		}
		b := broadcast{Room: id, Host: p.ID, Started: s.now().UnixMilli()}
		g, e := s.liveGrant(ctx, t, r, b, p.ID, false)
		if e != nil {
			return nil, e
		}
		stage, e := s.liveGrant(ctx, t, r, b, p.ID, true)
		if e != nil {
			return nil, e
		}
		out := map[string]any{"id": r.ID, "broadcast_id": r.ID, "room_id": id, "state": r.State, "grant_id": g.(map[string]any)["grant_id"], "stage_grant_id": stage.(map[string]any)["grant_id"]}
		if e = s.emit(ctx, t, p, r, "live.started", out); e != nil {
			return nil, e
		}
		return out, nil
	})
}
func (s *Service) publicRoom(ctx context.Context, id string) (any, error) {
	var title, broadcast, state string
	e := s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT r.title,l.current_broadcast,r.state FROM av_live_rooms l JOIN av_resources r ON l.scope=r.scope AND l.id=r.id WHERE l.id=?"), id).Scan(&title, &broadcast, &state)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, control.Fail(404, "room_not_found")
	}
	if e != nil {
		return nil, e
	}
	if broadcast != "" {
		if e = s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT state FROM av_resources WHERE id=?"), broadcast).Scan(&state); e != nil {
			return nil, e
		}
	}
	return map[string]any{"id": id, "title": title, "broadcast_id": broadcast, "state": state}, nil
}
func (s *Service) watch(ctx context.Context, p control.Principal, id string) (any, error) {
	var scope string
	e := s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT scope FROM av_broadcasts WHERE id=?"), id).Scan(&scope)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, control.Fail(404, "broadcast_not_found")
	}
	if e != nil {
		return nil, e
	}
	var out any
	e = s.P.Store.Within(ctx, scope, func(t *persistence.Tx) error {
		r, e := s.P.ResourceTx(ctx, t, id)
		if e != nil {
			return e
		}
		if isTerminal(r.State) {
			return control.Fail(409, "broadcast_ended")
		}
		b, e := broadcastTx(ctx, t, id)
		if e != nil {
			return e
		}
		actor := p
		token := ""
		if p.ID == "" || p.Scope != scope || p.ResourceID != "" && p.ResourceID != id {
			token, actor, e = s.P.IssueGuest(ctx, t, scope, id, "viewer")
			if e != nil {
				return e
			}
		}
		var banned int
		e = t.Row(ctx, "SELECT banned FROM av_live_members WHERE scope=? AND resource_id=? AND subject_id=?", scope, id, actor.ID).Scan(&banned)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if banned != 0 {
			return control.Fail(403, "viewer_banned")
		}
		v, e := participantTx(ctx, t, id, actor.ID)
		if errors.Is(e, sql.ErrNoRows) || e == nil && v.State == "left" {
			if e = putParticipant(ctx, t, id, actor.ID, "viewer", "active", s.now().UnixMilli()); e != nil {
				return e
			}
		} else if e != nil {
			return e
		} else if v.State == "removed" {
			return control.Fail(403, "viewer_banned")
		}
		if _, e = t.Exec(ctx, "INSERT INTO av_live_members(scope,resource_id,subject_id) VALUES(?,?,?) ON CONFLICT(scope,resource_id,subject_id) DO NOTHING", scope, id, actor.ID); e != nil {
			return e
		}
		g, e := s.liveGrant(ctx, t, r, b, actor.ID, false)
		if e != nil {
			return e
		}
		result := g.(map[string]any)
		result["id"], result["resource_id"], result["subject_id"] = id, id, actor.ID
		if token != "" {
			result["credential"] = token
		}
		out = result
		return nil
	})
	return out, e
}
func (s *Service) liveGrant(ctx context.Context, t *persistence.Tx, r control.Resource, b broadcast, subject string, stage bool) (any, error) {
	// PK uses the left broadcast's stage as its single interactive room. Each
	// host still publishes the final composite to their own program room.
	if stage && subject == b.Host && b.PK != "" {
		var left, right, state string
		if e := t.Row(ctx, "SELECT left_id,right_id,state FROM av_live_pk WHERE scope=? AND id=?", t.Scope, b.PK).Scan(&left, &right, &state); e != nil {
			return nil, e
		}
		if state == "active" && r.ID == right {
			canonical, e := s.P.ResourceTx(ctx, t, left)
			if e != nil {
				return nil, e
			}
			owner, e := broadcastTx(ctx, t, left)
			if e != nil {
				return nil, e
			}
			return s.liveGrant(ctx, t, canonical, owner, subject, true)
		}
	}
	purpose := "program"
	role := "viewer"
	caps := control.Capabilities{Subscribe: true}
	if subject == b.Host {
		role = "host"
		caps = control.Capabilities{Publish: true, Sources: []string{"camera", "microphone", "screen_share", "screen_share_audio"}}
	}
	if stage {
		purpose = "stage"
		var status string
		if e := t.Row(ctx, "SELECT stage FROM av_live_members WHERE scope=? AND resource_id=? AND subject_id=? AND banned=0", t.Scope, r.ID, subject).Scan(&status); e != nil {
			return nil, e
		}
		if subject != b.Host && status != "approved" && status != "pk" {
			return nil, control.Fail(403, "stage_not_approved")
		}
		if subject != b.Host {
			role = "guest"
		}
		caps = control.Capabilities{Publish: true, Subscribe: true, Sources: []string{"camera", "microphone", "screen_share", "screen_share_audio"}}
	}
	g, e := s.P.Grant(ctx, t, r, subject, role, purpose, caps)
	if e != nil {
		return nil, e
	}
	return map[string]any{"grant_id": g.ID, "participant_identity": g.Identity, "purpose": purpose, "role": role, "resource_id": r.ID}, nil
}
func (s *Service) endBroadcast(ctx context.Context, t *persistence.Tx, p control.Principal, r control.Resource, b broadcast) (any, error) {
	if b.PK != "" {
		if e := s.finishPK(ctx, t, b.PK, "ended", ""); e != nil {
			return nil, e
		}
	}
	if _, e := t.Exec(ctx, "UPDATE av_live_rooms SET current_broadcast='' WHERE scope=? AND id=? AND current_broadcast=?", t.Scope, b.Room, r.ID); e != nil {
		return nil, e
	}
	return s.finish(ctx, t, p, r, "ended")
}
func (s *Service) gift(ctx context.Context, t *persistence.Tx, p control.Principal, r control.Resource, b broadcast, amount int64) (any, error) {
	if !p.IsUser() || p.ID == b.Host {
		return nil, control.Fail(403, "gift_not_allowed")
	}
	if amount != 1 && amount != 10 && amount != 50 {
		return nil, control.Fail(400, "invalid_gift")
	}
	d := day(s.now())
	if _, e := t.Exec(ctx, "INSERT INTO av_live_wallets(scope,subject_id,day,balance) VALUES(?,?,?,100) ON CONFLICT(scope,subject_id,day) DO NOTHING", p.Scope, p.ID, d); e != nil {
		return nil, e
	}
	res, e := t.Exec(ctx, "UPDATE av_live_wallets SET balance=balance-? WHERE scope=? AND subject_id=? AND day=? AND balance>=?", amount, p.Scope, p.ID, d, amount)
	if e != nil {
		return nil, e
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return nil, control.Fail(409, "insufficient_points")
	}
	gift := control.ID()
	if _, e = t.Exec(ctx, "INSERT INTO av_live_gifts(id,scope,broadcast_id,sender,receiver,amount,day,created) VALUES(?,?,?,?,?,?,?,?)", gift, p.Scope, r.ID, p.ID, b.Host, amount, d, s.now().UnixMilli()); e != nil {
		return nil, e
	}
	if b.PK != "" {
		if _, e = t.Exec(ctx, "UPDATE av_live_pk SET left_score=left_score+CASE WHEN left_id=? THEN ? ELSE 0 END,right_score=right_score+CASE WHEN right_id=? THEN ? ELSE 0 END WHERE scope=? AND id=? AND state='active'", r.ID, amount, r.ID, amount, p.Scope, b.PK); e != nil {
			return nil, e
		}
	}
	var balance int64
	if e = t.Row(ctx, "SELECT balance FROM av_live_wallets WHERE scope=? AND subject_id=? AND day=?", p.Scope, p.ID, d).Scan(&balance); e != nil {
		return nil, e
	}
	out := map[string]any{"id": gift, "amount": amount, "sender": p.ID, "receiver": b.Host, "balance": balance}
	if e = s.emit(ctx, t, p, r, "live.gift", out); e != nil {
		return nil, e
	}
	return out, nil
}
func (s *Service) wallet(ctx context.Context, p control.Principal) (any, error) {
	if !p.IsUser() || !strings.HasPrefix(p.Scope, "org:") {
		return nil, control.Fail(403, "login_required")
	}
	d := day(s.now())
	balance := int64(100)
	e := s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT balance FROM av_live_wallets WHERE scope=? AND subject_id=? AND day=?"), p.Scope, p.ID, d).Scan(&balance)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	return map[string]any{"day": d, "balance": balance}, nil
}
func (s *Service) board(ctx context.Context, p control.Principal) (any, error) {
	if !p.IsUser() || !strings.HasPrefix(p.Scope, "org:") {
		return nil, control.Fail(403, "login_required")
	}
	rows, e := s.P.Store.DB.QueryContext(ctx, s.P.Store.Q("SELECT receiver,SUM(amount) FROM av_live_gifts WHERE scope=? AND day=? GROUP BY receiver ORDER BY SUM(amount) DESC,receiver LIMIT 100"), p.Scope, day(s.now()))
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
		out = append(out, map[string]any{"subject_id": who, "points": points})
	}
	return map[string]any{"items": out, "day": day(s.now())}, rows.Err()
}
func (s *Service) liveSnapshot(ctx context.Context, p control.Principal, id string) (any, error) {
	r, e := s.resource(ctx, p, id, "broadcast")
	if e != nil {
		return nil, e
	}
	v, e := s.participant(ctx, p, id)
	if e != nil || v.State == "removed" {
		return nil, control.Fail(403, "not_a_participant")
	}
	var room, host, pk string
	if e = s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT room_id,host,pk_id FROM av_broadcasts WHERE scope=? AND id=?"), p.Scope, id).Scan(&room, &host, &pk); e != nil {
		return nil, e
	}
	rows, e := s.P.Store.DB.QueryContext(ctx, s.P.Store.Q("SELECT subject_id,stage,muted FROM av_live_members WHERE scope=? AND resource_id=? AND banned=0 AND stage<>'' ORDER BY subject_id"), p.Scope, id)
	if e != nil {
		return nil, e
	}
	members := []map[string]any{}
	for rows.Next() {
		var who, stage string
		var muted int
		if e = rows.Scan(&who, &stage, &muted); e != nil {
			rows.Close()
			return nil, e
		}
		if stage == "pending" && p.ID != host && who != p.ID {
			continue
		}
		members = append(members, map[string]any{"subject_id": who, "stage": stage, "muted": muted != 0})
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	out := map[string]any{"broadcast": r, "room_id": room, "host": host, "stage_members": members, "pk_id": pk}
	stageResource := id
	if pk != "" {
		var left, state string
		if e = s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT left_id,state FROM av_live_pk WHERE scope=? AND id=?"), p.Scope, pk).Scan(&left, &state); e != nil {
			return nil, e
		}
		if state == "active" {
			stageResource = left
		}
	}
	out["stage_resource_id"] = stageResource
	identities, e := s.identities(ctx, p.Scope, stageResource, "stage")
	if e != nil {
		return nil, e
	}
	out["host_identity"] = identities[host]
	for _, member := range members {
		member["identity"] = identities[member["subject_id"].(string)]
	}
	audience, online, known, e := s.liveAudience(ctx, p.Scope, id, stageResource, identities)
	if e != nil {
		return nil, e
	}
	out["members"], out["online"], out["online_known"] = audience, online, known
	board, e := s.broadcastBoard(ctx, p.Scope, id)
	if e != nil {
		return nil, e
	}
	out["leaderboard"] = board
	if pk != "" {
		var state, winner, left, right string
		var ls, rs, expires int64
		if e = s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT state,winner,left_id,right_id,left_score,right_score,expires FROM av_live_pk WHERE scope=? AND id=?"), p.Scope, pk).Scan(&state, &winner, &left, &right, &ls, &rs, &expires); e != nil {
			return nil, e
		}
		out["pk"] = map[string]any{"id": pk, "state": state, "winner": winner, "left_id": left, "right_id": right, "left_score": ls, "right_score": rs, "expires": expires}
	}
	return out, nil
}
