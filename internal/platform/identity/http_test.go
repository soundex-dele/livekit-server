package identity

import (
	"av-platform/business/internal/infrastructure/database"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestSharedDesktopAuthLifecycle(t *testing.T) {
	db, e := database.OpenSQLite(filepath.Join(t.TempDir(), "auth.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	s, e := New(db)
	if e != nil {
		t.Fatal(e)
	}
	h := NewHandler(s)
	call := func(action, body, token string) (int, map[string]any) {
		r := httptest.NewRequest("POST", "/api/v1/auth/"+action, strings.NewReader(body))
		r.Header.Set("X-AV-Client", "desktop")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	status, out := call("register", `{"name":"alice","password":"long-test-passphrase"}`, "")
	if status != 200 {
		t.Fatal(status, out)
	}
	token, _ := out["session_token"].(string)
	if token == "" {
		t.Fatal("desktop token missing")
	}
	status, out = call("refresh", `{}`, token)
	if status != 200 {
		t.Fatal(status, out)
	}
	next := out["session_token"].(string)
	if _, e := s.Authenticate(t.Context(), token); e == nil {
		t.Fatal("old token survived refresh")
	}
	status, out = call("password", `{"old_password":"long-test-passphrase","password":"changed-passphrase-123"}`, next)
	if status != 200 {
		t.Fatal(status, out)
	}
	if _, e := s.Authenticate(t.Context(), next); e == nil {
		t.Fatal("session survived password change")
	}
	status, out = call("login", `{"name":"alice","password":"changed-passphrase-123"}`, "")
	if status != 200 {
		t.Fatal(status, out)
	}
	token = out["session_token"].(string)
	status, _ = call("logout", `{}`, token)
	if status != 200 {
		t.Fatal(status)
	}
	if _, e := s.Authenticate(t.Context(), token); e == nil {
		t.Fatal("logout did not revoke")
	}
}
