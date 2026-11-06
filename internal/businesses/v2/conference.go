package v2

import (
	"av-platform/business/internal/platform/control"
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"strings"
	"time"

	"platform-foundation/server/persistence"
)

type meeting struct {
	Number         string `json:"number"`
	Host           string `json:"host"`
	Starts         int64  `json:"starts_at"`
	Duration       int    `json:"duration_minutes"`
	Salt, Password string
	Waiting        int    `json:"waiting_room"`
	Locked         int    `json:"locked"`
	Sharing        string `json:"sharing_member"`
	Missing        int64  `json:"host_missing"`
}

const meetingCols = "number,host,starts,duration,password_salt,password_hash,waiting,locked,sharing,host_missing"

func scanMeeting(row interface{ Scan(...any) error }) (meeting, error) {
	var m meeting
	e := row.Scan(&m.Number, &m.Host, &m.Starts, &m.Duration, &m.Salt, &m.Password, &m.Waiting, &m.Locked, &m.Sharing, &m.Missing)
	return m, e
}
func (s *Service) meeting(ctx context.Context, p control.Principal, method string, path []string, c command, key string) (any, error) {
	if len(path) == 1 {
		if path[0] == "join" && method == "POST" {
			return s.joinMeeting(ctx, p, c)
		}
		if path[0] == "meetings" {
			if method == "POST" {
				return s.createMeeting(ctx, p, c, key)
			}
			if method == "GET" {
				return s.meetingList(ctx, p)
			}
		}
	}
	if len(path) < 2 || path[0] != "meetings" {
		return nil, control.Fail(404, "not_found")
	}
	id := path[1]
	if len(path) == 2 && method == "GET" {
		return s.meetingSnapshot(ctx, p, id)
	}
	if len(path) != 3 || method != "POST" {
		return nil, control.Fail(404, "not_found")
	}
	action := path[2]
	if action == "presence" || action == "heartbeat" {
		return s.heartbeat(ctx, p, id)
	}
	return s.P.Mutate(ctx, p, "meeting:"+id+":"+action, key, c, func(t *persistence.Tx) (any, error) {
		r, e := s.inResource(ctx, t, p, id, "meeting")
		if e != nil {
			return nil, e
		}
		m, e := scanMeeting(t.Row(ctx, "SELECT "+meetingCols+" FROM av_meetings WHERE scope=? AND id=?", p.Scope, id))
		if e != nil {
			return nil, e
		}
		member, e := participantTx(ctx, t, id, p.ID)
		if e != nil {
			return nil, control.Fail(403, "not_a_participant")
		}
		if isTerminal(r.State) {
			if action == "end" || action == "cancel" {
				return map[string]any{"id": id, "state": r.State}, nil
			}
			return nil, control.Fail(409, "meeting_ended")
		}
		if member.State == "removed" || member.State == "rejected" || member.State == "expired" || member.State == "left" {
			return nil, control.Fail(403, "participant_inactive")
		}
		host := p.ID == m.Host
		target := c.Target
		if target == "" {
			target = p.ID
		}
		switch action {
		case "start":
			if !host {
				return nil, control.Fail(403, "host_required")
			}
			if r.State != "scheduled" {
				return nil, control.Fail(409, "not_scheduled")
			}
			r.State = "active"
			m.Starts = s.now().UnixMilli()
			if _, e = t.Exec(ctx, "UPDATE av_meetings SET starts=? WHERE scope=? AND id=?", m.Starts, p.Scope, id); e != nil {
				return nil, e
			}
		case "edit":
			if !host && r.Owner != p.ID {
				return nil, control.Fail(403, "host_required")
			}
			if r.State != "scheduled" {
				return nil, control.Fail(409, "not_scheduled")
			}
			if c.Title != "" {
				if !validTitle(c.Title) {
					return nil, control.Fail(400, "invalid_title")
				}
				r.Title = c.Title
			}
			if c.Starts != "" {
				v, e := time.Parse(time.RFC3339, c.Starts)
				if e != nil || !v.After(s.now()) {
					return nil, control.Fail(400, "invalid_start_time")
				}
				m.Starts = v.UnixMilli()
			}
			if c.Duration != 0 {
				if c.Duration < 1 || c.Duration > 1440 {
					return nil, control.Fail(400, "invalid_duration")
				}
				m.Duration = c.Duration
			}
			if c.Waiting != nil {
				m.Waiting = boolInt(*c.Waiting)
			}
			if c.Password != "" {
				if len(c.Password) > 128 {
					return nil, control.Fail(400, "invalid_password")
				}
				m.Salt = control.ID()
				m.Password = control.Hash(m.Salt + ":" + c.Password)
			}
			if _, e = t.Exec(ctx, "UPDATE av_meetings SET starts=?,duration=?,waiting=?,password_salt=?,password_hash=? WHERE scope=? AND id=?", m.Starts, m.Duration, m.Waiting, m.Salt, m.Password, p.Scope, id); e != nil {
				return nil, e
			}
		case "connect", "credentials":
			if member.State != "accepted" && member.State != "active" && member.State != "disconnected" {
				return nil, control.Fail(403, "waiting_for_admission")
			}
			if member.State == "disconnected" && member.Updated+120000 <= s.now().UnixMilli() {
				return nil, control.Fail(409, "reconnect_expired")
			}
			if r.State == "scheduled" && s.now().UnixMilli() < m.Starts {
				return nil, control.Fail(409, "meeting_not_started")
			}
			if _, e = t.Exec(ctx, "UPDATE av_participants SET state='active',updated=? WHERE scope=? AND resource_id=? AND subject_id=?", s.now().UnixMilli(), p.Scope, id, p.ID); e != nil {
				return nil, e
			}
			return s.meetingGrant(ctx, t, r, p.ID, m)
		case "end", "cancel":
			if !host && r.Owner != p.ID {
				return nil, control.Fail(403, "host_required")
			}
			return s.finish(ctx, t, p, r, map[bool]string{true: "cancelled", false: "ended"}[action == "cancel"])
		case "admit", "reject":
			if !host {
				return nil, control.Fail(403, "host_required")
			}
			v, e := participantTx(ctx, t, id, target)
			if e != nil || v.State != "waiting" {
				return nil, control.Fail(409, "not_waiting")
			}
			state := "accepted"
			if action == "reject" {
				state = "rejected"
			}
			if e = putParticipant(ctx, t, id, target, v.Role, state, s.now().UnixMilli()); e != nil {
				return nil, e
			}
		case "lock":
			if !host {
				return nil, control.Fail(403, "host_required")
			}
			m.Locked = boolInt(c.Enabled)
		case "transfer":
			if !host {
				return nil, control.Fail(403, "host_required")
			}
			v, e := participantTx(ctx, t, id, target)
			if e != nil || v.State != "active" && v.State != "accepted" {
				return nil, control.Fail(409, "target_unavailable")
			}
			if e = putParticipant(ctx, t, id, m.Host, "member", member.State, s.now().UnixMilli()); e != nil {
				return nil, e
			}
			if e = putParticipant(ctx, t, id, target, "host", v.State, s.now().UnixMilli()); e != nil {
				return nil, e
			}
			if e = s.P.Revoke(ctx, t, id, m.Host); e != nil {
				return nil, e
			}
			if e = s.P.Revoke(ctx, t, id, target); e != nil {
				return nil, e
			}
			m.Host = target
			m.Missing = 0
		case "kick":
			if !host || target == m.Host {
				return nil, control.Fail(403, "host_required")
			}
			v, e := participantTx(ctx, t, id, target)
			if e != nil {
				return nil, control.Fail(404, "participant_not_found")
			}
			if e = putParticipant(ctx, t, id, target, v.Role, "removed", s.now().UnixMilli()); e != nil {
				return nil, e
			}
			if e = s.P.Revoke(ctx, t, id, target); e != nil {
				return nil, e
			}
			if m.Sharing == target {
				m.Sharing = ""
			}
		case "mute":
			if !host && target != p.ID {
				return nil, control.Fail(403, "host_required")
			}
			if !host && !c.Enabled {
				return nil, control.Fail(403, "host_unmute_required")
			}
			v, e := participantTx(ctx, t, id, target)
			if e != nil {
				return nil, control.Fail(404, "participant_not_found")
			}
			if _, e = t.Exec(ctx, "UPDATE av_meeting_members SET muted=?,unmute_request=0 WHERE scope=? AND resource_id=? AND subject_id=?", boolInt(c.Enabled), p.Scope, id, target); e != nil {
				return nil, e
			}
			if e = putParticipant(ctx, t, id, target, v.Role, v.State, s.now().UnixMilli()); e != nil {
				return nil, e
			}
			if e = s.P.Revoke(ctx, t, id, target); e != nil {
				return nil, e
			}
		case "request-unmute":
			if member.State != "active" && member.State != "accepted" {
				return nil, control.Fail(403, "participant_inactive")
			}
			if target != p.ID && !host {
				return nil, control.Fail(403, "host_required")
			}
			if _, e = t.Exec(ctx, "UPDATE av_meeting_members SET unmute_request=? WHERE scope=? AND resource_id=? AND subject_id=?", s.now().UnixMilli(), p.Scope, id, target); e != nil {
				return nil, e
			}
		case "mute-all":
			if !host {
				return nil, control.Fail(403, "host_required")
			}
			rows, e := t.SQL.QueryContext(ctx, t.Store.Q("SELECT subject_id FROM av_participants WHERE scope=? AND resource_id=? AND subject_id<>? AND state IN ('active','accepted')"), p.Scope, id, p.ID)
			if e != nil {
				return nil, e
			}
			var subjects []string
			for rows.Next() {
				var who string
				if e = rows.Scan(&who); e != nil {
					rows.Close()
					return nil, e
				}
				subjects = append(subjects, who)
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return nil, e
			}
			for _, who := range subjects {
				if _, e = t.Exec(ctx, "UPDATE av_meeting_members SET muted=1 WHERE scope=? AND resource_id=? AND subject_id=?", p.Scope, id, who); e != nil {
					return nil, e
				}
				if _, e = t.Exec(ctx, "UPDATE av_participants SET policy_version=policy_version+1 WHERE scope=? AND resource_id=? AND subject_id=?", p.Scope, id, who); e != nil {
					return nil, e
				}
				if e = s.P.Revoke(ctx, t, id, who); e != nil {
					return nil, e
				}
			}
		case "share-start", "share-stop":
			if member.State != "active" && member.State != "accepted" {
				return nil, control.Fail(403, "participant_inactive")
			}
			if action == "share-start" {
				target = p.ID
				if m.Sharing != "" && m.Sharing != p.ID {
					return nil, control.Fail(409, "sharing_busy")
				}
				m.Sharing = p.ID
			} else {
				if m.Sharing != "" && m.Sharing != p.ID && !host {
					return nil, control.Fail(403, "sharing_owner_required")
				}
				target = m.Sharing
				m.Sharing = ""
			}
			if target != "" {
				v, e := participantTx(ctx, t, id, target)
				if e != nil {
					return nil, e
				}
				if e = putParticipant(ctx, t, id, target, v.Role, v.State, s.now().UnixMilli()); e != nil {
					return nil, e
				}
				if e = s.P.Revoke(ctx, t, id, target); e != nil {
					return nil, e
				}
			}
		case "leave":
			if e = putParticipant(ctx, t, id, p.ID, member.Role, "left", s.now().UnixMilli()); e != nil {
				return nil, e
			}
			if e = s.P.Revoke(ctx, t, id, p.ID); e != nil {
				return nil, e
			}
			if m.Sharing == p.ID {
				m.Sharing = ""
			}
			if host {
				var next string
				e = t.Row(ctx, "SELECT subject_id FROM av_participants WHERE scope=? AND resource_id=? AND state IN ('active','accepted') AND subject_id<>? ORDER BY updated,subject_id LIMIT 1", p.Scope, id, p.ID).Scan(&next)
				if errors.Is(e, sql.ErrNoRows) {
					return s.finish(ctx, t, p, r, "ended")
				}
				if e != nil {
					return nil, e
				}
				m.Host = next
				if _, e = t.Exec(ctx, "UPDATE av_participants SET role='host',policy_version=policy_version+1 WHERE scope=? AND resource_id=? AND subject_id=?", p.Scope, id, next); e != nil {
					return nil, e
				}
				if e = s.P.Revoke(ctx, t, id, next); e != nil {
					return nil, e
				}
			}
		default:
			return nil, control.Fail(404, "not_found")
		}
		if _, e = t.Exec(ctx, "UPDATE av_meetings SET host=?,locked=?,sharing=?,host_missing=? WHERE scope=? AND id=?", m.Host, m.Locked, m.Sharing, m.Missing, p.Scope, id); e != nil {
			return nil, e
		}
		r.Updated = s.now().UnixMilli()
		if e = s.P.UpdateResource(ctx, t, r); e != nil {
			return nil, e
		}
		out := map[string]any{"id": id, "state": r.State, "version": r.Version + 1, "action": action, "target": target}
		if e = s.emit(ctx, t, p, r, "meeting."+action, out); e != nil {
			return nil, e
		}
		return out, nil
	})
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func (s *Service) createMeeting(ctx context.Context, p control.Principal, c command, key string) (any, error) {
	if e := permission(p, "av.conference.create"); e != nil {
		return nil, e
	}
	if !validTitle(c.Title) || len(c.Password) > 128 {
		return nil, control.Fail(400, "invalid_meeting")
	}
	if c.Duration == 0 {
		c.Duration = 60
	}
	if c.Duration < 1 || c.Duration > 1440 {
		return nil, control.Fail(400, "invalid_duration")
	}
	now := s.now()
	starts := now
	if c.Scheduled {
		var e error
		starts, e = time.Parse(time.RFC3339, c.Starts)
		if e != nil || !starts.After(now) {
			return nil, control.Fail(400, "invalid_start_time")
		}
	}
	return s.P.Mutate(ctx, p, "meeting:create", key, c, func(t *persistence.Tx) (any, error) {
		n, e := number(9)
		if e != nil {
			return nil, e
		}
		state := "active"
		if c.Scheduled {
			state = "scheduled"
		}
		r := control.Resource{ID: control.ID(), Scope: p.Scope, Kind: "meeting", Owner: p.ID, Title: strings.TrimSpace(c.Title), State: state, Version: 1, Created: now.UnixMilli(), Updated: now.UnixMilli()}
		if e = s.P.InsertResource(ctx, t, r); e != nil {
			return nil, e
		}
		salt, password := control.ID(), ""
		if c.Password != "" {
			password = control.Hash(salt + ":" + c.Password)
		}
		waiting := 0
		if c.Waiting != nil {
			waiting = boolInt(*c.Waiting)
		}
		if _, e = t.Exec(ctx, "INSERT INTO av_meetings(scope,id,number,host,starts,duration,password_salt,password_hash,waiting) VALUES(?,?,?,?,?,?,?,?,?)", p.Scope, r.ID, n, p.ID, starts.UnixMilli(), c.Duration, salt, password, waiting); e != nil {
			return nil, e
		}
		if e = putParticipant(ctx, t, r.ID, p.ID, "host", "active", now.UnixMilli()); e != nil {
			return nil, e
		}
		if _, e = t.Exec(ctx, "INSERT INTO av_meeting_members(scope,resource_id,subject_id,name) VALUES(?,?,?,?)", p.Scope, r.ID, p.ID, c.Name); e != nil {
			return nil, e
		}
		if e = s.emit(ctx, t, p, r, "meeting.created", map[string]string{"number": n}); e != nil {
			return nil, e
		}
		return map[string]any{"id": r.ID, "number": n, "state": state, "version": 1, "starts_at": starts.UTC().Format(time.RFC3339)}, nil
	})
}

func (s *Service) joinMeeting(ctx context.Context, p control.Principal, c command) (any, error) {
	lookup := c.Number
	if lookup == "" {
		lookup = c.ResourceID
	}
	var scope, id string
	e := s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT scope,id FROM av_meetings WHERE number=? OR id=?"), lookup, lookup).Scan(&scope, &id)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, control.Fail(404, "meeting_not_found")
	}
	if e != nil {
		return nil, e
	}
	if len(c.Name) > 120 || len(c.Password) > 128 {
		return nil, control.Fail(400, "invalid_join")
	}
	var out any
	e = s.P.Store.Within(ctx, scope, func(t *persistence.Tx) error {
		r, e := s.P.ResourceTx(ctx, t, id)
		if e != nil {
			return e
		}
		m, e := scanMeeting(t.Row(ctx, "SELECT "+meetingCols+" FROM av_meetings WHERE scope=? AND id=?", scope, id))
		if e != nil {
			return e
		}
		if isTerminal(r.State) {
			return control.Fail(409, "meeting_ended")
		}
		if m.Password != "" && subtle.ConstantTimeCompare([]byte(m.Password), []byte(control.Hash(m.Salt+":"+c.Password))) != 1 {
			return control.Fail(403, "invalid_meeting_password")
		}
		if r.State == "scheduled" && m.Starts > s.now().UnixMilli() {
			return control.Fail(409, "meeting_not_started")
		}
		var token string
		actor := p
		if p.Scope != scope || p.ID == "" || p.ResourceID != "" && p.ResourceID != id {
			token, actor, e = s.P.IssueGuest(ctx, t, scope, id, "guest")
			if e != nil {
				return e
			}
		}
		member, e := participantTx(ctx, t, id, actor.ID)
		existing := e == nil
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if existing && (member.State == "removed" || member.State == "rejected") {
			return control.Fail(403, "admission_denied")
		}
		if existing && member.State == "disconnected" && member.Updated+120000 <= s.now().UnixMilli() {
			return control.Fail(409, "reconnect_expired")
		}
		if m.Locked != 0 && (!existing || member.State == "left" || member.State == "expired") {
			return control.Fail(403, "meeting_locked")
		}
		state := "accepted"
		role := "member"
		if !actor.IsUser() || m.Waiting != 0 {
			state = "waiting"
		}
		if existing && member.State != "left" && member.State != "expired" {
			state, role = member.State, member.Role
		} else {
			var count int
			if e = t.Row(ctx, "SELECT COUNT(*) FROM av_participants WHERE scope=? AND resource_id=? AND state IN ('active','accepted','waiting','disconnected')", scope, id).Scan(&count); e != nil {
				return e
			}
			if count >= 25 {
				return control.Fail(409, "meeting_full")
			}
			if e = putParticipant(ctx, t, id, actor.ID, role, state, s.now().UnixMilli()); e != nil {
				return e
			}
			if _, e = t.Exec(ctx, "INSERT INTO av_meeting_members(scope,resource_id,subject_id,name) VALUES(?,?,?,?) ON CONFLICT(scope,resource_id,subject_id) DO UPDATE SET name=excluded.name", scope, id, actor.ID, c.Name); e != nil {
				return e
			}
		}
		result := map[string]any{"id": id, "resource_id": id, "subject_id": actor.ID, "state": state, "role": role}
		if token != "" {
			result["credential"] = token
		}
		if state == "active" || state == "accepted" {
			g, e := s.meetingGrant(ctx, t, r, actor.ID, m)
			if e != nil {
				return e
			}
			result["grant_id"] = g.(map[string]any)["grant_id"]
		}
		if e = s.emit(ctx, t, actor, r, "meeting.joined", map[string]string{"subject_id": actor.ID, "state": state}); e != nil {
			return e
		}
		out = result
		return nil
	})
	return out, e
}
func (s *Service) meetingGrant(ctx context.Context, t *persistence.Tx, r control.Resource, subject string, m meeting) (any, error) {
	v, e := participantTx(ctx, t, r.ID, subject)
	if e != nil {
		return nil, e
	}
	var muted int
	if e = t.Row(ctx, "SELECT muted FROM av_meeting_members WHERE scope=? AND resource_id=? AND subject_id=?", r.Scope, r.ID, subject).Scan(&muted); e != nil {
		return nil, e
	}
	sources := []string{"camera"}
	if muted == 0 {
		sources = append(sources, "microphone")
	}
	if m.Sharing == subject {
		sources = append(sources, "screen_share", "screen_share_audio")
	}
	g, e := s.P.Grant(ctx, t, r, subject, v.Role, "conference", control.Capabilities{Publish: true, Subscribe: true, Data: true, Sources: sources})
	if e != nil {
		return nil, e
	}
	return map[string]any{"grant_id": g.ID, "participant_identity": g.Identity, "policy_version": g.PolicyVersion}, nil
}
func (s *Service) meetingSnapshot(ctx context.Context, p control.Principal, id string) (any, error) {
	r, e := s.resource(ctx, p, id, "meeting")
	if e != nil {
		return nil, e
	}
	v, e := s.participant(ctx, p, id)
	if e != nil || v.State == "removed" || v.State == "rejected" {
		return nil, control.Fail(403, "not_a_participant")
	}
	m, e := scanMeeting(s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT "+meetingCols+" FROM av_meetings WHERE scope=? AND id=?"), p.Scope, id))
	if e != nil {
		return nil, e
	}
	rows, e := s.P.Store.DB.QueryContext(ctx, s.P.Store.Q("SELECT p.subject_id,p.role,p.state,p.policy_version,m.name,m.muted,m.unmute_request FROM av_participants p JOIN av_meeting_members m ON p.scope=m.scope AND p.resource_id=m.resource_id AND p.subject_id=m.subject_id WHERE p.scope=? AND p.resource_id=? ORDER BY p.updated,p.subject_id"), p.Scope, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	members := []map[string]any{}
	for rows.Next() {
		var subject, role, state, name string
		var version, request int64
		var muted int
		if e = rows.Scan(&subject, &role, &state, &version, &name, &muted, &request); e != nil {
			return nil, e
		}
		if state == "waiting" && p.ID != m.Host && subject != p.ID {
			continue
		}
		members = append(members, map[string]any{"subject_id": subject, "role": role, "state": state, "policy_version": version, "name": name, "muted": muted != 0, "unmute_request": request})
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	rows.Close()
	identities, e := s.identities(ctx, p.Scope, id, "conference")
	if e != nil {
		return nil, e
	}
	for _, member := range members {
		member["identity"] = identities[member["subject_id"].(string)]
	}
	return map[string]any{"meeting": r, "number": m.Number, "host": m.Host, "starts_at": time.UnixMilli(m.Starts).UTC().Format(time.RFC3339), "waiting_room": m.Waiting != 0, "locked": m.Locked != 0, "sharing_member": m.Sharing, "members": members}, nil
}
