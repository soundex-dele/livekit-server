package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func adminRequest(t *testing.T, h http.Handler, path, token, body, key string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Foundation-Request", "1")
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
		t.Fatalf("bad JSON %d %s", w.Code, w.Body.String())
	}
	return w.Code, out
}

func TestAdminHTTPPlatformAndOrganizationAuthorization(t *testing.T) {
	a, ownerToken, org := newHTTPApp(t)
	ctx := context.Background()
	h := a.Handler()
	platform, e := a.Foundation.Identity.Register(ctx, "platform_admin", "a-long-platform-password", true)
	if e != nil {
		t.Fatal(e)
	}
	token, e := a.Foundation.Identity.Session(ctx, platform.ID)
	if e != nil {
		t.Fatal(e)
	}
	key, _, e := a.Foundation.Identity.IssueAPIKey(ctx, platform.ID, []string{"read", "write"}, time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	status, created := avRequest(t, h, "POST", "/api/v1/av/conference/meetings", ownerToken, org, "", map[string]string{"title": "Managed meeting", "password": "never-return-this"})
	if status != 200 {
		t.Fatal(status, created)
	}
	id := created["id"].(string)
	prefix := "/api/v1/av/admin/organizations/" + org
	status, out := avRequest(t, h, "GET", prefix+"/resources/"+id, token, "", "", nil)
	if status != 200 {
		t.Fatal(status, out)
	}
	raw, _ := json.Marshal(out)
	if bytes.Contains(raw, []byte("never-return-this")) || bytes.Contains(raw, []byte("password_hash")) {
		t.Fatal("secret leaked")
	}
	status, out = avRequest(t, h, "GET", "/api/v1/av/conference/meetings/"+id, token, org, "", nil)
	if status != 403 {
		t.Fatalf("ordinary API bypass %d %+v", status, out)
	}
	status, out = avRequest(t, h, "GET", prefix+"/resources", ownerToken, org, "", nil)
	if status != 200 || out["total"] != float64(1) {
		t.Fatal(status, out)
	}
	for _, suffix := range []string{"overview", "resources", "operations", "media-nodes"} {
		status, out = avRequest(t, h, "GET", "/api/v1/av/admin/platform/"+suffix, token, "", "", nil)
		if status != 200 {
			t.Fatal(suffix, status, out)
		}
		status, out = avRequest(t, h, "GET", "/api/v1/av/admin/platform/"+suffix, ownerToken, org, "", nil)
		if status != 403 {
			t.Fatalf("org reached platform %s %d", suffix, status)
		}
	}
	for _, credential := range []string{"", key} {
		status, out = avRequest(t, h, "GET", prefix+"/resources", credential, "", "", nil)
		if status != 401 {
			t.Fatalf("credential accepted %d %v", status, out)
		}
	}
	status, out = avRequest(t, h, "GET", prefix+"/resources", token, "", "fake-resource-credential", nil)
	if status != 403 {
		t.Fatalf("resource credential accepted %d", status)
	}
	other, e := a.Foundation.Identity.Register(ctx, "outsider", "another-long-password", false)
	if e != nil {
		t.Fatal(e)
	}
	otherToken, e := a.Foundation.Identity.Session(ctx, other.ID)
	if e != nil {
		t.Fatal(e)
	}
	status, out = avRequest(t, h, "GET", prefix+"/resources", otherToken, "", "", nil)
	if status != 403 {
		t.Fatalf("outsider accepted %d %+v", status, out)
	}
	status, out = avRequest(t, h, "GET", "/api/v1/av/admin/organizations/missing/resources", token, "", "", nil)
	if status != 404 {
		t.Fatalf("missing org %d %+v", status, out)
	}
	for _, query := range []string{"limit=101", "limit=-1", "offset=-2", "business=invalid", "from=bad", "from=200&to=100"} {
		status, out = avRequest(t, h, "GET", prefix+"/resources?"+query, token, "", "", nil)
		if status != 400 {
			t.Fatalf("bad filter %s %d %+v", query, status, out)
		}
	}
	body := `{"action":"end","reason":"platform moderation"}`
	status, out = adminRequest(t, h, prefix+"/resources/"+id+"/actions", token, body, "")
	if status != 400 {
		t.Fatalf("no key accepted %d", status)
	}
	status, out = adminRequest(t, h, prefix+"/resources/"+id+"/actions", token, body, "end-id")
	if status != 200 || out["state"] != "ended" {
		t.Fatal(status, out)
	}
	actionID := out["action_id"]
	status, out = adminRequest(t, h, prefix+"/resources/"+id+"/actions", token, body, "end-id")
	if status != 200 || out["action_id"] != actionID {
		t.Fatal("unstable repeated action", status, out)
	}
	status, out = avRequest(t, h, "GET", prefix+"/resources/"+id, token, "", "", nil)
	if status != 200 {
		t.Fatal(status, out)
	}
	actions := out["actions"].([]any)
	if len(actions) != 1 || actions[0].(map[string]any)["actor"] != platform.ID {
		t.Fatal("incorrect audit attribution", actions)
	}
	request := httptest.NewRequest("POST", prefix+"/resources/"+id+"/actions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, request)
	if w.Code != 403 {
		t.Fatal("CSRF bypass", w.Code)
	}
}

func TestAdminHTTPNodesNeverExposeCredentialsAndDetectStale(t *testing.T) {
	a, _, _ := newHTTPApp(t)
	ctx := context.Background()
	user, e := a.Foundation.Identity.Register(ctx, "operator", "another-long-password", true)
	if e != nil {
		t.Fatal(e)
	}
	token, e := a.Foundation.Identity.Session(ctx, user.ID)
	if e != nil {
		t.Fatal(e)
	}
	h := a.Handler()
	status, out := avRequest(t, h, "GET", "/api/v1/av/admin/platform/media-nodes", token, "", "", nil)
	if status != 200 {
		t.Fatal(status, out)
	}
	raw, _ := json.Marshal(out)
	for _, secret := range []string{a.Config.Nodes[0].APIURL, a.Config.Nodes[0].RTCSecret, a.Config.Nodes[0].ManagementSecret, a.Config.Nodes[0].AdmissionSecret, "rtc_secret", "management_secret", "admission_secret", "payload"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("node secret leaked")
		}
	}
	a.Platform.Now = func() time.Time { return time.Now().Add(time.Minute) }
	status, out = avRequest(t, h, "GET", "/api/v1/av/admin/platform/media-nodes", token, "", "", nil)
	if status != 200 || out["items"].([]any)[0].(map[string]any)["status"] != "stale" {
		t.Fatal(status, out)
	}
}

func TestAdminStaticMountAndOpenAPI(t *testing.T) {
	a, _, _ := newHTTPApp(t)
	a.Config.Businesses = nil
	a.Config.AdminWeb = t.TempDir()
	if e := os.WriteFile(filepath.Join(a.Config.AdminWeb, "index.html"), []byte("admin fixture"), 0600); e != nil {
		t.Fatal(e)
	}
	h := a.Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/apps/admin/", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "admin fixture") {
		t.Fatalf("static=%d %s", w.Code, w.Body.String())
	}
	status, caps := avRequest(t, h, "GET", "/api/v1/av/capabilities", "", "", "", nil)
	if status != 200 || caps["admin_url"] != "/apps/admin/" {
		t.Fatal(status, caps)
	}
	raw, e := AVOpenAPI()
	if e != nil {
		t.Fatal(e)
	}
	var doc map[string]any
	if e = json.Unmarshal(raw, &doc); e != nil {
		t.Fatal(e)
	}
	paths := doc["paths"].(map[string]any)
	count := 0
	for path, item := range paths {
		if !strings.HasPrefix(path, "/api/v1/av/admin/") {
			continue
		}
		count++
		bytes, _ := json.Marshal(item)
		if strings.Contains(string(bytes), "avResource") {
			t.Fatalf("guest security on %s", path)
		}
	}
	if count != 10 {
		t.Fatalf("admin routes=%d", count)
	}
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	if schemas["AVAdminAction"] == nil || schemas["AVAdminDetail"] == nil {
		t.Fatal("missing admin contracts")
	}
}
