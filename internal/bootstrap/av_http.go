package bootstrap

import (
	v2 "av-platform/business/internal/businesses/v2"
	"av-platform/business/internal/platform/control"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/coder/websocket"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"io"
	"log/slog"
	"net"
	"net/http"
	"platform-foundation/server/access"
	"platform-foundation/server/httpx"
	"platform-foundation/server/identity"
	"strconv"
	"strings"
	"time"
)

func avReply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func (a *AVApp) fail(w http.ResponseWriter, r *http.Request, e error) {
	a.failures.Add(1)
	status, code, retry := 500, "internal_error", true
	var public *httpx.HTTPError
	if errors.As(e, &public) {
		status, code, retry = public.Status, public.Code, public.Retryable
	} else if errors.Is(e, identity.ErrUnauthorized) {
		status, code, retry = 401, "unauthorized", false
	} else if errors.Is(e, access.ErrForbidden) {
		status, code, retry = 403, "forbidden", false
	}
	avReply(w, status, map[string]any{"error": map[string]any{"code": code, "message": code, "retryable": retry}, "request_id": r.Header.Get("X-Request-ID")})
}
func bodyJSON(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
		return control.Fail(400, "invalid_json")
	}
	return nil
}
func (a *AVApp) principal(r *http.Request, optional bool) (control.Principal, error) {
	if token := r.Header.Get("X-AV-Resource-Credential"); token != "" {
		p, e := a.Platform.Guest(r.Context(), token)
		if e != nil {
			return p, e
		}
		if org := r.Header.Get("X-Organization-ID"); org != "" && "org:"+org != p.Scope {
			return p, control.Fail(403, "resource_forbidden")
		}
		return p, nil
	}
	_, cookieErr := r.Cookie("pf_session")
	if optional && r.Header.Get("Authorization") == "" && cookieErr != nil {
		return control.Principal{}, nil
	}
	actor, e := a.Foundation.AuthenticateRequest(r)
	if e != nil {
		return control.Principal{}, e
	}
	return control.FromActor(actor), nil
}
func (a *AVApp) freshPrincipal(ctx context.Context, p control.Principal) (control.Principal, error) {
	if e := a.validateSubject(ctx, p); e != nil {
		return p, e
	}
	if strings.HasPrefix(p.ID, "guest:") {
		p.Kind = "guest"
		return p, nil
	}
	u, e := a.Foundation.Identity.User(ctx, p.ID)
	if e != nil {
		return p, e
	}
	actor, e := a.Foundation.Access.Resolve(ctx, u, strings.TrimPrefix(p.Scope, "org:"))
	if e != nil {
		return p, e
	}
	return control.FromActor(actor), nil
}
func (a *AVApp) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		raw, e := AVOpenAPI()
		if e != nil {
			a.fail(w, r, e)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if e := a.ready(ctx); e != nil {
			a.fail(w, r, control.Fail(503, "not_ready"))
			return
		}
		avReply(w, 200, map[string]bool{"ready": true})
	})
	mux.HandleFunc("GET /api/v1/av/capabilities", func(w http.ResponseWriter, r *http.Request) {
		adminURL := ""
		if a.Config.AdminWeb != "" {
			adminURL = "/apps/admin/"
		}
		avReply(w, 200, map[string]any{"modules": a.Config.Businesses, "organization_required": true, "api_version": "1", "media_protocol": "av_grant", "admin_url": adminURL})
	})
	mux.HandleFunc("POST /api/v1/av/organizations/initialize", func(w http.ResponseWriter, r *http.Request) {
		actor, e := a.Foundation.AuthenticateRequest(r)
		if e == nil && (!actor.Can("roles.manage") || actor.Organization == "") {
			e = access.ErrForbidden
		}
		if e == nil {
			e = a.roles(r.Context(), actor)
		}
		if e != nil {
			a.fail(w, r, e)
			return
		}
		avReply(w, 200, map[string]any{"initialized": true, "default_member_role": "av-member"})
	})
	mux.HandleFunc("POST /api/v1/av/rtc-grants/{id}/credentials", a.credentials)
	mux.HandleFunc("GET /api/v1/av/operations", func(w http.ResponseWriter, r *http.Request) {
		p, e := a.principal(r, false)
		var out []map[string]any
		if e == nil {
			out, e = a.Platform.Operations(r.Context(), p, r.URL.Query().Get("resource_id"))
		}
		if e != nil {
			a.fail(w, r, e)
			return
		}
		avReply(w, 200, map[string]any{"items": out})
	})
	mux.HandleFunc("GET /api/v1/av/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, e := a.principal(r, false)
		var out any
		if e == nil {
			out, e = a.Platform.Operation(r.Context(), p, r.PathValue("id"))
		}
		if e != nil {
			a.fail(w, r, e)
			return
		}
		avReply(w, 200, out)
	})
	mux.HandleFunc("POST /api/v1/av/realtime/tickets", a.ticket)
	mux.HandleFunc("GET /api/v1/av/realtime/ws", a.socket)
	mux.HandleFunc("GET /api/v1/av/events", a.events)
	mux.HandleFunc("GET /api/v1/av/metrics", func(w http.ResponseWriter, r *http.Request) {
		p, e := a.principal(r, false)
		if e != nil || !p.Can("av.manage") {
			a.fail(w, r, control.Fail(403, "forbidden"))
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "av_http_requests_total %d\nav_http_failures_total %d\nav_websocket_connections %d\n", a.requests.Load(), a.failures.Load(), a.connections.Load())
	})
	mux.HandleFunc("POST /internal/v1/media/admissions/check", a.admission)
	mux.HandleFunc("/api/v1/av/admin/", a.admin)
	mux.HandleFunc("/api/v1/av/", a.business)
	if a.Config.AdminWeb != "" {
		mux.Handle("/apps/admin/", http.StripPrefix("/apps/admin/", http.FileServer(http.Dir(a.Config.AdminWeb))))
	}
	for name, root := range a.Config.Web {
		if root != "" && a.Config.Enabled(name) {
			prefix := "/apps/" + name + "/"
			mux.Handle(prefix, http.StripPrefix(prefix, http.FileServer(http.Dir(root))))
		}
	}
	mux.Handle("/", a.Foundation.Handler())
	return otelhttp.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/realtime/ws") {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			r = r.WithContext(ctx)
		}
		start := time.Now()
		a.requests.Add(1)
		id := control.ID()
		r.Header.Set("X-Request-ID", id)
		w.Header().Set("X-Request-ID", id)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		defer func() {
			if v := recover(); v != nil {
				slog.Error("AV request panic", "request_id", id)
				a.fail(w, r, control.Fail(500, "internal_error"))
			}
			slog.Info("AV request", "method", r.Method, "path", r.URL.Path, "request_id", id, "duration_ms", time.Since(start).Milliseconds())
		}()
		internal := strings.HasPrefix(r.URL.Path, "/internal/")
		if !internal && (r.Method != "GET" && r.Method != "HEAD" || strings.HasSuffix(r.URL.Path, "/ws")) {
			expected := a.Config.Foundation.PublicOrigin
			if expected == "" {
				scheme := "http"
				if r.TLS != nil {
					scheme = "https"
				}
				expected = scheme + "://" + r.Host
			}
			origin := r.Header.Get("Origin")
			if origin != "" && origin != expected || r.Header.Get("Sec-Fetch-Site") == "cross-site" || (r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("X-Foundation-Request") != "1") {
				a.fail(w, r, control.Fail(403, "origin_denied"))
				return
			}
		}
		if !internal && strings.HasPrefix(r.URL.Path, "/api/") {
			ip, _, e := net.SplitHostPort(r.RemoteAddr)
			if e != nil {
				ip = r.RemoteAddr
			}
			limit := 1200
			bucket := "api:" + ip
			if strings.HasPrefix(r.URL.Path, "/api/v1/auth/") {
				limit = 20
				bucket = "auth:" + ip
			} else if v2.Public(r.Method, strings.TrimPrefix(r.URL.Path, "/api/v1/av/")) && r.Method == "POST" {
				limit = 30
				bucket = "redeem:" + ip
			}
			ok, e := a.Platform.Ephemeral.Allow(r.Context(), bucket, limit, time.Minute)
			if e != nil {
				a.fail(w, r, control.Fail(503, "realtime_unavailable"))
				return
			}
			if !ok {
				a.fail(w, r, control.Fail(429, "rate_limited"))
				return
			}
		}
		mux.ServeHTTP(w, r)
	}), "av.http")
}
func (a *AVApp) business(w http.ResponseWriter, r *http.Request) {
	if e := a.requireModule(r.URL.Path); e != nil {
		a.fail(w, r, e)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/av/")
	if v2.Sensitive(r.Method, path) {
		if _, ok := r.Header["Idempotency-Key"]; ok {
			a.fail(w, r, control.Fail(400, "idempotency_not_supported"))
			return
		}
	}
	p, e := a.principal(r, v2.Public(r.Method, path))
	if e != nil {
		a.fail(w, r, e)
		return
	}
	var raw json.RawMessage
	if r.Method != "GET" && r.Method != "HEAD" {
		if e = bodyJSON(w, r, &raw); e != nil {
			a.fail(w, r, e)
			return
		}
	}
	out, e := a.Business.Handle(r.Context(), p, r.Method, path, raw, r.Header.Get("Idempotency-Key"))
	if e != nil {
		a.fail(w, r, e)
		return
	}
	avReply(w, 200, out)
}
func (a *AVApp) credentials(w http.ResponseWriter, r *http.Request) {
	if _, ok := r.Header["Idempotency-Key"]; ok {
		a.fail(w, r, control.Fail(400, "idempotency_not_supported"))
		return
	}
	p, e := a.principal(r, false)
	var out any
	if e == nil {
		out, e = a.Platform.Credentials(r.Context(), p, r.PathValue("id"))
	}
	if e != nil {
		a.fail(w, r, e)
		return
	}
	avReply(w, 200, out)
}
func (a *AVApp) admission(w http.ResponseWriter, r *http.Request) {
	var q control.AdmissionRequest
	if e := bodyJSON(w, r, &q); e != nil {
		a.fail(w, r, e)
		return
	}
	var secret string
	for _, n := range a.Config.Nodes {
		if n.ID == q.NodeID {
			secret = n.AdmissionSecret
			break
		}
	}
	if secret == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+secret)) != 1 {
		a.fail(w, r, control.Fail(401, "unauthorized"))
		return
	}
	out, e := a.Platform.Admit(r.Context(), q)
	if e != nil {
		a.fail(w, r, e)
		return
	}
	avReply(w, 200, out)
}

