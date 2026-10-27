// Package application owns the single-process screen-cast state machine.
package application

import (
	"av-platform/business/internal/businesses/screencast/domain"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"math/big"
	"sync"
	"time"
)

type Store interface {
	Load(context.Context) ([]domain.Session, error)
	Save(context.Context, domain.Session) error
}
type Media interface {
	Connect(context.Context, domain.Session, string) (map[string]any, error)
	Delete(context.Context, string) error
}
type Error struct{ Code string }

func (e *Error) Error() string { return e.Code }
func fail(code string) error   { return &Error{code} }

type Command struct {
	Action, ID, Credential, Code, Source string
	Connected                            bool
}
type Service struct {
	mu       sync.Mutex
	store    Store
	media    Media
	now      func() time.Time
	sessions map[string]domain.Session
	limits   map[string]bucket
}
type bucket struct {
	At    time.Time
	Count int
}

func hash(s string) string { v := sha256.Sum256([]byte(s)); return hex.EncodeToString(v[:]) }
func secret() (string, error) {
	b := make([]byte, 32)
	_, e := rand.Read(b)
	return hex.EncodeToString(b), e
}
func New(ctx context.Context, store Store, media Media, now func() time.Time) (*Service, error) {
	if now == nil {
		now = time.Now
	}
	s := &Service{store: store, media: media, now: now, sessions: map[string]domain.Session{}, limits: map[string]bucket{}}
	all, e := store.Load(ctx)
	if e != nil {
		return nil, e
	}
	for _, v := range all {
		if !v.Terminal() {
			s.end(&v, "ended", "service_restart")
		}
		if e = store.Save(ctx, v); e != nil {
			return nil, e
		}
		s.sessions[v.ID] = v
	}
	s.Sweep(ctx)
	return s, nil
}
func (s *Service) save(ctx context.Context, v domain.Session) error {
	if s.store.Save(ctx, v) != nil {
		return fail("storage_error")
	}
	s.sessions[v.ID] = v
	return nil
}
func (s *Service) end(v *domain.Session, state, reason string) {
	v.State = state
	v.Reason = reason
	v.CodeHash = ""
	v.Cleanup = !v.Paired.IsZero()
	v.CleanupUntil = s.now().Add(65 * time.Second)
}
func (s *Service) expire(v *domain.Session) {
	now := s.now()
	if v.Terminal() {
		return
	}
	if v.State == "waiting" {
		if !now.Before(v.Expires) {
			s.end(v, "expired", "code_expired")
		}
		return
	}
	if v.State == "paired" && !now.Before(v.Paired.Add(30*time.Second)) {
		s.end(v, "ended", "connect_timeout")
		return
	}
	if !now.Before(v.ReceiverSeen.Add(30*time.Second)) || !now.Before(v.SenderSeen.Add(30*time.Second)) {
		s.end(v, "ended", "heartbeat_timeout")
	}
}
func (s *Service) allow(key string, max int) bool {
	now := s.now()
	for k, v := range s.limits {
		if !now.Before(v.At.Add(time.Minute)) {
			delete(s.limits, k)
		}
	}
	b := s.limits[key]
	if b.Count == 0 {
		if len(s.limits) >= 4096 {
			return false
		}
		b.At = now
	}
	b.Count++
	s.limits[key] = b
	return b.Count <= max
}
func view(v domain.Session) map[string]any {
	return map[string]any{"id": v.ID, "state": v.State, "reason": v.Reason, "expires_at": v.Expires.UTC().Format(time.RFC3339), "heartbeat_interval_ms": 3000}
}
func role(v domain.Session, key string) string {
	if key == "" {
		return ""
	}
	h := hash(key)
	if subtle.ConstantTimeCompare([]byte(h), []byte(v.ReceiverHash)) == 1 {
		return "receiver"
	}
	if subtle.ConstantTimeCompare([]byte(h), []byte(v.SenderHash)) == 1 {
		return "sender"
	}
	return ""
}
func (s *Service) Execute(ctx context.Context, c Command) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.Action == "create" || c.Action == "redeem" {
		max := 10
		if c.Action == "redeem" {
			max = 30
		}
		if !s.allow(c.Action+":"+c.Source, max) {
			return nil, fail("rate_limited")
		}
	}
	if c.Action == "create" {
		id, e := secret()
		if e != nil {
			return nil, e
		}
		key, e := secret()
		if e != nil {
			return nil, e
		}
		var code string
		for {
			n, e := rand.Int(rand.Reader, big.NewInt(10000000000))
			if e != nil {
				return nil, e
			}
			code = n.String()
			for len(code) < 10 {
				code = "0" + code
			}
			collision := false
			for _, v := range s.sessions {
				if v.CodeHash == hash(code) {
					collision = true
					break
				}
			}
			if !collision {
				break
			}
		}
		v := domain.Session{ID: id, Room: "screencast-" + id, State: "waiting", ReceiverHash: hash(key), CodeHash: hash(code), Expires: s.now().Add(5 * time.Minute)}
		if e = s.save(ctx, v); e != nil {
			return nil, e
		}
		out := view(v)
		out["credential"] = key
		out["code"] = code
		out["role"] = "receiver"
		return out, nil
	}
	if c.Action == "redeem" {
		if len(c.Code) != 10 {
			return nil, fail("code_unavailable")
		}
		for _, ch := range c.Code {
			if ch < '0' || ch > '9' {
				return nil, fail("code_unavailable")
			}
		}
		for _, v := range s.sessions {
			if v.CodeHash != hash(c.Code) {
				continue
			}
			s.expire(&v)
			if v.State != "waiting" {
				if e := s.save(ctx, v); e != nil {
					return nil, e
				}
				return nil, fail("code_unavailable")
			}
			key, e := secret()
			if e != nil {
				return nil, e
			}
			v.SenderHash = hash(key)
			v.CodeHash = ""
			v.State = "paired"
			v.Paired = s.now()
			v.ReceiverSeen = v.Paired
			v.SenderSeen = v.Paired
			if e = s.save(ctx, v); e != nil {
				return nil, e
			}
			out := view(v)
			out["credential"] = key
			out["role"] = "sender"
			return out, nil
		}
		return nil, fail("code_unavailable")
	}
	v, ok := s.sessions[c.ID]
	if !ok {
		return nil, fail("not_found")
	}
	who := role(v, c.Credential)
	if who == "" {
		return nil, fail("unauthorized")
	}
	old := v
	s.expire(&v)
	if old.State != v.State {
		if e := s.save(ctx, v); e != nil {
			return nil, e
		}
	}
	switch c.Action {
	case "get":
		return view(v), nil
	case "stop":
		if !v.Terminal() {
			s.end(&v, "ended", "stopped")
		}
		if e := s.save(ctx, v); e != nil {
			return nil, e
		}
		return view(v), nil
	case "heartbeat":
		if v.State != "paired" && v.State != "streaming" {
			return view(v), nil
		}
		if who == "receiver" {
			v.ReceiverSeen = s.now()
			v.ReceiverConnected = c.Connected
		} else {
			v.SenderSeen = s.now()
			v.SenderConnected = c.Connected
		}
		if v.State == "streaming" && !c.Connected {
			s.end(&v, "ended", "media_disconnected")
		} else if v.ReceiverConnected && v.SenderConnected {
			v.State = "streaming"
		}
		if e := s.save(ctx, v); e != nil {
			return nil, e
		}
		return view(v), nil
	case "connect":
		if v.State != "paired" && v.State != "streaming" {
			return nil, fail("session_inactive")
		}
		out, e := s.media.Connect(ctx, v, who)
		if e != nil {
			return nil, fail("media_unavailable")
		}
		return out, nil
	default:
		return nil, fail("invalid_request")
	}
}
func (s *Service) Sweep(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.sessions {
		old := v
		s.expire(&v)
		if old != v {
			if s.save(ctx, v) != nil {
				continue
			}
		}
		if v.Cleanup && s.media.Delete(ctx, v.Room) == nil && !s.now().Before(v.CleanupUntil) {
			v.Cleanup = false
			_ = s.save(ctx, v)
		}
	}
}
