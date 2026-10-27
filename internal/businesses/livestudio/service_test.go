package livestudio

import (
	"av-platform/business/internal/infrastructure/database"
	"av-platform/business/internal/platform/identity"
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeMedia struct{}

func (fakeMedia) ConnectStudio(_ context.Context, room, who, grant string, pub, sub bool) (map[string]any, error) {
	return map[string]any{"media_room": room, "identity": who, "grant": grant, "publish": pub, "subscribe": sub}, nil
}
func (fakeMedia) Published(context.Context, string, string) (bool, error) { return true, nil }
func (fakeMedia) Remove(context.Context, string, string) error            { return nil }
func (fakeMedia) Delete(context.Context, string) error                    { return nil }

type fixture struct {
	s     *Service
	a     *identity.Service
	users []identity.User
	now   time.Time
}

func setup(t *testing.T) *fixture {
	t.Helper()
	db, e := database.OpenSQLite(":memory:")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	a, e := identity.New(db)
	if e != nil {
		t.Fatal(e)
	}
	s, e := New(db, fakeMedia{})
	if e != nil {
		t.Fatal(e)
	}
	f := &fixture{s: s, a: a, now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	s.Now = func() time.Time { return f.now }
	a.Now = s.Now
	for _, name := range []string{"host_a", "host_b", "viewer", "guest_1", "guest_2", "guest_3", "guest_4"} {
		u, e := a.Register(context.Background(), name, "good-password-123", false)
		if e != nil {
			t.Fatal(e)
		}
		f.users = append(f.users, u)
	}
	return f
}
func (f *fixture) start(t *testing.T, index int) Broadcast {
	t.Helper()
	v, e := f.s.Start(context.Background(), f.users[index], "直播测试")
	if e != nil {
		t.Fatal(e)
	}
	b := v.(Broadcast)
	_, e = f.s.Execute(context.Background(), f.users[index], "", b.ID, "heartbeat", Command{Connected: true})
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func (f *fixture) cmd(index int, b, action string, c Command) (any, error) {
	return f.s.Execute(context.Background(), f.users[index], "", b, action, c)
}
func TestGiftsAtomicIdempotentAndDaily(t *testing.T) {
	f := setup(t)
	b := f.start(t, 0)
	if _, e := f.cmd(0, b.ID, "gift", Command{RequestID: "self", Points: 1}); e == nil {
		t.Fatal("self gift accepted")
	}
	if _, e := f.cmd(2, b.ID, "gift", Command{RequestID: "one", Points: 50}); e != nil {
		t.Fatal(e)
	}
	if _, e := f.cmd(2, b.ID, "gift", Command{RequestID: "one", Points: 50}); e != nil {
		t.Fatal(e)
	}
	if _, e := f.cmd(2, b.ID, "gift", Command{RequestID: "one", Points: 10}); e == nil {
		t.Fatal("conflicting idempotency accepted")
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, e := f.cmd(2, b.ID, "gift", Command{RequestID: fmt.Sprint(i), Points: 10})
			if e == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if success != 5 {
		t.Fatalf("success=%d", success)
	}
	w, e := f.s.Wallet(context.Background(), f.users[2])
	if e != nil || w.(map[string]any)["remaining"] != 0 {
		t.Fatalf("wallet %v %v", w, e)
	}
	f.now = f.now.Add(24 * time.Hour)
	w, e = f.s.Wallet(context.Background(), f.users[2])
	if e != nil || w.(map[string]any)["remaining"] != 100 {
		t.Fatalf("new day %v %v", w, e)
	}
}
func TestPermissionsGuestsAndRevokedGrant(t *testing.T) {
	f := setup(t)
	b := f.start(t, 0)
	v, e := f.cmd(2, b.ID, "connect", Command{})
	if e != nil {
		t.Fatal(e)
	}
	m := v.(map[string]any)
	if m["publish"] != false {
		t.Fatal("viewer can publish")
	}
	if !f.s.Admit(context.Background(), m["grant"].(string), m["media_room"].(string), m["identity"].(string)) {
		t.Fatal("valid grant rejected")
	}
	if _, e = f.cmd(2, b.ID, "stop", Command{}); e == nil {
		t.Fatal("viewer stopped broadcast")
	}
	for i := 3; i < 7; i++ {
		if _, e = f.cmd(i, b.ID, "guest/request", Command{}); e != nil {
			t.Fatal(e)
		}
		_, e = f.cmd(0, b.ID, "guest/approve", Command{Target: f.users[i].ID})
		if (i < 6) != (e == nil) {
			t.Fatalf("guest %d: %v", i, e)
		}
	}
	if _, e = f.cmd(0, b.ID, "kick", Command{Target: "u-" + f.users[2].ID}); e != nil {
		t.Fatal(e)
	}
	if f.s.Admit(context.Background(), m["grant"].(string), m["media_room"].(string), m["identity"].(string)) {
		t.Fatal("revoked grant accepted")
	}
	if _, e = f.cmd(2, b.ID, "connect", Command{}); e == nil {
		t.Fatal("banned viewer rejoined")
	}
}
func TestPKBoundaryAndRestart(t *testing.T) {
	f := setup(t)
	a := f.start(t, 0)
	b := f.start(t, 1)
	v, e := f.cmd(0, a.ID, "pk/invite", Command{Target: b.ID})
	if e != nil {
		t.Fatal(e)
	}
	id := v.(PK).ID
	if _, e = f.cmd(0, a.ID, "pk/accept", Command{}); e == nil {
		t.Fatal("inviter accepted")
	}
	if _, e = f.cmd(1, b.ID, "pk/accept", Command{}); e != nil {
		t.Fatal(e)
	}
	if _, e = f.cmd(2, a.ID, "gift", Command{RequestID: "in", Points: 10}); e != nil {
		t.Fatal(e)
	}
	f.now = f.now.Add(300 * time.Second)
	if _, e = f.cmd(2, b.ID, "gift", Command{RequestID: "boundary", Points: 50}); e != nil {
		t.Fatal(e)
	}
	for i, bid := range []string{a.ID, b.ID} {
		if _, e = f.cmd(i, bid, "heartbeat", Command{Connected: true}); e != nil {
			t.Fatal(e)
		}
	}
	if e = f.s.Sweep(context.Background()); e != nil {
		t.Fatal(e)
	}
	var state, winner string
	var sa, sb int
	e = f.s.DB.QueryRow("SELECT state,winner,score_a,score_b FROM live_pks WHERE id=?", id).Scan(&state, &winner, &sa, &sb)
	if e != nil || state != "finished" || winner != a.ID || sa != 10 || sb != 0 {
		t.Fatalf("%s %s %d %d %v", state, winner, sa, sb, e)
	}
	if e = f.s.Sweep(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e = f.cmd(0, a.ID, "pk/invite", Command{Target: b.ID}); e != nil {
		t.Fatal(e)
	}
	if _, e = New(f.s.DB, fakeMedia{}); e != nil {
		t.Fatal(e)
	}
	var active int
	_ = f.s.DB.QueryRow("SELECT count(*) FROM live_pks WHERE state IN ('active','invited')").Scan(&active)
	if active != 0 {
		t.Fatal("restart resumed PK")
	}
}
func TestAccountsAndHTTP(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	u, token, e := f.a.Login(ctx, "viewer", "good-password-123")
	if e != nil {
		t.Fatal(e)
	}
	if e = f.a.ChangePassword(ctx, u, "wrong", "new-password-123"); e == nil {
		t.Fatal("wrong old password")
	}
	if e = f.a.ChangePassword(ctx, u, "good-password-123", "new-password-123"); e != nil {
		t.Fatal(e)
	}
	if _, e = f.a.Authenticate(ctx, token); e == nil {
		t.Fatal("old session still valid")
	}
	h := NewHandler(f.s, f.a, "secret-for-admission-long-enough")
	r := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{"name":"viewer","password":"new-password-123"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || len(w.Result().Cookies()) == 0 {
		t.Fatalf("login %d %s", w.Code, w.Body)
	}
	r = httptest.NewRequest("POST", "/api/v1/auth/logout", strings.NewReader(`{}`))
	r.Header.Set("Origin", "https://evil.test")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin mutation")
	}
	r = httptest.NewRequest("POST", Prefix+"admission", strings.NewReader(`{}`))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("admission endpoint unprotected")
	}
}

type unreliableMedia struct {
	fakeMedia
	unavailable bool
	removed     int
}

func (m *unreliableMedia) Remove(context.Context, string, string) error {
	if m.unavailable {
		return fmt.Errorf("offline")
	}
	m.removed++
	return nil
}
func TestRevocationCleanupRetries(t *testing.T) {
	f := setup(t)
	b := f.start(t, 0)
	m := &unreliableMedia{unavailable: true}
	f.s.Media = m
	grant, err := f.cmd(2, b.ID, "connect", Command{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.cmd(0, b.ID, "kick", Command{Target: "u-" + f.users[2].ID}); err != nil {
		t.Fatal(err)
	}
	g := grant.(map[string]any)
	if f.s.Admit(context.Background(), g["grant"].(string), b.MediaRoom, "u-"+f.users[2].ID) {
		t.Fatal("revoked grant accepted during outage")
	}
	var pending int
	f.s.DB.QueryRow("SELECT count(*) FROM live_media_cleanup").Scan(&pending)
	if pending != 2 {
		t.Fatalf("cleanup lost: %d", pending)
	}
	m.unavailable = false
	if err = f.s.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.s.DB.QueryRow("SELECT count(*) FROM live_media_cleanup").Scan(&pending)
	if pending != 0 || m.removed != 2 {
		t.Fatal("cleanup was not retried")
	}
}
