package identity

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type httpFailure struct {
	code   string
	status int
}

func (e *httpFailure) Error() string { return e.code }
func fail(c string, s int) error     { return &httpFailure{c, s} }

type bucket struct {
	start time.Time
	n     int
}
type Handler struct {
	Auth      *Service
	mu        sync.Mutex
	limits    map[string]bucket
	authSlots chan struct{}
}

func NewHandler(a *Service) *Handler {
	return &Handler{Auth: a, limits: make(map[string]bucket), authSlots: make(chan struct{}, 2)}
}
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func httpError(w http.ResponseWriter, err error) {
	status, code := 400, "invalid_account"
	if err.Error() == "account_unavailable" {
		code = "account_unavailable"
	}
	if errors.Is(err, ErrUnauthorized) {
		status, code = 401, "unauthorized"
	}
	var he *httpFailure
	if errors.As(err, &he) {
		status, code = he.status, he.code
	}
	reply(w, status, map[string]string{"error": code})
}
func body(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
		return fail("invalid_json", 400)
	}
	return nil
}
func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if c, e := r.Cookie("av_session"); e == nil {
		return c.Value
	}
	return ""
}
func secure(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	return r.TLS != nil || (host != "127.0.0.1" && host != "localhost" && host != "[::1]" && host != "::1")
}
func setSession(w http.ResponseWriter, r *http.Request, token string) {
	age := 86400
	if token == "" {
		age = -1
	}
	http.SetCookie(w, &http.Cookie{Name: "av_session", Value: token, Path: "/", MaxAge: age, HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteStrictMode})
}
func visitor(w http.ResponseWriter, r *http.Request) string {
	if c, e := r.Cookie("av_visitor"); e == nil && len(c.Value) == 48 {
		return c.Value
	}
	id := ID()
	http.SetCookie(w, &http.Cookie{Name: "av_visitor", Value: id, Path: "/", MaxAge: 86400, HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteStrictMode})
	return id
}
func (h *Handler) allow(key string, n int, period time.Duration) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	if len(h.limits) > 10000 {
		for k, v := range h.limits {
			if now.Sub(v.start) > time.Minute {
				delete(h.limits, k)
			}
		}
		if len(h.limits) > 10000 {
			return false
		}
	}
	b := h.limits[key]
	if now.Sub(b.start) >= period {
		b = bucket{start: now}
	}
	b.n++
	h.limits[key] = b
	return b.n <= n
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host && o != "https://"+r.Host {
		httpError(w, fail("origin_denied", 403))
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	h.authenticate(w, r, ip)
}
func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request, ip string) {
	action := strings.TrimPrefix(r.URL.Path, "/api/v1/auth/")
	token := bearer(r)
	u, _ := h.Auth.Authenticate(r.Context(), token)
	if action == "me" && r.Method == "GET" {
		if u.ID == "" {
			httpError(w, ErrUnauthorized)
		} else {
			reply(w, 200, u)
		}
		return
	}
	if r.Method != "POST" {
		httpError(w, fail("method_not_allowed", 405))
		return
	}
	var c struct {
		Name        string `json:"name"`
		Password    string `json:"password"`
		OldPassword string `json:"old_password"`
	}
	if e := body(w, r, &c); e != nil {
		httpError(w, e)
		return
	}
	if !h.allow("auth:"+ip, 20, time.Minute) {
		httpError(w, fail("rate_limited", 429))
		return
	}
	select {
	case h.authSlots <- struct{}{}:
		defer func() { <-h.authSlots }()
	default:
		httpError(w, fail("busy", 429))
		return
	}
	var e error
	var next string
	switch action {
	case "register":
		u, e = h.Auth.Register(r.Context(), c.Name, c.Password, false)
		if e == nil {
			next, e = h.Auth.Session(r.Context(), u.ID)
		}
	case "login":
		u, next, e = h.Auth.Login(r.Context(), c.Name, c.Password)
	case "logout":
		e = h.Auth.Logout(r.Context(), token)
	case "refresh":
		if u.ID == "" {
			e = ErrUnauthorized
		} else {
			next, e = h.Auth.Session(r.Context(), u.ID)
			if e == nil {
				e = h.Auth.Logout(r.Context(), token)
			}
		}
	case "password":
		if u.ID == "" {
			e = ErrUnauthorized
		} else {
			e = h.Auth.ChangePassword(r.Context(), u, c.OldPassword, c.Password)
		}
	default:
		e = fail("not_found", 404)
	}
	if e != nil {
		httpError(w, e)
		return
	}
	setSession(w, r, next)
	out := map[string]any{"user": u}
	if r.Header.Get("X-AV-Client") == "desktop" {
		out["session_token"] = next
	}
	reply(w, 200, out)
}
