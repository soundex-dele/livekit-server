package infrastructure

import (
	remote "av-platform/business/internal/businesses/remotecontrol/infrastructure"
	"av-platform/business/internal/businesses/screencast/domain"
	"av-platform/business/internal/platform/media"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationAndRoles(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	r, e := remote.OpenSQLite(path)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	s, e := OpenSQLite(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Save(ctx, domain.Session{ID: "test", State: "waiting"}); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = OpenSQLite(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	all, e := s.Load(ctx)
	if e != nil || len(all) != 1 {
		t.Fatal(e, all)
	}
	old, e := r.Load(ctx)
	if e != nil || len(old) != 0 {
		t.Fatal("business tables overlap")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		if b["maxParticipants"] != float64(2) {
			t.Error("not a two-person room")
		}
		w.Write([]byte(`{}`))
	}))
	defer server.Close()
	m := &Media{Client: &media.LiveKit{URL: server.URL, PublicURL: "ws://media", Key: "test", Secret: "secret", Client: server.Client()}}
	for _, role := range []string{"receiver", "sender"} {
		v, e := m.Connect(ctx, domain.Session{ID: "one", Room: "screencast-one"}, role)
		if e != nil {
			t.Fatal(e)
		}
		raw, _ := base64.RawURLEncoding.DecodeString(strings.Split(v["media_token"].(string), ".")[1])
		var claims map[string]any
		json.Unmarshal(raw, &claims)
		g := claims["video"].(map[string]any)
		if g["canPublish"] != (role == "sender") || g["canSubscribe"] != (role == "receiver") || g["canPublishData"] != false || g["room"] != "screencast-one" {
			t.Fatal(g)
		}
	}
}
