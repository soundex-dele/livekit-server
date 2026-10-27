package screencasthttp

import (
	"av-platform/business/internal/businesses/screencast/application"
	"av-platform/business/internal/businesses/screencast/domain"
	"av-platform/business/internal/businesses/screencast/infrastructure"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

type fakeMedia struct{}

func (fakeMedia) Connect(context.Context, domain.Session, string) (map[string]any, error) {
	return map[string]any{}, nil
}
func (fakeMedia) Delete(context.Context, string) error { return nil }
func TestHTTPContract(t *testing.T) {
	db, e := infrastructure.OpenSQLite(filepath.Join(t.TempDir(), "state.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	svc, e := application.New(context.Background(), db, fakeMedia{}, nil)
	if e != nil {
		t.Fatal(e)
	}
	h := New(svc)
	call := func(method, path, key, body, origin string) (int, map[string]any) {
		t.Helper()
		r := httptest.NewRequest(method, Prefix+path, strings.NewReader(body))
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("credential response cached")
		}
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	if status, _ := call("POST", "sessions", "", "{}", "https://evil.example"); status != 403 {
		t.Fatal(status)
	}
	status, a := call("POST", "sessions", "", "{}", "")
	if status != 201 {
		t.Fatal(status, a)
	}
	id := a["id"].(string)
	key := a["credential"].(string)
	if status, _ = call("POST", "redeem", "", `{"code":"1234567890","role":"receiver"}`, ""); status != 400 {
		t.Fatal("unknown fields accepted")
	}
	if status, _ = call("POST", "redeem", "", `{} {}`, ""); status != 400 {
		t.Fatal("trailing JSON accepted")
	}
	if status, _ = call("GET", "sessions/"+id, "", "", ""); status != 401 {
		t.Fatal("unauthorized")
	}
	if status, _ = call("POST", "sessions/"+id+"/connect", key, "{}", ""); status != 409 {
		t.Fatal("unpaired connect")
	}
	body, _ := json.Marshal(map[string]any{"code": a["code"]})
	status, b := call("POST", "redeem", "", string(body), "")
	if status != 200 {
		t.Fatal(status, b)
	}
	for _, credential := range []string{key, b["credential"].(string)} {
		if status, _ = call("POST", "sessions/"+id+"/connect", credential, "{}", ""); status != 200 {
			t.Fatal(status)
		}
	}
	for _, credential := range []string{key, b["credential"].(string)} {
		if status, _ = call("POST", "sessions/"+id+"/stop", credential, "{}", ""); status != 200 {
			t.Fatal(status)
		}
	}
	if status, _ = call("POST", "sessions/"+id+"/connect", key, "{}", ""); status != 409 {
		t.Fatal("ended connect")
	}
}
