package bootstrap

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBusinessRoutingAndProductAssets(t *testing.T) {
	web := t.TempDir()
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("remote-control-product"), 0600); err != nil {
		t.Fatal(err)
	}
	remoteCalls := 0
	h := Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteCalls++
		w.WriteHeader(http.StatusAccepted)
	}), web)
	for _, tc := range []struct {
		path   string
		status int
		body   string
	}{
		{"/api/v1/remote-control/pairings", 202, ""},
		{"/api/v1/screen-cast/sessions", 404, "not_found"},
		{"/api/v1/pairings", 404, "not_found"},
		{"/api/v1/control-grants/test", 404, "not_found"},
		{"/healthz", 200, "ok"},
		{"/apps/remote-control/", 200, "remote-control-product"},
		{"/apps/screen-cast/", 404, ""},
		{"/", 404, ""},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.body) {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		})
	}
	if remoteCalls != 1 {
		t.Fatalf("unrelated business was routed to remote control: %d", remoteCalls)
	}
}

func TestScreenCastRouting(t *testing.T) {
	web := t.TempDir()
	os.WriteFile(filepath.Join(web, "index.html"), []byte("screen-cast-product"), 0600)
	remoteCalls, castCalls := 0, 0
	h := Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { remoteCalls++; w.WriteHeader(202) }), "", Product{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { castCalls++; w.WriteHeader(201) }), web})
	for _, tc := range []struct {
		path   string
		status int
	}{{"/api/v1/remote-control/pairings", 202}, {"/api/v1/screen-cast/sessions", 201}, {"/api/v1/unknown/sessions", 404}, {"/apps/screen-cast/", 200}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		if w.Code != tc.status {
			t.Fatal(tc, w.Code)
		}
	}
	if remoteCalls != 1 || castCalls != 1 {
		t.Fatal("route isolation")
	}
}