type socketTicket struct {
	Principal control.Principal `json:"principal"`
	Resource  string            `json:"resource_id"`
}

func (a *AVApp) ticket(w http.ResponseWriter, r *http.Request) {
	if _, ok := r.Header["Idempotency-Key"]; ok {
		a.fail(w, r, control.Fail(400, "idempotency_not_supported"))
		return
	}
	p, e := a.principal(r, false)
	if e != nil {
		a.fail(w, r, e)
		return
	}
	var q struct {
		Resource string `json:"resource_id"`
	}
	if e = bodyJSON(w, r, &q); e != nil {
		a.fail(w, r, e)
		return
	}
	if e = a.Platform.CanObserve(r.Context(), p, q.Resource); e != nil {
		a.fail(w, r, e)
		return
	}
	raw, _ := json.Marshal(socketTicket{p, q.Resource})
	token := control.ID()
	if e = a.Platform.Ephemeral.Set(r.Context(), "ws:"+control.Hash(token), string(raw), 30*time.Second); e != nil {
		a.fail(w, r, control.Fail(503, "realtime_unavailable"))
		return
	}
	avReply(w, 200, map[string]any{"ticket": token, "expires_in": 30})
}
func (a *AVApp) events(w http.ResponseWriter, r *http.Request) {
	p, e := a.principal(r, false)
	if e != nil {
		a.fail(w, r, e)
		return
	}
	after, e := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if r.URL.Query().Get("after") == "" {
		e = nil
	}
	if e != nil || after < 0 {
		a.fail(w, r, control.Fail(400, "invalid_cursor"))
		return
	}
	out, e := a.Platform.Events(r.Context(), p, r.URL.Query().Get("resource_id"), after)
	if e != nil {
		a.fail(w, r, e)
		return
	}
	last := after
	if len(out) > 0 {
		last = out[len(out)-1].Sequence
	}
	avReply(w, 200, map[string]any{"items": out, "limit": 200, "last_sequence": last})
}
func (a *AVApp) socket(w http.ResponseWriter, r *http.Request) {
	raw, e := a.Platform.Ephemeral.Take(r.Context(), "ws:"+control.Hash(r.URL.Query().Get("ticket")))
	if e != nil {
		a.fail(w, r, control.Fail(401, "invalid_ticket"))
		return
	}
	var ticket socketTicket
	if json.Unmarshal([]byte(raw), &ticket) != nil {
		a.fail(w, r, control.Fail(401, "invalid_ticket"))
		return
	}
	p, e := a.freshPrincipal(r.Context(), ticket.Principal)
	if e == nil {
		e = a.Platform.CanObserve(r.Context(), p, ticket.Resource)
	}
	if e != nil {
		a.fail(w, r, e)
		return
	}
	c, e := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if e != nil {
		return
	}
	a.connections.Add(1)
	defer a.connections.Add(-1)
	defer c.Close(websocket.StatusNormalClosure, "reauthentication_required")
	c.SetReadLimit(1024)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	ctx = c.CloseRead(ctx)
	var hints <-chan string
	if sub, ok := a.Platform.Ephemeral.(control.Subscription); ok {
		hints, _ = sub.Subscribe(ctx, "events:"+p.Scope+":"+ticket.Resource)
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if after < 0 {
		after = 0
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		p, e = a.freshPrincipal(ctx, ticket.Principal)
		if e != nil {
			return
		}
		events, e := a.Platform.Events(ctx, p, ticket.Resource, after)
		if e != nil {
			return
		}
		for _, event := range events {
			payload, _ := json.Marshal(event)
			writeCtx, done := context.WithTimeout(ctx, 3*time.Second)
			e = c.Write(writeCtx, websocket.MessageText, payload)
			done()
			if e != nil {
				return
			}
			after = event.Sequence
		}
		if len(events) == 200 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case _, ok := <-hints:
			if !ok {
				hints = nil
			}
		}
	}
}
