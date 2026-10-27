// Package livestudio owns broadcasts and all authoritative interaction state.
package livestudio

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"av-platform/business/internal/platform/identity"
	"av-platform/business/migrations"
)

type Media interface {
	ConnectStudio(context.Context, string, string, string, bool, bool) (map[string]any, error)
	Published(context.Context, string, string) (bool, error)
	Remove(context.Context, string, string) error
	Delete(context.Context, string) error
}
type Service struct {
	DB    *sql.DB
	Media Media
	Now   func() time.Time
	mu    sync.Mutex
}
type Error struct {
	Code   string
	Status int
}

func (e *Error) Error() string           { return e.Code }
func fail(code string, status int) error { return &Error{code, status} }

type Broadcast struct {
	ID        string `json:"id"`
	Room      string `json:"room"`
	Owner     string `json:"owner"`
	State     string `json:"state"`
	MediaRoom string `json:"-"`
	StageRoom string `json:"-"`
	Created   int64  `json:"created"`
	Heartbeat int64  `json:"heartbeat"`
	Ended     int64  `json:"ended"`
}
type Room struct {
	ID        string `json:"id"`
	Owner     string `json:"owner"`
	Title     string `json:"title"`
	Broadcast string `json:"broadcast"`
	State     string `json:"state"`
}
type PK struct {
	ID       string `json:"id"`
	A        string `json:"a"`
	B        string `json:"b"`
	State    string `json:"state"`
	Stage    string `json:"-"`
	Created  int64  `json:"created"`
	Started  int64  `json:"started"`
	Deadline int64  `json:"deadline"`
	ScoreA   int    `json:"score_a"`
	ScoreB   int    `json:"score_b"`
	Winner   string `json:"winner"`
}
type Event struct {
	Seq     int64           `json:"seq"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
	Created int64           `json:"created"`
}
type Command struct {
	Title     string `json:"title"`
	Text      string `json:"text"`
	Target    string `json:"target"`
	RequestID string `json:"request_id"`
	Points    int    `json:"points"`
	Enabled   bool   `json:"enabled"`
	Connected bool   `json:"connected"`
	Stage     bool   `json:"stage"`
}
type dbtx struct {
	*sql.Tx
	err error
	now int64
}

func (t *dbtx) exec(q string, args ...any) {
	if t.err == nil {
		_, t.err = t.Exec(q, args...)
	}
}
func (t *dbtx) event(b, kind string, p any) {
	v, e := json.Marshal(p)
	if e != nil {
		t.err = e
		return
	}
	t.exec("INSERT INTO live_events(broadcast,kind,payload,created) VALUES(?,?,?,?)", b, kind, string(v), t.now)
}
func broadcast(t *dbtx, id string) (Broadcast, error) {
	var b Broadcast
	e := t.QueryRow("SELECT id,room,owner,state,media_room,stage_room,created,heartbeat,ended FROM live_broadcasts WHERE id=?", id).Scan(&b.ID, &b.Room, &b.Owner, &b.State, &b.MediaRoom, &b.StageRoom, &b.Created, &b.Heartbeat, &b.Ended)
	if e == sql.ErrNoRows {
		return b, fail("broadcast_not_found", 404)
	}
	return b, e
}
func getPK(t *dbtx, id string) (PK, error) {
	var p PK
	e := t.QueryRow("SELECT id,a,b,state,stage_room,created,started,deadline,score_a,score_b,winner FROM live_pks WHERE id=?", id).Scan(&p.ID, &p.A, &p.B, &p.State, &p.Stage, &p.Created, &p.Started, &p.Deadline, &p.ScoreA, &p.ScoreB, &p.Winner)
	if e == sql.ErrNoRows {
		return p, fail("pk_not_found", 404)
	}
	return p, e
}
func activePK(t *dbtx, b string) (PK, bool) {
	var id string
	e := t.QueryRow("SELECT id FROM live_pks WHERE (a=? OR b=?) AND state IN ('invited','active')", b, b).Scan(&id)
	if e == sql.ErrNoRows {
		return PK{}, false
	}
	if e != nil {
		t.err = e
		return PK{}, false
	}
	p, e := getPK(t, id)
	if e != nil {
		t.err = e
	}
	return p, e == nil
}
func New(db *sql.DB, media Media) (*Service, error) {
	if _, e := db.Exec(migrations.LiveStudio); e != nil {
		return nil, e
	}
	s := &Service{DB: db, Media: media, Now: time.Now}
	// A restart never resumes a contest using unobserved media or stale authority.
	_, e := s.transact(context.Background(), func(t *dbtx) (any, error) {
		rows, e := t.Query("SELECT id FROM live_pks WHERE state IN ('invited','active')")
		if e != nil {
			return nil, e
		}
		var ids []string
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				return nil, e
			}
			ids = append(ids, id)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
		for _, id := range ids {
			p, e := getPK(t, id)
			if e != nil {
				return nil, e
			}
			s.finishPK(t, p, "cancelled", "")
		}
		t.exec("UPDATE live_grants SET revoked=1")
		t.exec("UPDATE live_broadcasts SET state='reconnecting',heartbeat=? WHERE state!='ended'", t.now-61)
		return nil, nil
	})
	return s, e
}
func (s *Service) transact(ctx context.Context, f func(*dbtx) (any, error)) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Run effects after Commit/Rollback closes the transaction.
	defer s.drainMedia(ctx)
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	t := &dbtx{Tx: tx, now: s.Now().Unix()}
	v, e := f(t)
	if e != nil {
		return nil, e
	}
	if t.err != nil {
		return nil, t.err
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return v, nil
}
func userIdentity(u identity.User, anonymous string) string {
	if u.ID != "" {
		return "u-" + u.ID
	}
	return "v-" + anonymous
}
func ensurePresent(t *dbtx, b Broadcast, id string, u identity.User) error {
	var banned int
	e := t.QueryRow("SELECT banned FROM live_presence WHERE broadcast=? AND identity=?", b.ID, id).Scan(&banned)
	if e != nil && e != sql.ErrNoRows {
		return e
	}
	if banned != 0 {
		return fail("banned", 403)
	}
	name := u.Name
	if name == "" {
		name = "访客"
	}
	t.exec("INSERT INTO live_presence(broadcast,identity,user_id,name,seen) VALUES(?,?,?,?,?) ON CONFLICT(broadcast,identity) DO UPDATE SET seen=excluded.seen", b.ID, id, u.ID, name, t.now)
	return t.err
}
func (s *Service) Rooms(ctx context.Context) ([]Room, error) {
	rows, e := s.DB.QueryContext(ctx, `SELECT r.id,r.owner,r.title,COALESCE(b.id,''),COALESCE(b.state,'offline') FROM live_rooms r LEFT JOIN live_broadcasts b ON b.room=r.id AND b.state!='ended' ORDER BY r.created DESC LIMIT 200`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Room{}
	for rows.Next() {
		var r Room
		if e = rows.Scan(&r.ID, &r.Owner, &r.Title, &r.Broadcast, &r.State); e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Service) Start(ctx context.Context, u identity.User, title string) (any, error) {
	if u.ID == "" {
		return nil, identity.ErrUnauthorized
	}
	title = strings.TrimSpace(title)
	if title == "" || utf8.RuneCountInString(title) > 80 {
		return nil, fail("invalid_title", 400)
	}
	return s.transact(ctx, func(t *dbtx) (any, error) {
		var existing string
		e := t.QueryRow("SELECT id FROM live_broadcasts WHERE owner=? AND state!='ended'", u.ID).Scan(&existing)
		if e == nil {
			return broadcast(t, existing)
		}
		if e != sql.ErrNoRows {
			return nil, e
		}
		var count int
		if e = t.QueryRow("SELECT count(*) FROM live_broadcasts WHERE state!='ended'").Scan(&count); e != nil {
			return nil, e
		}
		if count >= 5 {
			return nil, fail("broadcast_capacity", 409)
		}
		room := identity.ID()
		t.exec("INSERT INTO live_rooms(id,owner,title,created) VALUES(?,?,?,?) ON CONFLICT(owner) DO UPDATE SET title=excluded.title", room, u.ID, title, t.now)
		if t.err != nil {
			return nil, t.err
		}
		if e = t.QueryRow("SELECT id FROM live_rooms WHERE owner=?", u.ID).Scan(&room); e != nil {
			return nil, e
		}
		id := identity.ID()
		t.exec("INSERT INTO live_broadcasts(id,room,owner,state,media_room,stage_room,created,heartbeat) VALUES(?,?,?,'starting',?,?,?,?)", id, room, u.ID, "studio-program-"+id, "studio-stage-"+id, t.now, t.now)
		t.event(id, "broadcast", map[string]any{"state": "starting"})
		if t.err != nil {
			return nil, t.err
		}
		return broadcast(t, id)
	})
}
func (s *Service) Execute(ctx context.Context, u identity.User, anonymous, id, action string, c Command) (any, error) {
	return s.transact(ctx, func(t *dbtx) (any, error) {
		b, e := broadcast(t, id)
		if e != nil {
			return nil, e
		}
		who := userIdentity(u, anonymous)
		owner := u.ID != "" && u.ID == b.Owner
		if action == "snapshot" {
			return s.snapshot(t, b)
		}
		if b.State == "ended" {
			if action == "heartbeat" || action == "presence" {
				return s.snapshot(t, b)
			}
			if action == "stop" && owner {
				return b, nil
			}
			return nil, fail("broadcast_ended", 409)
		}
		if u.ID == "" && anonymous == "" {
			return nil, identity.ErrUnauthorized
		}
		if e = ensurePresent(t, b, who, u); e != nil {
			return nil, e
		}
		switch action {
		case "connect":
			room := b.MediaRoom
			publish := owner
			subscribe := !owner
			role := "viewer"
			if owner {
				role = "host"
			}
			if c.Stage {
				if !owner {
					var state string
					if e = t.QueryRow("SELECT state FROM live_guests WHERE broadcast=? AND user_id=?", id, u.ID).Scan(&state); e != nil || state != "approved" {
						return nil, fail("guest_not_approved", 403)
					}
				}
				room = b.StageRoom
				publish = true
				subscribe = true
				role = "guest"
				if owner {
					role = "stage_host"
				}
				if p, ok := activePK(t, id); ok && p.State == "active" {
					if !owner {
						return nil, fail("pk_active", 409)
					}
					room = p.Stage
					role = "pk_host"
				}
			}
			if !owner && !c.Stage {
				var n int
				if e = t.QueryRow("SELECT count(*) FROM live_presence WHERE broadcast=? AND banned=0 AND seen>? AND user_id!=?", id, t.now-30, b.Owner).Scan(&n); e != nil {
					return nil, e
				}
				if n > 20 {
					return nil, fail("viewer_capacity", 409)
				}
			}

			var pending int
			if e = t.QueryRow("SELECT count(*) FROM live_media_cleanup WHERE media_room=? AND (identity='' OR identity=?)", room, who).Scan(&pending); e != nil {
				return nil, e
			}
			if pending > 0 {
				return nil, fail("media_cleanup_pending", 503)
			}
			grant := identity.ID()
			out, e := s.Media.ConnectStudio(ctx, room, who, grant, publish, subscribe)
			if e != nil {
				return nil, fail("media_unavailable", 502)
			}
			t.exec("INSERT INTO live_grants(id,broadcast,identity,media_room,user_id,role,expires) VALUES(?,?,?,?,?,?,?)", grant, id, who, room, u.ID, role, t.now+60)
			out["role"] = role
			return out, nil
		case "heartbeat", "presence":
			t.exec("UPDATE live_grants SET expires=? WHERE broadcast=? AND identity=? AND revoked=0", t.now+60, id, who)
			if owner && action == "heartbeat" {
				published := false
				if c.Connected {
					published, e = s.Media.Published(ctx, b.MediaRoom, who)
					if e != nil {
						return nil, fail("media_unavailable", 502)
					}
				}
				state := "reconnecting"
				if published {
					state = "live"
					t.exec("UPDATE live_broadcasts SET heartbeat=?,state=? WHERE id=?", t.now, state, id)
				} else {
					t.exec("UPDATE live_broadcasts SET state=? WHERE id=?", state, id)
				}
				if state != b.State {
					t.event(id, "broadcast", map[string]any{"state": state})
				}
			}
			return s.snapshot(t, b)
		case "stop":
			if !owner && !u.Admin {
				return nil, fail("forbidden", 403)
			}
			s.end(t, b, ctx)
			return map[string]any{"state": "ended"}, nil
		case "chat":
			if u.ID == "" {
				return nil, identity.ErrUnauthorized
			}
			var muted int
			if e = t.QueryRow("SELECT muted FROM live_presence WHERE broadcast=? AND identity=?", id, who).Scan(&muted); e != nil {
				return nil, e
			}
			if muted != 0 {
				return nil, fail("muted", 403)
			}
			text := strings.TrimSpace(c.Text)
			if text == "" || utf8.RuneCountInString(text) > 300 {
				return nil, fail("invalid_message", 400)
			}
			t.event(id, "chat", map[string]any{"user_id": u.ID, "name": u.Name, "text": text})
			return map[string]bool{"ok": true}, nil
		case "mute", "kick":
			if !owner && !u.Admin {
				return nil, fail("forbidden", 403)
			}
			if c.Target == "u-"+b.Owner {
				return nil, fail("cannot_moderate_host", 400)
			}
			column := "muted"
			if action == "kick" {
				column = "banned"
				c.Enabled = true
			}
			t.exec("UPDATE live_presence SET "+column+"=? WHERE broadcast=? AND identity=?", c.Enabled, id, c.Target)
			if action == "kick" {
				t.exec("UPDATE live_grants SET revoked=1 WHERE broadcast=? AND identity=?", id, c.Target)
				t.exec("UPDATE live_guests SET state='ended' WHERE broadcast=? AND user_id=?", id, strings.TrimPrefix(c.Target, "u-"))
				t.cleanup(b.MediaRoom, c.Target)
				t.cleanup(b.StageRoom, c.Target)
			}
			t.event(id, action, map[string]any{"identity": c.Target, "enabled": c.Enabled})
			return map[string]bool{"ok": true}, nil
		case "guest/request":
			if u.ID == "" {
				return nil, identity.ErrUnauthorized
			}
			if owner {
				return nil, fail("host_cannot_apply", 400)
			}
			if _, ok := activePK(t, id); ok {
				return nil, fail("pk_active", 409)
			}
			t.exec("INSERT INTO live_guests(broadcast,user_id,state) VALUES(?,?,'pending') ON CONFLICT(broadcast,user_id) DO UPDATE SET state=CASE WHEN state='approved' THEN state ELSE 'pending' END", id, u.ID)
			t.event(id, "guest", map[string]any{"user_id": u.ID, "state": "pending"})
			return map[string]bool{"ok": true}, nil
		case "guest/approve", "guest/reject", "guest/end":
			if !owner && !(action == "guest/end" && c.Target == u.ID) {
				return nil, fail("forbidden", 403)
			}
			state := "ended"
			if action == "guest/reject" {
				state = "rejected"
			}
			if action == "guest/approve" {
				if _, ok := activePK(t, id); ok {
					return nil, fail("pk_active", 409)
				}
				var n int
				if e = t.QueryRow("SELECT count(*) FROM live_guests WHERE broadcast=? AND state='approved' AND user_id!=?", id, c.Target).Scan(&n); e != nil {
					return nil, e
				}
				if n >= 3 {
					return nil, fail("guest_capacity", 409)
				}
				state = "approved"
			}
			var previous string
			if e = t.QueryRow("SELECT state FROM live_guests WHERE broadcast=? AND user_id=?", id, c.Target).Scan(&previous); e != nil {
				return nil, fail("guest_not_found", 404)
			}
			t.exec("UPDATE live_guests SET state=? WHERE broadcast=? AND user_id=?", state, id, c.Target)
			if state != "approved" {
				t.exec("UPDATE live_grants SET revoked=1 WHERE broadcast=? AND user_id=? AND role='guest'", id, c.Target)
				t.cleanup(b.StageRoom, "u-"+c.Target)
			}
			t.event(id, "guest", map[string]any{"user_id": c.Target, "state": state})
			return map[string]bool{"ok": true}, nil
		case "gift":
			return s.gift(t, b, u, c)
		case "pk/invite":
			if !owner {
				return nil, fail("forbidden", 403)
			}
			other, e := broadcast(t, c.Target)
			if e != nil {
				return nil, e
			}
			if other.ID == id || b.State != "live" || other.State != "live" {
				return nil, fail("pk_requires_two_live_hosts", 409)
			}
			for _, bid := range []string{id, other.ID} {
				if _, ok := activePK(t, bid); ok {
					return nil, fail("pk_busy", 409)
				}
				var n int
				if e = t.QueryRow("SELECT count(*) FROM live_guests WHERE broadcast=? AND state='approved'", bid).Scan(&n); e != nil {
					return nil, e
				}
				if n > 0 {
					return nil, fail("end_guests_before_pk", 409)
				}
			}
			pid := identity.ID()
			t.exec("INSERT INTO live_pks(id,a,b,state,stage_room,created) VALUES(?,?,?,'invited',?,?)", pid, id, other.ID, "studio-pk-"+pid, t.now)
			if t.err != nil {
				return nil, t.err
			}
			p, e := getPK(t, pid)
			if e != nil {
				return nil, e
			}
			t.event(id, "pk", p)
			t.event(other.ID, "pk", p)
			return p, nil
		case "pk/accept", "pk/reject", "pk/leave":
			if !owner {
				return nil, fail("forbidden", 403)
			}
			p, ok := activePK(t, id)
			if !ok {
				return nil, fail("pk_not_active", 409)
			}
			if action == "pk/accept" {
				if p.B != id || p.State != "invited" {
					return nil, fail("only_invitee_can_accept", 403)
				}
				a, e := broadcast(t, p.A)
				if e != nil {
					return nil, e
				}
				if b.State != "live" || a.State != "live" {
					return nil, fail("pk_requires_two_live_hosts", 409)
				}
				t.exec("UPDATE live_pks SET state='active',started=?,deadline=? WHERE id=?", t.now, t.now+300, p.ID)
				p.State = "active"
				p.Started = t.now
				p.Deadline = t.now + 300
				t.event(p.A, "pk", p)
				t.event(p.B, "pk", p)
			} else if action == "pk/reject" {
				if p.B != id || p.State != "invited" {
					return nil, fail("invalid_pk_action", 409)
				}
				s.finishPK(t, p, "cancelled", "")
			} else {
				winner := p.A
				if id == p.A {
					winner = p.B
				}
				state := "finished"
				if p.State == "invited" {
					state = "cancelled"
					winner = ""
				}
				s.finishPK(t, p, state, winner)
			}
			if t.err != nil {
				return nil, t.err
			}
			return getPK(t, p.ID)
		}
		return nil, fail("not_found", 404)
	})
}
func (s *Service) gift(t *dbtx, b Broadcast, u identity.User, c Command) (any, error) {
	if u.ID == "" {
		return nil, identity.ErrUnauthorized
	}
	if u.ID == b.Owner {
		return nil, fail("cannot_gift_self", 400)
	}
	if c.RequestID == "" || len(c.RequestID) > 100 || (c.Points != 1 && c.Points != 10 && c.Points != 50) {
		return nil, fail("invalid_gift", 400)
	}
	var oldRoom string
	var oldPoints int
	e := t.QueryRow("SELECT broadcast,points FROM live_gifts WHERE sender=? AND request_id=?", u.ID, c.RequestID).Scan(&oldRoom, &oldPoints)
	if e == nil {
		if oldRoom != b.ID || oldPoints != c.Points {
			return nil, fail("idempotency_conflict", 409)
		}
		return map[string]bool{"ok": true, "duplicate": true}, nil
	}
	if e != sql.ErrNoRows {
		return nil, e
	}
	if b.State != "live" {
		return nil, fail("broadcast_not_live", 409)
	}
	day := time.Unix(t.now, 0).In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02")
	t.exec("INSERT OR IGNORE INTO live_wallets(user_id,day,remaining) VALUES(?,?,100)", u.ID, day)
	if t.err != nil {
		return nil, t.err
	}
	var remaining int
	if e = t.QueryRow("SELECT remaining FROM live_wallets WHERE user_id=? AND day=?", u.ID, day).Scan(&remaining); e != nil {
		return nil, e
	}
	if remaining < c.Points {
		return nil, fail("insufficient_points", 409)
	}
	t.exec("UPDATE live_wallets SET remaining=remaining-? WHERE user_id=? AND day=?", c.Points, u.ID, day)
	t.exec("INSERT INTO live_gifts(request_id,sender,broadcast,owner,points,day,created) VALUES(?,?,?,?,?,?,?)", c.RequestID, u.ID, b.ID, b.Owner, c.Points, day, t.now)
	if p, ok := activePK(t, b.ID); ok && p.State == "active" && t.now >= p.Started && t.now < p.Deadline {
		if p.A == b.ID {
			p.ScoreA += c.Points
		} else {
			p.ScoreB += c.Points
		}
		t.exec("UPDATE live_pks SET score_a=?,score_b=? WHERE id=?", p.ScoreA, p.ScoreB, p.ID)
		t.event(p.A, "pk", p)
		t.event(p.B, "pk", p)
	}
	t.event(b.ID, "gift", map[string]any{"name": u.Name, "user_id": u.ID, "points": c.Points})
	return map[string]any{"remaining": remaining - c.Points}, nil
}
func (s *Service) finishPK(t *dbtx, p PK, state, winner string) {
	t.exec("UPDATE live_pks SET state=?,winner=? WHERE id=? AND state IN ('invited','active')", state, winner, p.ID)
	t.exec("UPDATE live_grants SET revoked=1 WHERE media_room=?", p.Stage)
	t.cleanup(p.Stage, "")
	p.State = state
	p.Winner = winner
	t.event(p.A, "pk", p)
	t.event(p.B, "pk", p)
}
func (s *Service) end(t *dbtx, b Broadcast, ctx context.Context) {
	t.exec("UPDATE live_broadcasts SET state='ended',ended=? WHERE id=?", t.now, b.ID)
	t.exec("UPDATE live_grants SET revoked=1 WHERE broadcast=?", b.ID)
	t.exec("UPDATE live_guests SET state='ended' WHERE broadcast=?", b.ID)
	if p, ok := activePK(t, b.ID); ok {
		winner := p.A
		if winner == b.ID {
			winner = p.B
		}
		if p.State == "active" {
			s.finishPK(t, p, "finished", winner)
		} else {
			s.finishPK(t, p, "cancelled", "")
		}
		t.cleanup(p.Stage, "")
	}
	t.event(b.ID, "broadcast", map[string]any{"state": "ended"})
	t.cleanup(b.MediaRoom, "")
	t.cleanup(b.StageRoom, "")
}
func (s *Service) snapshot(t *dbtx, b Broadcast) (any, error) {
	fresh, e := broadcast(t, b.ID)
	if e != nil {
		return nil, e
	}
	b = fresh
	viewers := []map[string]any{}
	rows, e := t.Query("SELECT identity,user_id,name,muted FROM live_presence WHERE broadcast=? AND banned=0 AND seen>? ORDER BY name LIMIT 100", b.ID, t.now-30)
	if e != nil {
		return nil, e
	}
	for rows.Next() {
		var id, user, name string
		var muted bool
		if e = rows.Scan(&id, &user, &name, &muted); e != nil {
			rows.Close()
			return nil, e
		}
		viewers = append(viewers, map[string]any{"identity": id, "user_id": user, "name": name, "muted": muted})
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	guests := []map[string]any{}
	rows, e = t.Query("SELECT g.user_id,u.name,g.state FROM live_guests g JOIN identity_users u ON u.id=g.user_id WHERE broadcast=?", b.ID)
	if e != nil {
		return nil, e
	}
	for rows.Next() {
		var id, name, state string
		if e = rows.Scan(&id, &name, &state); e != nil {
			rows.Close()
			return nil, e
		}
		guests = append(guests, map[string]any{"user_id": id, "name": name, "state": state})
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	var seq int64
	if e = t.QueryRow("SELECT COALESCE(MAX(seq),0) FROM live_events WHERE broadcast=?", b.ID).Scan(&seq); e != nil {
		return nil, e
	}
	var pk any
	p, ok := activePK(t, b.ID)
	if ok {
		pk = p
	} else {
		var pid string
		if e = t.QueryRow("SELECT id FROM live_pks WHERE a=? OR b=? ORDER BY created DESC LIMIT 1", b.ID, b.ID).Scan(&pid); e == nil {
			p, e = getPK(t, pid)
			if e != nil {
				return nil, e
			}
			pk = p
		} else if e != sql.ErrNoRows {
			return nil, e
		}
	}
	board := []map[string]any{}
	rows, e = t.Query("SELECT u.name,SUM(g.points) FROM live_gifts g JOIN identity_users u ON u.id=g.sender WHERE broadcast=? GROUP BY sender ORDER BY SUM(g.points) DESC,u.name LIMIT 20", b.ID)
	if e != nil {
		return nil, e
	}
	for rows.Next() {
		var name string
		var points int
		if e = rows.Scan(&name, &points); e != nil {
			rows.Close()
			return nil, e
		}
		board = append(board, map[string]any{"name": name, "points": points})
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	return map[string]any{"broadcast": b, "viewers": viewers, "online": len(viewers), "guests": guests, "pk": pk, "leaderboard": board, "seq": seq}, nil
}
func (s *Service) Events(ctx context.Context, b string, after int64) ([]Event, error) {
	rows, e := s.DB.QueryContext(ctx, "SELECT seq,kind,payload,created FROM live_events WHERE broadcast=? AND seq>? ORDER BY seq LIMIT 200", b, after)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var v Event
		var p string
		if e = rows.Scan(&v.Seq, &v.Kind, &p, &v.Created); e != nil {
			return nil, e
		}
		v.Payload = json.RawMessage(p)
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Service) Wallet(ctx context.Context, u identity.User) (any, error) {
	if u.ID == "" {
		return nil, identity.ErrUnauthorized
	}
	day := s.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02")
	remaining := 100
	e := s.DB.QueryRowContext(ctx, "SELECT remaining FROM live_wallets WHERE user_id=? AND day=?", u.ID, day).Scan(&remaining)
	if e != nil && e != sql.ErrNoRows {
		return nil, e
	}
	return map[string]any{"day": day, "remaining": remaining}, nil
}
func (s *Service) DailyBoard(ctx context.Context) (any, error) {
	day := s.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02")
	rows, e := s.DB.QueryContext(ctx, "SELECT u.name,SUM(g.points) FROM live_gifts g JOIN identity_users u ON u.id=g.owner WHERE day=? GROUP BY owner ORDER BY SUM(g.points) DESC,u.name LIMIT 20", day)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var name string
		var points int
		if e = rows.Scan(&name, &points); e != nil {
			return nil, e
		}
		out = append(out, map[string]any{"name": name, "points": points})
	}
	return out, rows.Err()
}
func (s *Service) Admit(ctx context.Context, grant, room, who string) bool {
	var count int
	e := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM live_grants g JOIN live_broadcasts b ON b.id=g.broadcast LEFT JOIN identity_users u ON u.id=g.user_id WHERE g.id=? AND g.media_room=? AND g.identity=? AND g.revoked=0 AND g.expires>? AND b.state!='ended' AND (g.user_id='' OR u.disabled=0)`, grant, room, who, s.Now().Unix()).Scan(&count)
	return e == nil && count == 1
}
func (s *Service) Sweep(ctx context.Context) error {
	_, e := s.transact(ctx, func(t *dbtx) (any, error) {
		rows, e := t.Query("SELECT id FROM live_pks WHERE state IN ('active','invited')")
		if e != nil {
			return nil, e
		}
		var ids []string
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				return nil, e
			}
			ids = append(ids, id)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
		for _, id := range ids {
			p, e := getPK(t, id)
			if e != nil {
				return nil, e
			}
			if p.State == "invited" {
				if t.now-p.Created > 60 {
					s.finishPK(t, p, "cancelled", "")
				}
				continue
			}
			a, e := broadcast(t, p.A)
			if e != nil {
				return nil, e
			}
			b, e := broadcast(t, p.B)
			if e != nil {
				return nil, e
			}
			winner := ""
			finish := false
			state := "finished"
			aLost := t.now-a.Heartbeat > 30
			bLost := t.now-b.Heartbeat > 30
			if aLost && bLost {
				state = "cancelled"
				finish = true
			} else if aLost {
				winner = p.B
				finish = true
			} else if bLost {
				winner = p.A
				finish = true
			} else if t.now >= p.Deadline {
				finish = true
				if p.ScoreA > p.ScoreB {
					winner = p.A
				} else if p.ScoreB > p.ScoreA {
					winner = p.B
				}
			}
			if finish {
				s.finishPK(t, p, state, winner)
				t.cleanup(p.Stage, "")
			}
		}
		rows, e = t.Query("SELECT id FROM live_broadcasts WHERE state!='ended' AND heartbeat<?", t.now-60)
		if e != nil {
			return nil, e
		}
		ids = nil
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				return nil, e
			}
			ids = append(ids, id)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
		for _, id := range ids {
			b, e := broadcast(t, id)
			if e != nil {
				return nil, e
			}
			s.end(t, b, ctx)
		}
		t.exec("DELETE FROM identity_sessions WHERE expires<?", t.now)
		return nil, nil
	})
	return e
}
func Status(err error) (int, string) {
	var e *Error
	if errors.As(err, &e) {
		return e.Status, e.Code
	}
	if errors.Is(err, identity.ErrUnauthorized) {
		return 401, "unauthorized"
	}
	if errors.Is(err, identity.ErrInvalid) {
		return 400, "invalid_account"
	}
	if err != nil && err.Error() == "account_unavailable" {
		return 409, err.Error()
	}
	return 500, "internal_error"
}

