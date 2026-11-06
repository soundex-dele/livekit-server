package bootstrap

import (
	"av-platform/business/internal/platform/control"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/coder/websocket"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"platform-foundation/server/persistence"
	foundation "platform-foundation/server/runtime"
	"strings"
	"testing"
	"time"
)

type httpFakeMedia struct{}

func (httpFakeMedia) Status(context.Context, control.Node) (control.NodeStatus, error) {
	return control.NodeStatus{ID: "test", Incarnation: "boot", Ready: true, AVMode: true}, nil
}
func (httpFakeMedia) Execute(context.Context, control.Node, control.MediaCommand) error { return nil }
func newHTTPApp(t *testing.T) (*AVApp, string, string) {
	t.Helper()
	f := foundation.DefaultConfig()
	f.Modules = []string{"organizations", "jobs", "events"}
	f.SQLite = filepath.Join(t.TempDir(), "av.db")
	c := AVConfig{Foundation: f, Memory: true, Businesses: []string{"screen-cast", "conference", "remote-control", "live-studio"}, Nodes: []control.Node{{ID: "test", APIURL: "http://127.0.0.1:1", PublicURL: "ws://127.0.0.1:2", RTCKey: "rtc", RTCSecret: strings.Repeat("r", 32), ManagementKey: "management", ManagementSecret: strings.Repeat("m", 32), AdmissionSecret: strings.Repeat("s", 32), Capacity: 10}}}
	if e := MigrateAV(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	a, e := NewAV(c, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { a.Close() })
	a.Platform.Media = httpFakeMedia{}
	if e = a.Platform.SyncNodes(context.Background()); e != nil {
		t.Fatal(e)
	}
	u, e := a.Foundation.Identity.Register(context.Background(), "owner", "a-long-test-password", false)
	if e != nil {
		t.Fatal(e)
	}
	actor, e := a.Foundation.Access.Resolve(context.Background(), u, "")
	if e != nil {
		t.Fatal(e)
	}
	org, e := a.Foundation.Access.Create(context.Background(), actor, "test", "")
	if e != nil {
		t.Fatal(e)
	}
	token, e := a.Foundation.Identity.Session(context.Background(), u.ID)
	if e != nil {
		t.Fatal(e)
	}
	return a, token, org
}
func avRequest(t *testing.T, h http.Handler, method, path, token, org, guest string, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.Header.Set("X-Foundation-Request", "1")
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if org != "" {
		r.Header.Set("X-Organization-ID", org)
	}
	if guest != "" {
		r.Header.Set("X-AV-Resource-Credential", guest)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
		t.Fatalf("non-json %d %s", w.Code, w.Body.String())
	}
	return w.Code, out
}
func runMedia(t *testing.T, a *AVApp) {
	t.Helper()
	for {
		j, e := a.Platform.Queue.Claim(context.Background(), "job", []string{"av.media"}, 30*time.Second)
		if errors.Is(e, persistence.ErrNotFound) {
			return
		}
		if e != nil {
			t.Fatal(e)
		}
		result, e := a.Platform.Execute(context.Background(), j, func(int) error { return nil })
		if e != nil {
			t.Fatal(e)
		}
		if e = a.Platform.Queue.Finish(context.Background(), j, result, nil); e != nil {
			t.Fatal(e)
		}
	}
}
func TestAVHTTPPairGrantRevokeAndIsolation(t *testing.T) {
	a, token, org := newHTTPApp(t)
	h := a.Handler()
	status, created := avRequest(t, h, "POST", "/api/v1/av/screen-cast/sessions", token, org, "", map[string]string{"title": "cast"})
	if status != 200 {
		t.Fatal(status, created)
	}
	id := created["id"].(string)
	status, paired := avRequest(t, h, "POST", "/api/v1/av/screen-cast/redeem", "", "", "", map[string]any{"code": created["pairing_code"]})
	if status != 200 {
		t.Fatal(status, paired)
	}
	guest := paired["credential"].(string)
	status, out := avRequest(t, h, "POST", "/api/v1/av/screen-cast/sessions/"+id+"/connect", "", "", guest, map[string]any{})
	if status != 200 {
		t.Fatal(status, out)
	}
	grant := out["grant_id"].(string)
	runMedia(t, a)
	status, out = avRequest(t, h, "POST", "/api/v1/av/rtc-grants/"+grant+"/credentials", "", "", guest, map[string]any{})
	if status != 200 || out["media_token"] == nil {
		t.Fatal(status, out)
	}
	status, out = avRequest(t, h, "POST", "/api/v1/av/rtc-grants/"+grant+"/credentials", token, org, "", map[string]any{})
	if status != 403 {
		t.Fatal("foreign participant obtained credentials", status, out)
	}
	status, out = avRequest(t, h, "GET", "/api/v1/av/screen-cast/sessions/"+id, token, "other-org", "", nil)
	if status != 403 {
		t.Fatal("cross org", status, out)
	}
	status, out = avRequest(t, h, "POST", "/api/v1/av/screen-cast/sessions/"+id+"/stop", token, org, "", map[string]any{})
	if status != 200 {
		t.Fatal(status, out)
	}
	status, out = avRequest(t, h, "POST", "/api/v1/av/rtc-grants/"+grant+"/credentials", "", "", guest, map[string]any{})
	if status != 403 {
		t.Fatal("ended cast issued credential", status, out)
	}
	// An API restart does not run the legacy service constructors or revoke
	// unrelated resources. The second instance shares durable state.
	b, e := NewAV(a.Config, a.Platform.Ephemeral)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	status, out = avRequest(t, b.Handler(), "GET", "/api/v1/av/screen-cast/sessions/"+id, token, org, "", nil)
	if status != 200 || out["state"] != "ended" {
		t.Fatal(status, out)
	}
}
func TestAVHTTPRejectsCrossOriginAndCredentialReplay(t *testing.T) {
	a, token, org := newHTTPApp(t)
	h := a.Handler()
	r := httptest.NewRequest("POST", "/api/v1/av/screen-cast/sessions", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Organization-ID", org)
	r.Header.Set("X-Foundation-Request", "1")
	r.Header.Set("Origin", "https://attacker.invalid")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	r.Header.Del("Origin")
	r.Header["Idempotency-Key"] = []string{""}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "idempotency_not_supported") {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestAVWebSocketOneUseAndEventReplay(t *testing.T) {
	a, token, org := newHTTPApp(t)
	h := a.Handler()
	_, created := avRequest(t, h, "POST", "/api/v1/av/screen-cast/sessions", token, org, "", map[string]any{})
	id := created["id"].(string)
	status, out := avRequest(t, h, "POST", "/api/v1/av/realtime/tickets", token, org, "", map[string]any{"resource_id": id})
	if status != 200 {
		t.Fatal(status, out)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/av/realtime/ws?ticket=" + out["ticket"].(string)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, e := websocket.Dial(ctx, wsURL, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer c.CloseNow()
	_, data, e := c.Read(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(data), "session.created") {
		t.Fatal(string(data))
	}
	duplicate, response, e := websocket.Dial(ctx, wsURL, nil)
	if duplicate != nil {
		duplicate.CloseNow()
	}
	if e == nil || response.StatusCode != 401 {
		t.Fatal("ticket replay accepted")
	}
}
func TestAVContractGeneration(t *testing.T) {
	raw, e := AVOpenAPI()
	if e != nil {
		t.Fatal(e)
	}
	var spec map[string]any
	if e = json.Unmarshal(raw, &spec); e != nil {
		t.Fatal(e)
	}
	paths := spec["paths"].(map[string]any)
	for _, path := range []string{"/api/v1/auth/login", "/api/v1/av/rtc-grants/{id}/credentials", "/api/v1/av/conference/meetings"} {
		if paths[path] == nil {
			t.Fatalf("missing %s", path)
		}
	}
}
