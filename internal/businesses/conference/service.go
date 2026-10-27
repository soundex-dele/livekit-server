// Package conference owns meeting policy. Media credentials are issued only after admission.
package conference

import (
	"av-platform/business/internal/platform/identity"
	"av-platform/business/migrations"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const Prefix = "/api/v1/conference/"

type Failure struct {
	Code   string
	Status int
}

func (e *Failure) Error() string { return e.Code }
func fail(c string, s int) error { return &Failure{c, s} }

type Peer struct {
	Identity string
	Tracks   []Track
}
type Track struct {
	SID    string
	Source int
	Muted  bool
}
type Media interface {
	Credentials(context.Context, string, string, string, string, bool) (map[string]any, error)
	Participants(context.Context, string) ([]Peer, error)
	Remove(context.Context, string, string) error
	Delete(context.Context, string) error
	Mute(context.Context, string, string, string) error
	Sharing(context.Context, string, string, bool) error
}
type Member struct {
	ID             string `json:"id"`
	UserID         string `json:"user_id,omitempty"`
	Name           string `json:"name"`
	Identity       string `json:"identity"`
	Status         string `json:"status"`
	Joined         int64  `json:"joined"`
	Seen           int64  `json:"seen"`
	Grant          string `json:"grant,omitempty"`
	CredentialHash string `json:"credential_hash,omitempty"`
	UnmuteRequest  int64  `json:"unmute_request"`
	Online         bool   `json:"online"`
}
type Job struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Member string `json:"member,omitempty"`
	State  string `json:"state"`
	Error  string `json:"error,omitempty"`
}
type Meeting struct {
	ID           string             `json:"id"`
	Number       string             `json:"number"`
	Room         string             `json:"room,omitempty"`
	Owner        string             `json:"owner"`
	Host         string             `json:"host"`
	Title        string             `json:"title"`
	Starts       string             `json:"starts_at"`
	Duration     int                `json:"duration_minutes"`
	State        string             `json:"state"`
	PasswordHash string             `json:"password_hash,omitempty"`
	Waiting      bool               `json:"waiting_room"`
	Locked       bool               `json:"locked"`
	Version      int64              `json:"version"`
	Created      int64              `json:"created"`
	Sharing      string             `json:"sharing_member,omitempty"`
	Members      map[string]*Member `json:"members"`
	Jobs         []*Job             `json:"operations"`
	HostMissing  int64              `json:"host_missing,omitempty"`
}
type Command struct {
	Title     string `json:"title"`
	Starts    string `json:"starts_at"`
	Duration  int    `json:"duration_minutes"`
	Password  string `json:"password"`
	Waiting   *bool  `json:"waiting_room,omitempty"`
	Scheduled bool   `json:"scheduled"`
	Number    string `json:"number"`
	Name      string `json:"name"`
	Target    string `json:"target"`
	Enabled   bool   `json:"enabled"`
	MemberID  string `json:"member_id"`
}
type Actor struct {
	User       identity.User
	Credential string
}
type Service struct {
	DB     *sql.DB
	Media  Media
	mu     sync.Mutex
	Now    func() time.Time
	number func() (string, error)
}

var schema = migrations.Conference

