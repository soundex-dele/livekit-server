package remote

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const Lease = 10 * time.Second

type Session struct {
	ID             string    `json:"id"`
	HostHash       string    `json:"host_hash"`
	ControllerHash string    `json:"controller_hash"`
	CodeHash       string    `json:"code_hash"`
	Name           string    `json:"name"`
	State          string    `json:"state"`
	Expires        time.Time `json:"expires"`
	LeaseUntil     time.Time `json:"lease_until"`
	Epoch          uint64    `json:"epoch"`
	Cleanup        bool      `json:"cleanup"`
}
type Media interface {
	Connect(context.Context, Session, string) (map[string]any, error)
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
	sessions map[string]Session
	limits   map[string]limit
	now      func() time.Time
}

func New(ctx context.Context, store Store, media Media) (*Service, error) {
	s := &Service{store: store, media: media, sessions: map[string]Session{}, limits: map[string]limit{}, now: time.Now}
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
func (s *Service) save(ctx context.Context, v Session, action string) error {
	if err := s.store.Save(ctx, v, action); err != nil {
		return err
	}
	s.sessions[v.ID] = v
	return nil
}
func expired(v Session, now time.Time) bool {
	return (v.State == "waiting" || v.State == "pending") && !now.Before(v.Expires) || v.State == "approved" && !now.Before(v.LeaseUntil)
}
func (s *Service) Sweep(ctx context.Context) {
	s.mu.Lock()
	var cleanup []Session
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
func reply(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
func fail(w http.ResponseWriter, status int, code string) {
	reply(w, status, map[string]any{"error": map[string]any{"code": code, "message": code, "retryable": status >= 500}, "request_id": randomToken()[:16]})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		fail(w, 400, "invalid_json")
		return false
	}
	if d.Decode(new(any)) != io.EOF {
		fail(w, 400, "invalid_json")
		return false
	}
	return true
}
func (s *Service) view(v Session) map[string]any {
	remaining := int64(0)
	if v.State == "approved" {
		remaining = v.LeaseUntil.Sub(s.now()).Milliseconds()
		if remaining < 0 {
			remaining = 0
		}
	}
	return map[string]any{"id": v.ID, "grant_id": v.ID, "state": v.State, "controller_name": v.Name, "epoch": v.Epoch, "expires_at": v.Expires.UTC().Format(time.RFC3339Nano), "lease_expires_at": v.LeaseUntil.UTC().Format(time.RFC3339Nano), "lease_remaining_ms": remaining, "host_identity": "host_" + v.ID, "controller_identity": "controller_" + v.ID, "scope": []string{"screen", "keyboard", "mouse"}}
}
func (s *Service) Handler() http.Handler { return http.HandlerFunc(s.serve) }
func (s *Service) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		reply(w, 200, map[string]bool{"ok": true})
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/v1/") {
		fail(w, 404, "not_found")
		return
	}
	// Same-origin browser API; no permissive CORS and no cookie authorization.
	if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host && origin != "https://"+r.Host {
		fail(w, 403, "origin_denied")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Method == "POST" && (r.URL.Path == "/api/v1/pairings" || r.URL.Path == "/api/v1/pairings/redeem") {
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		k := ip + ":" + r.URL.Path
		l := s.limits[k]
		if !s.now().Before(l.until) {
			l = limit{until: s.now().Add(time.Minute)}
		}
		l.count++
		s.limits[k] = l
		if l.count > 10 {
			fail(w, 429, "rate_limited")
			return
		}
		if r.URL.Path == "/api/v1/pairings" {
			secret := randomToken()
			c := code()
			for _, old := range s.sessions {
				if active(old.State) && old.CodeHash == digest(c) {
					fail(w, 503, "retry_pairing")
					return
				}
			}
			v := Session{ID: randomToken()[:32], HostHash: digest(secret), CodeHash: digest(c), State: "waiting", Expires: s.now().Add(5 * time.Minute), Epoch: 1}
			if s.save(r.Context(), v, "created") != nil {
				fail(w, 500, "storage_error")
				return
			}
			out := s.view(v)
			out["credential"] = secret
			out["pairing_code"] = c
			reply(w, 201, out)
			return
		}
		var in struct {
			Code string `json:"code"`
			Name string `json:"name"`
		}
		if !decode(w, r, &in) {
			return
		}
		if len(in.Code) != 10 || len(in.Name) > 80 {
			fail(w, 400, "invalid_request")
			return
		}
		for _, v := range s.sessions {
			if v.CodeHash == digest(in.Code) && v.State == "waiting" && s.now().Before(v.Expires) {
				secret := randomToken()
				v.ControllerHash = digest(secret)
				v.Name = in.Name
				v.State = "pending"
				if s.save(r.Context(), v, "requested") != nil {
					fail(w, 500, "storage_error")
					return
				}
				out := s.view(v)
				out["credential"] = secret
				reply(w, 201, out)
				return
			}
		}
		fail(w, 404, "code_unavailable")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/")
	if len(parts) < 2 || len(parts) > 3 || (parts[0] != "pairings" && parts[0] != "control-grants") {
		fail(w, 404, "not_found")
		return
	}
	v, ok := s.sessions[parts[1]]
	if !ok {
		fail(w, 404, "not_found")
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		fail(w, 401, "unauthorized")
		return
	}
	h := digest(strings.TrimPrefix(auth, "Bearer "))
	role := ""
	if h == v.HostHash {
		role = "host"
	} else if v.ControllerHash != "" && h == v.ControllerHash {
		role = "controller"
	}
	if role == "" {
		fail(w, 403, "forbidden")
		return
	}
	if expired(v, s.now()) {
		v.State = "expired"
		v.Cleanup = true
		if s.save(r.Context(), v, "expired") != nil {
			fail(w, 500, "storage_error")
			return
		}
	}
	if r.Method == "GET" && len(parts) == 2 {
		reply(w, 200, s.view(v))
		return
	}
	if r.Method != "POST" || len(parts) != 3 {
		fail(w, 405, "method_not_allowed")
		return
	}
	action := parts[2]
	switch action {
	case "approve", "reject", "renew":
		if role != "host" {
			fail(w, 403, "host_required")
			return
		}
		if action == "approve" && v.State == "approved" {
			reply(w, 200, s.view(v))
			return
		}
		if action == "reject" && v.State == "rejected" {
			reply(w, 200, s.view(v))
			return
		}
		if action == "renew" {
			if v.State != "approved" {
				fail(w, 409, "grant_inactive")
				return
			}
			v.LeaseUntil = s.now().Add(Lease)
		} else {
			if v.State != "pending" {
				fail(w, 409, "not_pending")
				return
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
			fail(w, 409, "grant_inactive")
			return
		}
		out, err := s.media.Connect(r.Context(), v, role)
		if err != nil {
			fail(w, 502, "media_unavailable")
			return
		}
		if !s.now().Before(v.LeaseUntil) {
			fail(w, 409, "grant_expired")
			return
		}
		reply(w, 200, out)
		return
	default:
		fail(w, 404, "not_found")
		return
	}
	if err := s.save(r.Context(), v, action); err != nil {
		fail(w, 500, "storage_error")
		return
	}
	reply(w, 200, s.view(v))
}
