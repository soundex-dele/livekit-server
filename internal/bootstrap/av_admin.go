package bootstrap

import (
	v2 "av-platform/business/internal/businesses/v2"
	"av-platform/business/internal/platform/control"
	"errors"
	"net/http"
	"platform-foundation/server/persistence"
	"strconv"
	"strings"
	"time"
)

func adminFilter(r *http.Request) (v2.AdminFilter, error) {
	q := r.URL.Query()
	f := v2.AdminFilter{Business: q.Get("business"), State: q.Get("state"), Owner: q.Get("owner"), Query: q.Get("q"), ResourceID: q.Get("resource_id"), Limit: 20}
	var e error
	if raw := q.Get("limit"); raw != "" {
		f.Limit, e = strconv.Atoi(raw)
		if e != nil {
			return f, control.Fail(400, "invalid_filter")
		}
	}
	if raw := q.Get("offset"); raw != "" {
		f.Offset, e = strconv.Atoi(raw)
		if e != nil {
			return f, control.Fail(400, "invalid_filter")
		}
	}
	for key, dst := range map[string]*int64{"from": &f.From, "to": &f.To} {
		if raw := q.Get(key); raw != "" {
			if n, x := strconv.ParseInt(raw, 10, 64); x == nil {
				*dst = n
			} else {
				when, x := time.Parse(time.RFC3339, raw)
				if x != nil {
					return f, control.Fail(400, "invalid_filter")
				}
				*dst = when.UnixMilli()
			}
		}
	}
	return f, nil
}

func (a *AVApp) admin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-AV-Resource-Credential") != "" {
		a.fail(w, r, control.Fail(403, "forbidden"))
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/av/admin/"), "/"), "/")
	platform := len(parts) > 0 && parts[0] == "platform"
	organization := ""
	var path []string
	if platform {
		path = parts[1:]
	} else if len(parts) >= 3 && parts[0] == "organizations" && parts[1] != "" {
		organization = parts[1]
		path = parts[2:]
	} else {
		a.fail(w, r, control.Fail(404, "not_found"))
		return
	}
	actor, e := a.Foundation.AuthenticateManagementRequest(r, organization)
	if e != nil {
		if errors.Is(e, persistence.ErrNotFound) {
			e = control.Fail(404, "organization_not_found")
		}
		a.fail(w, r, e)
		return
	}
	if platform && !actor.User.Admin || !platform && !actor.Can("av.manage") {
		a.fail(w, r, control.Fail(403, "forbidden"))
		return
	}
	p := control.FromActor(actor)
	var out any
	switch {
	case r.Method == "GET" && len(path) == 1 && path[0] == "overview":
		out, e = a.Business.AdminOverview(r.Context(), p, platform)
	case r.Method == "GET" && len(path) == 1 && path[0] == "resources":
		var f v2.AdminFilter
		f, e = adminFilter(r)
		if e == nil {
			out, e = a.Business.AdminResources(r.Context(), p, platform, f)
		}
	case r.Method == "GET" && len(path) == 1 && path[0] == "operations":
		var f v2.AdminFilter
		f, e = adminFilter(r)
		if e == nil {
			out, e = a.Business.AdminOperations(r.Context(), p, platform, f)
		}
	case r.Method == "GET" && platform && len(path) == 1 && path[0] == "media-nodes":
		var nodes []control.AdminNode
		nodes, e = a.Platform.AdminNodes(r.Context())
		out = map[string]any{"items": nodes}
	case r.Method == "GET" && !platform && len(path) == 2 && path[0] == "resources":
		out, e = a.Business.AdminDetail(r.Context(), p, path[1])
	case r.Method == "GET" && !platform && len(path) == 2 && path[0] == "operations":
		out, e = a.Business.AdminOperation(r.Context(), p, path[1])
	case r.Method == "POST" && !platform && len(path) == 3 && path[0] == "resources" && path[2] == "actions":
		var c v2.AdminAction
		e = bodyJSON(w, r, &c)
		if e == nil {
			out, e = a.Business.AdminAct(r.Context(), p, path[1], c, r.Header.Get("Idempotency-Key"))
		}
	default:
		e = control.Fail(404, "not_found")
	}
	if e != nil {
		a.fail(w, r, e)
		return
	}
	avReply(w, 200, out)
}
