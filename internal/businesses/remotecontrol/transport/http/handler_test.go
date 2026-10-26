package remotehttp

import (
	"av-platform/business/internal/businesses/remotecontrol/application"
	"av-platform/business/internal/businesses/remotecontrol/domain"
	infra "av-platform/business/internal/businesses/remotecontrol/infrastructure"
	"av-platform/business/internal/platform/media"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeMedia struct {
	mu      sync.Mutex
	deleted []string
}

func (f *fakeMedia) Connect(_ context.Context, s domain.Session, role string) (map[string]any, error) {
	return map[string]any{"participant_identity": role + "_" + s.ID}, nil
}
func (f *fakeMedia) Delete(_ context.Context, room string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, room)
	return nil
}

type client struct {
	t     *testing.T
	s     *application.Service
	now   *time.Time
	media *fakeMedia
}

func (c client) call(method, path, token string, body any, want int) map[string]any {
	c.t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, Prefix+path, bytes.NewReader(raw))
	r.RemoteAddr = "127.0.0.1:12345"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	New(c.s).ServeHTTP(w, r)
	if w.Code != want {
		c.t.Fatalf("%s %s: status %d want %d: %s", method, path, w.Code, want, w.Body.String())
	}
	var v map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		c.t.Fatal(err)
	}
	return v
}
func setup(t *testing.T) (client, *infra.SQLite) {
	t.Helper()
	store, err := infra.OpenSQLite(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := time.Now()
	fake := &fakeMedia{}
	s, err := application.New(context.Background(), store, fake, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return client{t: t, s: s, now: &now, media: fake}, store
}
func pair(c client) (map[string]any, map[string]any) {
	h := c.call("POST", "pairings", "", nil, 201)
	v := c.call("POST", "pairings/redeem", "", map[string]string{"code": h["pairing_code"].(string), "name": "helper"}, 201)
	return h, v
}
func TestAuthorizationAndLifecycle(t *testing.T) {
	c, _ := setup(t)
	h, v := pair(c)
	id := h["id"].(string)
	host := h["credential"].(string)
	controller := v["credential"].(string)
	route := "control-grants/" + id
	c.call("POST", route+"/connect", controller, nil, 409)
	c.call("POST", route+"/approve", controller, nil, 403)
	c.call("GET", route, "wrong", nil, 403)
	c.call("POST", "pairings/redeem", "", map[string]string{"code": h["pairing_code"].(string)}, 404)
	approved := c.call("POST", route+"/approve", host, nil, 200)
	if approved["state"] != "approved" {
		t.Fatal(approved)
	}
	again := c.call("POST", route+"/approve", host, nil, 200)
	if again["lease_expires_at"] != approved["lease_expires_at"] {
		t.Fatal("duplicate approval extended lease")
	}
	c.call("POST", route+"/renew", controller, nil, 403)
	c.call("POST", route+"/connect", controller, nil, 200)
	c.call("POST", route+"/revoke", controller, nil, 200)
	c.call("POST", route+"/revoke", controller, nil, 200)
	c.call("POST", route+"/renew", host, nil, 409)
	c.call("POST", route+"/connect", host, nil, 409)
	c.s.Sweep(context.Background())
	if len(c.media.deleted) != 1 {
		t.Fatal("cleanup missing")
	}
}
func TestExpiryRestartAndIsolation(t *testing.T) {
	c, store := setup(t)
	now := time.Now()
	*c.now = now
	h, v := pair(c)
	route := "control-grants/" + h["id"].(string)
	host := h["credential"].(string)
	other, _ := pair(c)
	c.call("GET", route, other["credential"].(string), nil, 403)
	c.call("POST", route+"/approve", host, nil, 200)
	now = now.Add(11 * time.Second)
	*c.now = now
	c.call("POST", route+"/renew", host, nil, 409)
	state := c.call("GET", route, v["credential"].(string), nil, 200)
	if state["state"] != "expired" {
		t.Fatal(state)
	}
	restarted, err := application.New(context.Background(), store, &fakeMedia{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.s = restarted
	state = c.call("GET", "control-grants/"+other["id"].(string), other["credential"].(string), nil, 200)
	if state["state"] != "revoked" {
		t.Fatal("restart did not revoke")
	}
}
func TestPairingExpiryRateLimitAndSecrets(t *testing.T) {
	c, store := setup(t)
	now := time.Now()
	*c.now = now
	h := c.call("POST", "pairings", "", nil, 201)
	now = now.Add(6 * time.Minute)
	*c.now = now
	c.call("POST", "pairings/redeem", "", map[string]string{"code": h["pairing_code"].(string)}, 404)
	for i := 0; i < 9; i++ {
		c.call("POST", "pairings/redeem", "", map[string]string{"code": "0000000000"}, 404)
	}
	c.call("POST", "pairings/redeem", "", map[string]string{"code": "0000000000"}, 429)
	items, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(data)
	if bytes.Contains([]byte(raw), []byte(h["credential"].(string))) || bytes.Contains([]byte(raw), []byte(h["pairing_code"].(string))) {
		t.Fatal("plaintext secret persisted")
	}
}
func TestConcurrentRedeem(t *testing.T) {
	c, _ := setup(t)
	h := c.call("POST", "pairings", "", nil, 201)
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, _ := json.Marshal(map[string]string{"code": h["pairing_code"].(string)})
			req := httptest.NewRequest("POST", Prefix+"pairings/redeem", bytes.NewReader(raw))
			rec := httptest.NewRecorder()
			New(c.s).ServeHTTP(rec, req)
			statuses <- rec.Code
		}()
	}
	wg.Wait()
	close(statuses)
	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[201] != 1 || counts[404] != 1 {
		t.Fatal(counts)
	}
}
func TestMediaTokenAndManagement(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		methods = append(methods, r.URL.Path)
		if r.URL.Path == "/twirp/livekit.RoomService/CreateRoom" {
			if _, ok := body["room"]; ok {
				t.Error("invalid create field")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer server.Close()
	m := &infra.Media{Client: &media.LiveKit{URL: server.URL, PublicURL: "ws://test", Key: "key", Secret: "secret", Client: server.Client()}}
	v, err := m.Connect(context.Background(), domain.Session{ID: "test"}, "controller")
	if err != nil {
		t.Fatal(err)
	}
	if v["participant_identity"] != "controller_test" {
		t.Fatal(v)
	}
	parts := strings.Split(v["media_token"].(string), ".")
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err = json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	video := claims["video"].(map[string]any)
	if video["canPublish"] != false || video["canSubscribe"] != true || video["canPublishData"] != true || video["room"] != "rc_test" || claims["sub"] != "controller_test" {
		t.Fatal(claims)
	}
	if _, ok := video["roomAdmin"]; ok {
		t.Fatal("unexpected management permission")
	}
	if err = m.Delete(context.Background(), "rc_test"); err != nil {
		t.Fatal(err)
	}
	if len(methods) != 2 {
		t.Fatal(methods)
	}
}