func New(db *sql.DB, m Media) (*Service, error) {
	_, e := db.Exec(schema)
	return &Service{DB: db, Media: m, Now: time.Now, number: randomNumber}, e
}
func randomNumber() (string, error) {
	n, e := rand.Int(rand.Reader, big.NewInt(900000000))
	if e != nil {
		return "", e
	}
	return fmt.Sprint(n.Int64() + 100000000), nil
}
func digest(s string) string { x := sha256.Sum256([]byte(s)); return hex.EncodeToString(x[:]) }
func (s *Service) load(ctx context.Context, id string) (*Meeting, error) {
	var raw string
	e := s.DB.QueryRowContext(ctx, "SELECT data FROM conference_meetings WHERE id=? OR number=?", id, id).Scan(&raw)
	if e == sql.ErrNoRows {
		return nil, fail("meeting_not_found", 404)
	}
	if e != nil {
		return nil, e
	}
	var m Meeting
	e = json.Unmarshal([]byte(raw), &m)
	return &m, e
}
func (s *Service) save(ctx context.Context, m *Meeting, a, action, key, request string, result any) error {
	m.Version++
	raw, e := json.Marshal(m)
	if e != nil {
		return e
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	_, e = tx.ExecContext(ctx, "INSERT INTO conference_meetings(id,number,data) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", m.ID, m.Number, string(raw))
	if e != nil {
		return e
	}
	if action != "presence" {
		_, e = tx.ExecContext(ctx, "INSERT INTO conference_audit(meeting_id,actor,action,created) VALUES(?,?,?,?)", m.ID, a, action, s.Now().Unix())
		if e != nil {
			return e
		}
	}
	if key != "" {
		b, _ := json.Marshal(result)
		_, e = tx.ExecContext(ctx, "INSERT INTO conference_idempotency(actor,key,request,response) VALUES(?,?,?,?)", a, key, request, string(b))
		if e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (s *Service) replay(ctx context.Context, a, key, req string) (any, error) {
	if key == "" {
		return nil, fail("idempotency_key_required", 400)
	}
	var previous, raw string
	e := s.DB.QueryRowContext(ctx, "SELECT request,response FROM conference_idempotency WHERE actor=? AND key=?", a, key).Scan(&previous, &raw)
	if e == sql.ErrNoRows {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	if previous != req {
		return nil, fail("idempotency_conflict", 409)
	}
	var out any
	e = json.Unmarshal([]byte(raw), &out)
	return out, e
}
func subject(a Actor) string {
	if a.User.ID != "" {
		return "user:" + a.User.ID
	}
	return "guest:" + digest(a.Credential)
}
func member(m *Meeting, a Actor) *Member {
	for _, p := range m.Members {
		if a.User.ID != "" && p.UserID == a.User.ID {
			return p
		}
		if a.User.ID == "" && a.Credential != "" && p.CredentialHash == digest(a.Credential) {
			return p
		}
	}
	return nil
}
func cleanTitle(c *Command) error {
	c.Title = strings.TrimSpace(c.Title)
	if !utf8.ValidString(c.Title) || utf8.RuneCountInString(c.Title) < 1 || utf8.RuneCountInString(c.Title) > 100 {
		return fail("invalid_title", 400)
	}
	if c.Duration == 0 {
		c.Duration = 60
	}
	if c.Duration < 1 || c.Duration > 1440 {
		return fail("invalid_duration", 400)
	}
	if len(c.Password) > 64 {
		return fail("invalid_password", 400)
	}
	if c.Starts != "" {
		parsed, e := time.Parse(time.RFC3339, c.Starts)
		if e != nil {
			return fail("invalid_time", 400)
		}
		c.Starts = parsed.UTC().Format(time.RFC3339)
	}
	return nil
}
func (s *Service) Create(ctx context.Context, a Actor, c Command, key string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.User.ID == "" {
		return nil, fail("login_required", 401)
	}
	if e := cleanTitle(&c); e != nil {
		return nil, e
	}
	b, _ := json.Marshal(c)
	req := "create:" + string(b)
	who := subject(a)
	if old, e := s.replay(ctx, who, key, req); e != nil || old != nil {
		return old, e
	}
	now := s.Now()
	if c.Starts == "" {
		c.Starts = now.UTC().Format(time.RFC3339)
	}
	m := &Meeting{ID: identity.ID(), Owner: a.User.ID, Title: c.Title, Starts: c.Starts, Duration: c.Duration, State: "active", Waiting: true, Created: now.Unix(), Members: map[string]*Member{}, Jobs: []*Job{}}
	m.Room = "conference_" + m.ID
	if c.Scheduled {
		m.State = "scheduled"
	}
	if c.Waiting != nil {
		m.Waiting = *c.Waiting
	}
	if c.Password != "" {
		m.PasswordHash = digest(m.ID + ":" + c.Password)
	}
	for {
		n, e := s.number()
		if e != nil {
			return nil, e
		}
		m.Number = n
		var count int
		if e = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM conference_meetings WHERE number=?", m.Number).Scan(&count); e != nil {
			return nil, e
		}
		if count == 0 {
			break
		}
	}
	p := &Member{ID: identity.ID(), UserID: a.User.ID, Name: a.User.Name, Status: "approved", Grant: identity.ID()}
	p.Identity = "conference_" + p.ID
	m.Members[p.ID] = p
	m.Host = p.ID
	out := map[string]any{"id": m.ID, "number": m.Number}
	if e := s.save(ctx, m, who, "create", key, req, out); e != nil {
		return nil, e
	}
	return out, nil
}
func (s *Service) List(ctx context.Context, a Actor) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.User.ID == "" {
		return []any{}, nil
	}
	rows, e := s.DB.QueryContext(ctx, "SELECT data FROM conference_meetings ORDER BY rowid DESC")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var raw string
		if e = rows.Scan(&raw); e != nil {
			return nil, e
		}
		var m Meeting
		if e = json.Unmarshal([]byte(raw), &m); e != nil {
			return nil, e
		}
		p := member(&m, a)
		if m.Owner == a.User.ID || (p != nil && p.Joined > 0) {
			out = append(out, summary(&m))
		}
	}
	return out, rows.Err()
}
func summary(m *Meeting) map[string]any {
	return map[string]any{"id": m.ID, "number": m.Number, "title": m.Title, "starts_at": m.Starts, "duration_minutes": m.Duration, "state": m.State, "owner": m.Owner, "waiting_room": m.Waiting, "locked": m.Locked, "has_password": m.PasswordHash != ""}
}
func view(m *Meeting, p *Member) map[string]any {
	out := summary(m)
	out["version"] = m.Version
	out["host"] = m.Host
	out["sharing_member"] = m.Sharing
	if p.Status == "approved" {
		out["operations"] = m.Jobs
	} else {
		out["operations"] = []*Job{}
	}
	out["self"] = p.ID
	out["status"] = p.Status
	list := []any{}
	for _, v := range m.Members {
		if v.ID == p.ID || p.ID == m.Host || (p.Status == "approved" && v.Status == "approved") {
			list = append(list, map[string]any{"id": v.ID, "name": v.Name, "identity": v.Identity, "status": v.Status, "online": v.Online, "registered": v.UserID != "", "unmute_request": v.UnmuteRequest, "joined": v.Joined})
		}
	}
	out["members"] = list
	return out
}
func (s *Service) Join(ctx context.Context, a Actor, c Command, key string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.User.ID == "" && len(a.Credential) < 32 {
		return nil, fail("guest_session_required", 401)
	}
	c.Name = strings.TrimSpace(c.Name)
	if !utf8.ValidString(c.Name) || utf8.RuneCountInString(c.Name) < 1 || utf8.RuneCountInString(c.Name) > 40 {
		return nil, fail("invalid_name", 400)
	}
	m, e := s.load(ctx, c.Number)
	if e != nil {
		return nil, e
	}
	if m.State != "active" && m.State != "scheduled" {
		return nil, fail("meeting_ended", 409)
	}
	p := member(m, a)
	if p != nil && (p.Status == "removed" || p.Status == "rejected") {
		return nil, fail("admission_denied", 403)
	}
	if p == nil || p.Status == "left" {
		if p != nil {
			for _, job := range m.Jobs {
				if job.Kind == "remove" && job.Member == p.ID && job.State != "done" {
					return nil, fail("membership_cleanup_pending", 409)
				}
			}
		}
		if m.Locked {
			return nil, fail("meeting_locked", 403)
		}
		if m.PasswordHash != "" && subtle.ConstantTimeCompare([]byte(m.PasswordHash), []byte(digest(m.ID+":"+c.Password))) != 1 {
			return nil, fail("incorrect_password", 403)
		}
		count := 0
		for _, v := range m.Members {
			if v.Status == "approved" || v.Status == "waiting" {
				count++
			}
		}
		if count >= 25 {
			return nil, fail("meeting_full", 409)
		}
		if p == nil {
			p = &Member{ID: identity.ID(), UserID: a.User.ID, CredentialHash: digest(a.Credential)}
			p.Identity = "conference_" + p.ID
			m.Members[p.ID] = p
		}
		p.Status = "approved"
		if p.ID != m.Host && (m.Waiting || a.User.ID == "") {
			p.Status = "waiting"
		}
		p.Grant = identity.ID()
	}
	p.Name = c.Name
	p.Seen = s.Now().Unix()
	out := map[string]any{"id": m.ID, "member_id": p.ID}
	e = s.save(ctx, m, subject(a), "join", "", "", nil)
	return out, e
}
func (s *Service) Snapshot(ctx context.Context, id string, a Actor) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, e := s.load(ctx, id)
	if e != nil {
		return nil, e
	}
	p := member(m, a)
	if p == nil {
		return nil, fail("forbidden", 403)
	}
	return view(m, p), nil
}
func (s *Service) Credentials(ctx context.Context, id string, a Actor) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, e := s.load(ctx, id)
	if e != nil {
		return nil, e
	}
	p := member(m, a)
	if p == nil || p.Status != "approved" {
		return nil, fail("not_admitted", 403)
	}
	if m.State != "active" {
		return nil, fail("meeting_not_active", 409)
	}
	p.Seen = s.Now().Unix()
	if e = s.save(ctx, m, subject(a), "credentials", "", "", nil); e != nil {
		return nil, e
	}
	return s.Media.Credentials(ctx, m.Room, p.Identity, p.Name, p.Grant, m.Sharing == p.ID)
}
func (s *Service) Admitted(ctx context.Context, room, who, grant string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, e := s.load(ctx, strings.TrimPrefix(room, "conference_"))
	if e != nil || m.Room != room || m.State != "active" {
		return false
	}
	for _, p := range m.Members {
		if p.Identity == who && p.Status == "approved" && p.Grant == grant && grant != "" {
			return true
		}
	}
	return false
}
func (s *Service) Execute(ctx context.Context, id, action string, a Actor, c Command, key string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, e := s.load(ctx, id)
	if e != nil {
		return nil, e
	}
	p := member(m, a)
	if p == nil {
		return nil, fail("forbidden", 403)
	}
	who := subject(a)
	b, _ := json.Marshal(c)
	req := id + ":" + action + ":" + string(b)
	if action != "presence" {
		if old, e := s.replay(ctx, who, key, req); e != nil || old != nil {
			return old, e
		}
	}
	if action == "presence" {
		if p.Status != "approved" && p.Status != "waiting" {
			return view(m, p), nil
		}
		if p.Status == "waiting" {
			p.Seen = s.Now().Unix()
		}
		e = s.save(ctx, m, who, action, "", "", nil)
		return view(m, p), e
	}
	if m.State == "ended" || m.State == "cancelled" || m.State == "ending" {
		return nil, fail("meeting_ended", 409)
	}
	host := m.Host == p.ID
	target := m.Members[c.Target]
	queue := func(kind, member string) {
		m.Jobs = append(m.Jobs, &Job{ID: identity.ID(), Kind: kind, Member: member, State: "pending"})
	}
	switch action {
	case "start":
		if m.Owner != a.User.ID || m.State != "scheduled" {
			return nil, fail("forbidden", 403)
		}
		m.State = "active"
	case "edit":
		if m.Owner != a.User.ID || m.State != "scheduled" {
			return nil, fail("forbidden", 403)
		}
		if e = cleanTitle(&c); e != nil {
			return nil, e
		}
		m.Title = c.Title
		m.Starts = c.Starts
		m.Duration = c.Duration
		if c.Waiting != nil {
			m.Waiting = *c.Waiting
		}
		if c.Password != "" {
			m.PasswordHash = digest(m.ID + ":" + c.Password)
		}
	case "cancel":
		if m.Owner != a.User.ID || m.State != "scheduled" {
			return nil, fail("forbidden", 403)
		}
		m.State = "cancelled"
	case "approve", "reject", "remove", "mute", "request-unmute", "transfer":
		if !host || target == nil {
			return nil, fail("forbidden", 403)
		}
		switch action {
		case "approve", "reject":
			if target.Status != "waiting" {
				return nil, fail("state_conflict", 409)
			}
			target.Status = "approved"
			if action == "reject" {
				target.Status = "rejected"
			}
		case "remove":
			if target.ID == p.ID {
				return nil, fail("cannot_remove_host", 409)
			}
			target.Status = "removed"
			target.Grant = ""
			queue("remove", target.ID)
			if m.Sharing == target.ID {
				m.Sharing = ""
			}
		case "mute":
			queue("mute", target.ID)
		case "request-unmute":
			target.UnmuteRequest = s.Now().UnixMilli()
		case "transfer":
			if target.UserID == "" || target.Status != "approved" || !target.Online {
				return nil, fail("invalid_host", 409)
			}
			m.Host = target.ID
			m.HostMissing = 0
		}
	case "lock", "mute-all", "end":
		if !host {
			return nil, fail("forbidden", 403)
		}
		if action == "lock" {
			m.Locked = c.Enabled
		}
		if action == "mute-all" {
			for _, v := range m.Members {
				if v.ID != p.ID && v.Status == "approved" {
					queue("mute", v.ID)
				}
			}
		}
		if action == "end" {
			m.State = "ending"
			queue("delete", "")
		}
	case "leave":
		if host && m.State == "active" {
			return nil, fail("transfer_or_end_required", 409)
		}
		p.Status = "left"
		p.Grant = ""
		queue("remove", p.ID)
		if m.Sharing == p.ID {
			m.Sharing = ""
		}
	case "share":
		if p.Status != "approved" || m.State != "active" {
			return nil, fail("not_admitted", 403)
		}
		if c.Enabled {
			// Do not grant a second publisher while the previous publisher's
			// permission revocation or removal is still in flight.
			for _, job := range m.Jobs {
				if job.State != "done" && (job.Kind == "share-off" || job.Kind == "remove") {
					return nil, fail("sharing_busy", 409)
				}
			}
			if m.Sharing != "" && m.Sharing != p.ID {
				return nil, fail("sharing_busy", 409)
			}
			m.Sharing = p.ID
			queue("share-on", p.ID)
		} else {
			if m.Sharing != p.ID {
				return nil, fail("forbidden", 403)
			}
			m.Sharing = ""
			p.Grant = identity.ID()
			queue("share-off", p.ID)
		}
	default:
		return nil, fail("not_found", 404)
	}
	out := map[string]any{"accepted": true, "id": m.ID, "operations": m.Jobs}
	if e = s.save(ctx, m, who, action, key, req, out); e != nil {
		return nil, e
	}
	return out, nil
}

