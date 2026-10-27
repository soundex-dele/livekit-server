package conference

import (
	httpx "av-platform/business/internal/infrastructure/http"
	"av-platform/business/internal/platform/identity"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Handler struct {
	S      *Service
	Auth   *identity.Service
	Secret string
	mu     sync.Mutex
	limits map[string]rate
}
type rate struct {
	at time.Time
	n  int
}

func NewHandler(s *Service, a *identity.Service, secret string) *Handler {
	return &Handler{S: s, Auth: a, Secret: secret, limits: map[string]rate{}}
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	respondError := func(e error) {
		status, code := 503, "service_unavailable"
		var f *Failure
		if errors.As(e, &f) {
			status, code = f.Status, f.Code
		}
		httpx.Fail(w, status, code)
	}
	if r.URL.Path == Prefix+"admission" {
		if r.Method != "POST" {
			httpx.Fail(w, 405, "method_not_allowed")
			return
		}
		if h.Secret == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+h.Secret)) != 1 {
			httpx.Fail(w, 401, "unauthorized")
			return
		}
		var c struct {
			Room     string `json:"room"`
			Identity string `json:"identity"`
			Grant    string `json:"grant"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&c) != nil || !h.S.Admitted(r.Context(), c.Room, c.Identity, c.Grant) {
			httpx.Fail(w, 403, "admission_denied")
			return
		}
		httpx.Reply(w, 200, map[string]bool{"allowed": true})
		return
	}
	if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host && o != "https://"+r.Host {
		httpx.Fail(w, 403, "origin_denied")
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	h.mu.Lock()
	now := time.Now()
	if len(h.limits) > 10000 {
		for k, v := range h.limits {
			if now.Sub(v.at) > time.Minute {
				delete(h.limits, k)
			}
		}
	}
	v := h.limits[ip]
	if now.Sub(v.at) > time.Minute {
		v = rate{at: now}
	}
	v.n++
	h.limits[ip] = v
	limited := v.n > 3600 || len(h.limits) > 10000
	h.mu.Unlock()
	if limited {
		httpx.Fail(w, 429, "rate_limited")
		return
	}
	a := Actor{Credential: r.Header.Get("X-AV-Guest")}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token != "" {
		u, e := h.Auth.Authenticate(r.Context(), token)
		if e != nil {
			httpx.Fail(w, 401, "unauthorized")
			return
		}
		a.User = u
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, Prefix), "/")
	if path == "guest" && r.Method == "POST" {
		httpx.Reply(w, 200, map[string]string{"guest_token": identity.ID()})
		return
	}
	var c Command
	if r.Method == "POST" {
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		d.DisallowUnknownFields()
		if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF {
			httpx.Fail(w, 400, "invalid_json")
			return
		}
	} else if r.Method != "GET" {
		httpx.Fail(w, 405, "method_not_allowed")
		return
	}
	var out any
	var e error
	key := r.Header.Get("Idempotency-Key")
	if len(key) > 128 {
		httpx.Fail(w, 400, "invalid_idempotency_key")
		return
	}
	switch {
	case path == "meetings" && r.Method == "GET":
		out, e = h.S.List(r.Context(), a)
	case path == "meetings" && r.Method == "POST":
		out, e = h.S.Create(r.Context(), a, c, key)
	case path == "join" && r.Method == "POST":
		out, e = h.S.Join(r.Context(), a, c, key)
	default:
		parts := strings.Split(path, "/")
		if len(parts) < 2 || parts[0] != "meetings" {
			httpx.Fail(w, 404, "not_found")
			return
		}
		id := parts[1]
		if len(parts) == 2 && r.Method == "GET" {
			out, e = h.S.Snapshot(r.Context(), id, a)
		} else if len(parts) == 3 && r.Method == "POST" {
			if parts[2] == "credentials" {
				out, e = h.S.Credentials(r.Context(), id, a)
			} else {
				out, e = h.S.Execute(r.Context(), id, parts[2], a, c, key)
			}
		} else {
			e = fail("not_found", 404)
		}
	}
	if e != nil {
		respondError(e)
		return
	}
	httpx.Reply(w, 200, out)
}