func (t *dbtx) cleanup(room, who string) {
	if room != "" {
		t.exec("INSERT OR IGNORE INTO live_media_cleanup(media_room,identity) VALUES(?,?)", room, who)
	}
}

// Failed effects remain durable across service restarts.
func (s *Service) drainMedia(ctx context.Context) {
	ctx, cancelAll := context.WithTimeout(ctx, 3*time.Second)
	defer cancelAll()
	rows, err := s.DB.QueryContext(ctx, "SELECT media_room,identity FROM live_media_cleanup LIMIT 32")
	if err != nil {
		return
	}
	type effect struct{ room, who string }
	var effects []effect
	for rows.Next() {
		var e effect
		if rows.Scan(&e.room, &e.who) != nil {
			rows.Close()
			return
		}
		effects = append(effects, e)
	}
	rows.Close()
	for _, e := range effects {
		if ctx.Err() != nil {
			return
		}
		call, cancel := context.WithTimeout(ctx, time.Second)
		var err error
		if e.who == "" {
			err = s.Media.Delete(call, e.room)
		} else {
			err = s.Media.Remove(call, e.room, e.who)
		}
		cancel()
		if err == nil {
			_, _ = s.DB.ExecContext(ctx, "DELETE FROM live_media_cleanup WHERE media_room=? AND identity=?", e.room, e.who)
		}
	}
}
