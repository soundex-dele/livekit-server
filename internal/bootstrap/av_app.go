package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	v2 "av-platform/business/internal/businesses/v2"
	"av-platform/business/internal/platform/control"
	"platform-foundation/server/access"
	"platform-foundation/server/persistence"
	foundation "platform-foundation/server/runtime"
	"platform-foundation/server/taskqueue"
)

var avPermissions = []string{"av.screen-cast.create", "av.conference.create", "av.remote-control.create", "av.live-studio.create", "av.manage"}

type AVApp struct {
	Foundation                      *foundation.App
	Platform                        *control.Platform
	Business                        *v2.Service
	Config                          AVConfig
	requests, failures, connections atomic.Int64
	ownsEphemeral                   bool
}

func MigrateAV(ctx context.Context, c AVConfig) error {
	s, e := foundation.OpenStore(c.Foundation)
	if e != nil {
		return e
	}
	defer s.DB.Close()
	return control.Migrate(ctx, s, v2.Schema)
}
func NewAV(c AVConfig, ephemeral control.Ephemeral) (*AVApp, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	ownsEphemeral := ephemeral == nil
	if ephemeral == nil {
		if c.Memory {
			ephemeral = control.NewMemory()
		} else {
			var e error
			ephemeral, e = control.OpenRedis(c.RedisURL)
			if e != nil {
				return nil, e
			}
		}
	}
	a := &AVApp{Config: c, ownsEphemeral: ownsEphemeral}
	f := c.Foundation
	f.Modules = append(append([]string{}, f.Modules...), "av")
	module := foundation.Module{Name: "av", Requires: []string{"organizations", "jobs", "events"}, Mount: func(app *foundation.App) error {
		if e := control.CheckSchema(context.Background(), app.Store, v2.Schema); e != nil {
			return e
		}
		a.Platform = control.New(app.Store, ephemeral, c.Nodes)
		a.Business = v2.NewWithModules(a.Platform, c.Businesses)
		for _, p := range avPermissions {
			if e := app.Access.RegisterPermission(p); e != nil {
				return e
			}
		}
		if e := app.RegisterJob("av.media", a.Platform.Execute); e != nil {
			return e
		}
		if e := app.RegisterEvent("av.notify", a.Platform.Notify); e != nil {
			return e
		}
		return app.RegisterEvent("foundation.changed", func(ctx context.Context, j taskqueue.Job, _ func(int) error) (json.RawMessage, error) {
			return json.RawMessage(`{"handled":true}`), nil
		})
	}, Run: func(ctx context.Context, _ *foundation.App) error {
		return a.Platform.RunMaintenance(ctx, a.Business.Sweep)
	}}
	app, e := foundation.NewWithModules(f, []foundation.Module{module})
	if e != nil {
		return nil, e
	}
	a.Foundation = app
	a.Platform.ValidateSubject = a.validateSubject
	a.Platform.AuthorizeGrant = func(ctx context.Context, scope, resource, subject string) error {
		r, e := a.Platform.GetResource(ctx, control.Principal{ID: subject, Scope: scope}, resource)
		if e != nil {
			return e
		}
		module := map[string]string{"cast": "screen-cast", "assist": "remote-control", "meeting": "conference", "broadcast": "live-studio"}[r.Kind]
		if !a.Config.Enabled(module) {
			return control.Fail(403, "module_disabled")
		}
		return a.Business.AuthorizeGrant(ctx, scope, resource, subject)
	}
	return a, nil
}
func (a *AVApp) Close() error {
	if c, ok := a.Platform.Ephemeral.(interface{ Close() error }); ok && a.ownsEphemeral {
		_ = c.Close()
	}
	return a.Foundation.Close()
}
func (a *AVApp) validateSubject(ctx context.Context, p control.Principal) error {
	if strings.HasPrefix(p.ID, "guest:") {
		var n int
		e := a.Platform.Store.DB.QueryRowContext(ctx, a.Platform.Store.Q("SELECT COUNT(*) FROM av_credentials WHERE scope=? AND resource_id=? AND subject_id=? AND revoked=0 AND expires>?"), p.Scope, p.ResourceID, p.ID, a.Platform.Now().UnixMilli()).Scan(&n)
		if e != nil {
			return e
		}
		if n == 0 {
			return control.Fail(401, "invalid_resource_credential")
		}
		return nil
	}
	u, e := a.Foundation.Identity.User(ctx, p.ID)
	if e != nil {
		return e
	}
	_, e = a.Foundation.Access.Resolve(ctx, u, strings.TrimPrefix(p.Scope, "org:"))
	return e
}
func (a *AVApp) Bootstrap(ctx context.Context, name, password, organization string) error {
	if name == "" || password == "" || organization == "" {
		return errors.New("set PF_ADMIN_NAME, PF_ADMIN_PASSWORD and AV_ORGANIZATION_NAME")
	}
	var count int
	if e := a.Foundation.Store.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM identity_users").Scan(&count); e != nil {
		return e
	}
	if count != 0 {
		return errors.New("bootstrap requires an empty account database")
	}
	u, e := a.Foundation.Identity.Register(ctx, name, password, true)
	if e != nil {
		return e
	}
	actor, e := a.Foundation.Access.Resolve(ctx, u, "")
	if e != nil {
		return e
	}
	id, e := a.Foundation.Access.Create(ctx, actor, organization, "")
	if e != nil {
		return e
	}
	actor, e = a.Foundation.Access.Resolve(ctx, u, id)
	if e != nil {
		return e
	}
	return a.roles(ctx, actor)
}
func (a *AVApp) roles(ctx context.Context, actor access.Actor) error {
	if actor.Organization == "" || !actor.Can("roles.manage") {
		return nil
	}
	for _, name := range []string{"av-member", "av-operator"} {
		_, e := a.Foundation.Store.Get(ctx, actor.Scope, "role", name)
		if e == nil {
			continue
		}
		if !errors.Is(e, persistence.ErrNotFound) {
			return e
		}
		permissions := append([]string{}, avPermissions[:4]...)
		if name == "av-operator" {
			permissions = append(permissions, "av.manage")
		}
		if e = a.Foundation.Access.SetRole(ctx, actor, name, access.Role{Name: name, Permissions: permissions}, ""); e != nil {
			return e
		}
	}
	return nil
}
func (a *AVApp) Serve(ctx context.Context, worker bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errorsCh := make(chan error, 2)
	workersDone := make(chan struct{})
	if worker {
		go func() { defer close(workersDone); errorsCh <- a.Foundation.Workers(ctx) }()
	} else {
		close(workersDone)
	}
	server := &http.Server{Addr: a.Config.Foundation.Address, Handler: a.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		var e error
		if a.Config.TLSCert != "" {
			e = server.ListenAndServeTLS(a.Config.TLSCert, a.Config.TLSKey)
		} else {
			e = server.ListenAndServe()
		}
		errorsCh <- e
	}()
	var result error
	select {
	case <-ctx.Done():
	case result = <-errorsCh:
	}
	cancel()
	stop, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	_ = server.Shutdown(stop)
	select {
	case <-workersDone:
	case <-stop.Done():
		return stop.Err()
	}
	if errors.Is(result, http.ErrServerClosed) || errors.Is(result, context.Canceled) {
		return nil
	}
	return result
}
func (a *AVApp) module(path string) string {
	name := strings.Split(strings.TrimPrefix(path, "/api/v1/av/"), "/")[0]
	if name == "control-grants" {
		return "remote-control"
	}
	return name
}
func (a *AVApp) requireModule(path string) error {
	m := a.module(path)
	switch m {
	case "screen-cast", "remote-control", "conference", "live-studio":
		if !a.Config.Enabled(m) {
			return control.Fail(404, "module_disabled")
		}
	}
	return nil
}
func (a *AVApp) ready(ctx context.Context) error {
	if e := a.Foundation.Store.DB.PingContext(ctx); e != nil {
		return fmt.Errorf("database unavailable")
	}
	if h, ok := a.Platform.Ephemeral.(control.Health); ok {
		return h.Ping(ctx)
	}
	return nil
}
