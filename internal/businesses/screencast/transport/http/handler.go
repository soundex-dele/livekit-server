package screencasthttp

import (
	"av-platform/business/internal/businesses/screencast/application"
	httpx "av-platform/business/internal/infrastructure/http"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
)

const Prefix = "/api/v1/screen-cast/"

type Handler struct{ service *application.Service }

func New(s *application.Service) http.Handler { return &Handler{s} }
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host && o != "https://"+r.Host {
		httpx.Fail(w, 403, "origin_denied")
		return
	}
	if !strings.HasPrefix(r.URL.Path, Prefix) {
		httpx.Fail(w, 404, "not_found")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, Prefix)
	c := application.Command{}
	c.Source, _, _ = net.SplitHostPort(r.RemoteAddr)
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		c.Credential = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	if path == "sessions" && r.Method == "POST" {
		c.Action = "create"
	} else if path == "redeem" && r.Method == "POST" {
		c.Action = "redeem"
	} else {
		p := strings.Split(path, "/")
		if len(p) < 2 || len(p) > 3 || p[0] != "sessions" || p[1] == "" {
			httpx.Fail(w, 404, "not_found")
			return
		}
		c.ID = p[1]
		if len(p) == 2 && r.Method == "GET" {
			c.Action = "get"
		} else if len(p) == 3 && r.Method == "POST" && (p[2] == "connect" || p[2] == "heartbeat" || p[2] == "stop") {
			c.Action = p[2]
		} else {
			httpx.Fail(w, 405, "method_not_allowed")
			return
		}
	}
	if c.Action == "redeem" || c.Action == "heartbeat" {
		var body struct {
			Code      string `json:"code"`
			Connected bool   `json:"connected"`
		}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		d.DisallowUnknownFields()
		if d.Decode(&body) != nil || d.Decode(new(any)) != io.EOF {
			httpx.Fail(w, 400, "invalid_json")
			return
		}
		c.Code = body.Code
		c.Connected = body.Connected
	}
	out, e := h.service.Execute(r.Context(), c)
	if e != nil {
		var app *application.Error
		if !errors.As(e, &app) {
			httpx.Fail(w, 500, "internal_error")
			return
		}
		status := map[string]int{"rate_limited": 429, "code_unavailable": 404, "not_found": 404, "unauthorized": 401, "session_inactive": 409, "media_unavailable": 502, "invalid_request": 400, "storage_error": 500}[app.Code]
		if status == 0 {
			status = 500
		}
		httpx.Fail(w, status, app.Code)
		return
	}
	status := 200
	if c.Action == "create" {
		status = 201
	}
	httpx.Reply(w, status, out)
}
