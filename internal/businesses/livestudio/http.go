package livestudio

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"av-platform/business/internal/platform/identity"
	"github.com/coder/websocket"
)

const Prefix = "/api/v1/live-studio/"

type bucket struct {
	start time.Time
	n     int
}
type Handler struct {
	Service         *Service
	Auth            *identity.Service
	AdmissionSecret string
	mu              sync.Mutex
	limits          map[string]bucket
	authSlots       chan struct{}
}

func NewHandler(s *Service, a *identity.Service, secret string) *Handler {
	return &Handler{Service: s, Auth: a, AdmissionSecret: secret, limits: make(map[string]bucket), authSlots: make(chan struct{}, 2)}
}
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func httpError(w http.ResponseWriter, err error) {
	status, code := Status(err)
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
	id := identity.ID()
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
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == Prefix+"admission" {
		h.admission(w, r)
		return
	}
	if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host && o != "https://"+r.Host {
		httpError(w, fail("origin_denied", 403))
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if !h.allow("request:"+ip, 3000, time.Minute) {
		httpError(w, fail("rate_limited", 429))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/auth/") {
		h.auth(w, r, ip)
		return
	}
	token := bearer(r)
	u, e := h.Auth.Authenticate(r.Context(), token)
	if token != "" && e != nil {
		httpError(w, e)
		return
	}
	anon := visitor(w, r)
	path := strings.TrimPrefix(r.URL.Path, Prefix)
	var out any
	switch {
	case path == "rooms" && r.Method == "GET":
		out, e = h.Service.Rooms(r.Context())
	case path == "start" && r.Method == "POST":
		var c Command
		e = body(w, r, &c)
		if e == nil {
			out, e = h.Service.Start(r.Context(), u, c.Title)
		}
	case path == "wallet" && r.Method == "GET":
		out, e = h.Service.Wallet(r.Context(), u)
	case path == "leaderboard" && r.Method == "GET":
		out, e = h.Service.DailyBoard(r.Context())
	case strings.HasPrefix(path, "admin/"):
		h.admin(w, r, u, strings.TrimPrefix(path, "admin/"))
		return
	default:
		parts := strings.SplitN(path, "/", 3)
		if len(parts) != 3 || parts[0] != "broadcasts" {
			httpError(w, fail("not_found", 404))
			return
		}
		id, action := parts[1], parts[2]
		if action == "events" && r.Method == "GET" {
			h.events(w, r, u, anon, id)
			return
		}
		if action == "snapshot" && r.Method == "GET" {
			out, e = h.Service.Execute(r.Context(), u, anon, id, action, Command{})
		} else if r.Method == "POST" {
			c := Command{}
			e = body(w, r, &c)
			if e == nil {
				if (action == "chat" || action == "gift" || action == "guest/request") && !h.allow(action+":"+u.ID, 5, time.Second) {
					e = fail("rate_limited", 429)
				} else {
					out, e = h.Service.Execute(r.Context(), u, anon, id, action, c)
				}
			}
		} else {
			e = fail("method_not_allowed", 405)
		}
	}
	if e != nil {
		httpError(w, e)
		return
	}
	reply(w, 200, out)
}
func (h *Handler) auth(w http.ResponseWriter, r *http.Request, ip string) {
	action := strings.TrimPrefix(r.URL.Path, "/api/v1/auth/")
	token := bearer(r)
	u, _ := h.Auth.Authenticate(r.Context(), token)
	if action == "me" && r.Method == "GET" {
		if u.ID == "" {
			httpError(w, identity.ErrUnauthorized)
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
			e = identity.ErrUnauthorized
		} else {
			next, e = h.Auth.Session(r.Context(), u.ID)
			if e == nil {
				e = h.Auth.Logout(r.Context(), token)
			}
		}
	case "password":
		if u.ID == "" {
			e = identity.ErrUnauthorized
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
func (h *Handler) events(w http.ResponseWriter, r *http.Request, u identity.User, anon, id string) {
	if _, e := h.Service.Execute(r.Context(), u, anon, id, "presence", Command{}); e != nil {
		httpError(w, e)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if after < 0 {
		after = 0
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		events, e := h.Service.Events(r.Context(), id, after)
		if e != nil {
			httpError(w, e)
			return
		}
		reply(w, 200, events)
		return
	}
	conn, e := websocket.Accept(w, r, nil)
	if e != nil {
		return
	}
	defer conn.CloseNow()
	ctx := conn.CloseRead(r.Context())
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	lastCheck := time.Now()
	for {
		events, e := h.Service.Events(ctx, id, after)
		if e != nil {
			return
		}
		if len(events) > 0 {
			data, e := json.Marshal(events)
			if e != nil {
				return
			}
			write, stop := context.WithTimeout(ctx, 5*time.Second)
			e = conn.Write(write, websocket.MessageText, data)
			stop()
			if e != nil {
				return
			}
			after = events[len(events)-1].Seq
		}
		if time.Since(lastCheck) > 10*time.Second {
			if u.ID != "" {
				if _, e = h.Auth.Authenticate(ctx, bearer(r)); e != nil {
					_ = conn.Close(websocket.StatusPolicyViolation, "session expired")
					return
				}
			}
			var banned int
			e = h.Service.DB.QueryRowContext(ctx, "SELECT banned FROM live_presence WHERE broadcast=? AND identity=?", id, userIdentity(u, anon)).Scan(&banned)
			if e != nil || banned != 0 {
				_ = conn.Close(websocket.StatusPolicyViolation, "access revoked")
				return
			}
			lastCheck = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (h *Handler) admission(w http.ResponseWriter, r *http.Request) {
	supplied := r.Header.Get("Authorization")
	expected := "Bearer " + h.AdmissionSecret
	if h.AdmissionSecret == "" || subtle.ConstantTimeCompare([]byte(supplied), []byte(expected)) != 1 {
		httpError(w, identity.ErrUnauthorized)
		return
	}
	if r.Method != "POST" {
		httpError(w, fail("method_not_allowed", 405))
		return
	}
	var c struct {
		Grant    string `json:"grant"`
		Room     string `json:"room"`
		Identity string `json:"identity"`
	}
	if e := body(w, r, &c); e != nil {
		httpError(w, e)
		return
	}
	if !h.Service.Admit(r.Context(), c.Grant, c.Room, c.Identity) {
		httpError(w, fail("admission_denied", 403))
		return
	}
	reply(w, 200, map[string]bool{"allowed": true})
}
func (h *Handler) admin(w http.ResponseWriter, r *http.Request, u identity.User, action string) {
	if !u.Admin {
		httpError(w, fail("forbidden", 403))
		return
	}
	if action == "users" && r.Method == "GET" {
		rows, e := h.Service.DB.QueryContext(r.Context(), "SELECT id,name,admin,disabled FROM identity_users ORDER BY name LIMIT 500")
		if e != nil {
			httpError(w, e)
			return
		}
		defer rows.Close()
		users := []identity.User{}
		for rows.Next() {
			var v identity.User
			if e = rows.Scan(&v.ID, &v.Name, &v.Admin, &v.Disabled); e != nil {
				httpError(w, e)
				return
			}
			users = append(users, v)
		}
		if e = rows.Err(); e != nil {
			httpError(w, e)
			return
		}
		reply(w, 200, users)
		return
	}
	if r.Method != "POST" {
		httpError(w, fail("method_not_allowed", 405))
		return
	}
	var c struct {
		User     string `json:"user"`
		Password string `json:"password"`
		Disabled bool   `json:"disabled"`
	}
	if e := body(w, r, &c); e != nil {
		httpError(w, e)
		return
	}
	if c.User == u.ID {
		httpError(w, fail("cannot_change_self_here", 400))
		return
	}
	var e error
	switch action {
	case "disable":
		e = h.Auth.Disable(r.Context(), c.User, c.Disabled)
		if e == nil && c.Disabled {
			_, e = h.Service.transact(r.Context(), func(t *dbtx) (any, error) {
				t.exec("INSERT OR IGNORE INTO live_media_cleanup(media_room,identity) SELECT DISTINCT media_room,identity FROM live_grants WHERE user_id=?", c.User)
				t.exec("UPDATE live_grants SET revoked=1 WHERE user_id=?", c.User)
				t.exec("UPDATE live_guests SET state='ended' WHERE user_id=?", c.User)
				t.exec("UPDATE live_presence SET banned=1 WHERE user_id=?", c.User)
				var id string
				if err := t.QueryRow("SELECT id FROM live_broadcasts WHERE owner=? AND state!='ended'", c.User).Scan(&id); err == nil {
					b, err := broadcast(t, id)
					if err != nil {
						return nil, err
					}
					h.Service.end(t, b, r.Context())
				}
				return nil, nil
			})
		}
	case "reset-password":
		e = h.Auth.ResetPassword(r.Context(), c.User, c.Password)
	default:
		e = fail("not_found", 404)
	}
	if e != nil {
		httpError(w, e)
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}
