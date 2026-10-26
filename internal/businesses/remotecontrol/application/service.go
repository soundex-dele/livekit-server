package application

import (
	"av-platform/business/internal/businesses/remotecontrol/domain"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"sync"
	"time"
)

const Lease = 10 * time.Second

type Media interface {
	Connect(context.Context, domain.Session, string) (map[string]any, error)
	Delete(context.Context, string) error
}
type limit struct {
	count int
	until time.Time
}
type Service struct {
	mu       sync.Mutex
	store    Store
	media    Media
	sessions map[string]domain.Session
	limits   map[string]limit
	now      func() time.Time
}

func New(ctx context.Context, store Store, media Media, now func() time.Time) (*Service, error) {
	if now == nil {
		now = time.Now
	}
	s := &Service{store: store, media: media, sessions: map[string]domain.Session{}, limits: map[string]limit{}, now: now}
	items, err := store.Load(ctx)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if active(item.State) {
			item.State = "revoked"
			item.Cleanup = true
			if err = store.Save(ctx, item, "restart_revoked"); err != nil {
				return nil, err
			}
		}
		s.sessions[item.ID] = item
	}
	return s, nil
}
func active(state string) bool {
	return state == "waiting" || state == "pending" || state == "approved"
}
func randomToken() string {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func digest(v string) string { b := sha256.Sum256([]byte(v)); return hex.EncodeToString(b[:]) }
func code() string {
	n, e := rand.Int(rand.Reader, big.NewInt(10000000000))
	if e != nil {
		panic(e)
	}
	return fmt.Sprintf("%010d", n.Int64())
}
func (s *Service) save(ctx context.Context, v domain.Session, action string) error {
	if err := s.store.Save(ctx, v, action); err != nil {
		return err
	}
	s.sessions[v.ID] = v
	return nil
}
func expired(v domain.Session, now time.Time) bool {
	return (v.State == "waiting" || v.State == "pending") && !now.Before(v.Expires) || v.State == "approved" && !now.Before(v.LeaseUntil)
}
func (s *Service) Sweep(ctx context.Context) {
	s.mu.Lock()
	var cleanup []domain.Session
	for _, v := range s.sessions {
		if expired(v, s.now()) {
			v.State = "expired"
			v.Cleanup = true
			if s.save(ctx, v, "expired") != nil {
				continue
			}
		}
		if v.Cleanup {
			cleanup = append(cleanup, v)
		}
	}
	for k, v := range s.limits {
		if !s.now().Before(v.until) {
			delete(s.limits, k)
		}
	}
	s.mu.Unlock()
	for _, v := range cleanup {
		if s.media.Delete(ctx, "rc_"+v.ID) != nil {
			continue
		}
		s.mu.Lock()
		current := s.sessions[v.ID]
		if current.Cleanup {
			current.Cleanup = false
			_ = s.save(ctx, current, "media_cleaned")
		}
		s.mu.Unlock()
	}
}
func (s *Service) view(v domain.Session) map[string]any {
	remaining := int64(0)
	if v.State == "approved" {
		remaining = v.LeaseUntil.Sub(s.now()).Milliseconds()
		if remaining < 0 {
			remaining = 0
		}
	}
	return map[string]any{"id": v.ID, "grant_id": v.ID, "state": v.State, "controller_name": v.Name, "epoch": v.Epoch, "expires_at": v.Expires.UTC().Format(time.RFC3339Nano), "lease_expires_at": v.LeaseUntil.UTC().Format(time.RFC3339Nano), "lease_remaining_ms": remaining, "host_identity": "host_" + v.ID, "controller_identity": "controller_" + v.ID, "scope": []string{"screen", "keyboard", "mouse"}}
}

// Execute owns authorization and transitions independently of the HTTP transport.
func (s *Service) Execute(ctx context.Context, cmd Command) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cmd.Action == "create" || cmd.Action == "redeem" {
		k := cmd.Source + ":" + cmd.Action
		l := s.limits[k]
		if !s.now().Before(l.until) {
			l = limit{until: s.now().Add(time.Minute)}
		}
		l.count++
		s.limits[k] = l
		if l.count > 10 {
			return Result{}, &Error{Code: "rate_limited"}
		}
		if cmd.Action == "create" {
			secret := randomToken()
			c := code()
			for _, old := range s.sessions {
				if active(old.State) && old.CodeHash == digest(c) {
					return Result{}, &Error{Code: "retry_pairing"}
				}
			}
			v := domain.Session{ID: randomToken()[:32], HostHash: digest(secret), CodeHash: digest(c), State: "waiting", Expires: s.now().Add(5 * time.Minute), Epoch: 1}
			if s.save(ctx, v, "created") != nil {
				return Result{}, &Error{Code: "storage_error"}
			}
			out := s.view(v)
			out["credential"] = secret
			out["pairing_code"] = c
			return Result{Data: out, Created: true}, nil
		}
		if len(cmd.Code) != 10 || len(cmd.Name) > 80 {
			return Result{}, &Error{Code: "invalid_request"}
		}
		for _, v := range s.sessions {
			if v.CodeHash == digest(cmd.Code) && v.State == "waiting" && s.now().Before(v.Expires) {
				secret := randomToken()
				v.ControllerHash = digest(secret)
				v.Name = cmd.Name
				v.State = "pending"
				if s.save(ctx, v, "requested") != nil {
					return Result{}, &Error{Code: "storage_error"}
				}
				out := s.view(v)
				out["credential"] = secret
				return Result{Data: out, Created: true}, nil
			}
		}
		return Result{}, &Error{Code: "code_unavailable"}
	}
	v, ok := s.sessions[cmd.ID]
	if !ok {
		return Result{}, &Error{Code: "not_found"}
	}
	if cmd.Credential == "" {
		return Result{}, &Error{Code: "unauthorized"}
	}
	h := digest(cmd.Credential)
	role := ""
	if h == v.HostHash {
		role = "host"
	} else if v.ControllerHash != "" && h == v.ControllerHash {
		role = "controller"
	}
	if role == "" {
		return Result{}, &Error{Code: "forbidden"}
	}
	if expired(v, s.now()) {
		v.State = "expired"
		v.Cleanup = true
		if s.save(ctx, v, "expired") != nil {
			return Result{}, &Error{Code: "storage_error"}
		}
	}
	if cmd.Action == "get" {
		return Result{Data: s.view(v), Created: false}, nil
	}
	action := cmd.Action
	switch action {
	case "approve", "reject", "renew":
		if role != "host" {
			return Result{}, &Error{Code: "host_required"}
		}
		if action == "approve" && v.State == "approved" {
			return Result{Data: s.view(v), Created: false}, nil
		}
		if action == "reject" && v.State == "rejected" {
			return Result{Data: s.view(v), Created: false}, nil
		}
		if action == "renew" {
			if v.State != "approved" {
				return Result{}, &Error{Code: "grant_inactive"}
			}
			v.LeaseUntil = s.now().Add(Lease)
		} else {
			if v.State != "pending" {
				return Result{}, &Error{Code: "not_pending"}
			}
			if action == "approve" {
				v.State = "approved"
				v.LeaseUntil = s.now().Add(Lease)
			} else {
				v.State = "rejected"
				v.Cleanup = true
			}
		}
	case "revoke", "close":
		if active(v.State) {
			v.State = "revoked"
			v.Cleanup = true
		}
	case "connect":
		if v.State != "approved" {
			return Result{}, &Error{Code: "grant_inactive"}
		}
		out, err := s.media.Connect(ctx, v, role)
		if err != nil {
			return Result{}, &Error{Code: "media_unavailable"}
		}
		if !s.now().Before(v.LeaseUntil) {
			return Result{}, &Error{Code: "grant_expired"}
		}
		return Result{Data: out, Created: false}, nil
	default:
		return Result{}, &Error{Code: "not_found"}
	}
	if err := s.save(ctx, v, action); err != nil {
		return Result{}, &Error{Code: "storage_error"}
	}
	return Result{Data: s.view(v)}, nil
}
