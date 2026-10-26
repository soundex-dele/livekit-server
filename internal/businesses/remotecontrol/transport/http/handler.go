package remotehttp

import (
	"av-platform/business/internal/businesses/remotecontrol/application"
	httpx "av-platform/business/internal/infrastructure/http"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
)

const Prefix = "/api/v1/remote-control/"

type Handler struct{ service *application.Service }

func New(service *application.Service) http.Handler { return &Handler{service: service} }
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, Prefix) {
		httpx.Fail(w, 404, "not_found")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host && origin != "https://"+r.Host {
		httpx.Fail(w, 403, "origin_denied")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, Prefix)
	cmd := application.Command{}
	cmd.Source, _, _ = net.SplitHostPort(r.RemoteAddr)
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		cmd.Credential = strings.TrimPrefix(auth, "Bearer ")
	}
	if path == "pairings" || path == "pairings/redeem" {
		if r.Method != "POST" {
			httpx.Fail(w, 405, "method_not_allowed")
			return
		}
		cmd.Action = "create"
		if path == "pairings/redeem" {
			cmd.Action = "redeem"
			var in struct {
				Code string `json:"code"`
				Name string `json:"name"`
			}
			r.Body = http.MaxBytesReader(w, r.Body, 4096)
			d := json.NewDecoder(r.Body)
			d.DisallowUnknownFields()
			if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF {
				httpx.Fail(w, 400, "invalid_json")
				return
			}
			cmd.Code, cmd.Name = in.Code, in.Name
		}
	} else {
		parts := strings.Split(path, "/")
		if len(parts) < 2 || len(parts) > 3 || (parts[0] != "pairings" && parts[0] != "control-grants") || parts[1] == "" {
			httpx.Fail(w, 404, "not_found")
			return
		}
		cmd.ID = parts[1]
		if r.Method == "GET" && len(parts) == 2 {
			cmd.Action = "get"
		} else if r.Method == "POST" && len(parts) == 3 {
			cmd.Action = parts[2]
			switch cmd.Action {
			case "approve", "reject", "renew", "revoke", "close", "connect":
			default:
				httpx.Fail(w, 404, "not_found")
				return
			}
		} else {
			httpx.Fail(w, 405, "method_not_allowed")
			return
		}
	}
	result, err := h.service.Execute(r.Context(), cmd)
	if err != nil {
		var e *application.Error
		if !errors.As(err, &e) {
			httpx.Fail(w, 500, "internal_error")
			return
		}
		status := map[string]int{"rate_limited": 429, "retry_pairing": 503, "storage_error": 500,
			"invalid_request": 400, "code_unavailable": 404, "not_found": 404, "unauthorized": 401,
			"forbidden": 403, "host_required": 403, "grant_inactive": 409, "not_pending": 409,
			"media_unavailable": 502, "grant_expired": 409}[e.Code]
		if status == 0 {
			status = 500
		}
		httpx.Fail(w, status, e.Code)
		return
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	httpx.Reply(w, status, result.Data)
}