// Sweep retries durable media work and reconciles actual presence, not HTTP heartbeats.
func (s *Service) Sweep(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, e := s.DB.QueryContext(ctx, "SELECT id FROM conference_meetings")
	if e != nil {
		return e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		m, e := s.load(ctx, id)
		if e != nil {
			return e
		}
		if m.State == "ended" || m.State == "cancelled" {
			continue
		}
		changed := false
		oldHost, oldState := m.Host, m.State
		attempted := 0
		for _, j := range m.Jobs {
			// Room deletion supersedes all participant-level commands. Failed
			// mute/removal retries must never starve end-meeting cleanup.
			if m.State == "ending" && j.Kind != "delete" {
				continue
			}
			if attempted >= 2 {
				break
			}
			if j.State == "done" {
				continue
			}
			attempted++
			p := m.Members[j.Member]
			var err error
			switch j.Kind {
			case "delete":
				err = s.Media.Delete(ctx, m.Room)
				if err == nil {
					m.State = "ended"
					for _, pending := range m.Jobs {
						pending.State = "done"
						pending.Error = ""
					}
					m.Sharing = ""
					for _, v := range m.Members {
						v.Online = false
						v.Grant = ""
					}
				}
			case "remove":
				if p != nil {
					err = s.Media.Remove(ctx, m.Room, p.Identity)
				}
			case "share-on", "share-off":
				if p != nil {
					err = s.Media.Sharing(ctx, m.Room, p.Identity, j.Kind == "share-on" && m.Sharing == p.ID)
				}
			case "mute":
				if p != nil {
					var peers []Peer
					peers, err = s.Media.Participants(ctx, m.Room)
					if err == nil {
						for _, peer := range peers {
							if peer.Identity == p.Identity {
								for _, t := range peer.Tracks {
									if t.Source == 2 {
										err = s.Media.Mute(ctx, m.Room, p.Identity, t.SID)
										if err != nil {
											break
										}
									}
								}
							}
						}
					}
				}
			}
			j.Error = ""
			j.State = "done"
			if err != nil {
				j.State = "retrying"
				j.Error = "media_unavailable"
			}
			changed = true
		}
		for _, p := range m.Members {
			if p.Status == "waiting" && s.Now().Unix()-p.Seen > 120 {
				p.Status = "left"
				p.Grant = ""
				changed = true
			}
		}
		if m.State == "active" {
			peers, err := s.Media.Participants(ctx, m.Room)
			if err == nil {
				online := map[string]bool{}
				for _, peer := range peers {
					online[peer.Identity] = true
				}
				for _, p := range m.Members {
					next := online[p.Identity] && p.Status == "approved"
					if next != p.Online {
						changed = true
					}
					p.Online = next
					if next {
						p.Seen = s.Now().Unix()
						if p.Joined == 0 {
							p.Joined = p.Seen
						}
						changed = true
					}
				}
				for _, p := range m.Members {
					if p.ID != m.Host && p.Status == "approved" && !p.Online && p.Seen > 0 && s.Now().Unix()-p.Seen > 120 {
						p.Status = "left"
						p.Grant = ""
						changed = true
					}
				}
				h := m.Members[m.Host]
				if h != nil && h.Online {
					m.HostMissing = 0
				} else {
					if m.HostMissing == 0 {
						m.HostMissing = s.Now().Unix()
						changed = true
					}
					if s.Now().Unix()-m.HostMissing >= 120 {
						var next *Member
						for _, p := range m.Members {
							if p.Online && p.UserID != "" && (next == nil || p.Joined < next.Joined) {
								next = p
							}
						}
						if next != nil {
							m.Host = next.ID
							m.HostMissing = 0
						} else {
							m.State = "ending"
							m.Jobs = append(m.Jobs, &Job{ID: identity.ID(), Kind: "delete", State: "pending"})
						}
						changed = true
					}
				}
				if m.Sharing != "" {
					p := m.Members[m.Sharing]
					if p == nil || (!p.Online && s.Now().Unix()-p.Seen > 15) {
						m.Sharing = ""
						if p != nil {
							p.Grant = identity.ID()
							m.Jobs = append(m.Jobs, &Job{ID: identity.ID(), Kind: "share-off", Member: p.ID, State: "pending"})
						}
						changed = true
					}
				}
			}
		}
		if changed {
			action := "presence"
			if attempted > 0 || oldHost != m.Host || oldState != m.State {
				action = "reconcile"
			}
			if e = s.save(ctx, m, "system", action, "", "", nil); e != nil {
				return e
			}
		}
	}
	return nil
}
