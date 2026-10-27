package conference

import (
	"av-platform/business/internal/infrastructure/database"
	"av-platform/business/internal/platform/identity"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeMedia struct {
	peers                   []Peer
	deleted, removed, muted int
	unavailable             bool
}

func (f *fakeMedia) Credentials(_ context.Context, r, i, n, g string, share bool) (map[string]any, error) {
	return map[string]any{"room": r, "participant_identity": i, "grant": g}, nil
}
func (f *fakeMedia) Participants(context.Context, string) ([]Peer, error) {
	if f.unavailable {
		return nil, errors.New("offline")
	}
	return f.peers, nil
}
func (f *fakeMedia) Delete(context.Context, string) error {
	if f.unavailable {
		return errors.New("offline")
	}
	f.deleted++
	return nil
}
func (f *fakeMedia) Remove(context.Context, string, string) error {
	if f.unavailable {
		return errors.New("offline")
	}
	f.removed++
	return nil
}
func (f *fakeMedia) Mute(context.Context, string, string, string) error  { f.muted++; return nil }
func (f *fakeMedia) Sharing(context.Context, string, string, bool) error { return nil }

type fixture struct {
	s          *Service
	media      *fakeMedia
	owner      Actor
	ctx        context.Context
	id, number string
}

func setup(t *testing.T, scheduled bool) *fixture {
	t.Helper()
	db, e := database.OpenSQLite(filepath.Join(t.TempDir(), "meeting.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	m := &fakeMedia{}
	s, e := New(db, m)
	if e != nil {
		t.Fatal(e)
	}
	f := &fixture{s: s, media: m, ctx: context.Background(), owner: Actor{User: identity.User{ID: "owner", Name: "主持人"}}}
	out, e := s.Create(f.ctx, f.owner, Command{Title: "测试会议", Scheduled: scheduled}, "create")
	if e != nil {
		t.Fatal(e)
	}
	f.id = out.(map[string]any)["id"].(string)
	f.number = out.(map[string]any)["number"].(string)
	return f
}
func (f *fixture) join(t *testing.T, a Actor) *Member {
	t.Helper()
	_, e := f.s.Join(f.ctx, a, Command{Number: f.number, Name: "参会者"}, "join")
	if e != nil {
		t.Fatal(e)
	}
	m, e := f.s.load(f.ctx, f.id)
	if e != nil {
		t.Fatal(e)
	}
	return member(m, a)
}
func (f *fixture) exec(t *testing.T, action string, a Actor, c Command, key string) {
	t.Helper()
	if _, e := f.s.Execute(f.ctx, f.id, action, a, c, key); e != nil {
		t.Fatal(action, e)
	}
}
func TestGuestAdmissionRevocationAndHistory(t *testing.T) {
	f := setup(t, false)
	guest := Actor{Credential: identity.ID()}
	p := f.join(t, guest)
	if p.Status != "waiting" {
		t.Fatal("guest bypassed waiting")
	}
	if _, e := f.s.Credentials(f.ctx, f.id, guest); e == nil {
		t.Fatal("waiting guest received media token")
	}
	f.exec(t, "approve", f.owner, Command{Target: p.ID}, "approve")
	creds, e := f.s.Credentials(f.ctx, f.id, guest)
	if e != nil {
		t.Fatal(e)
	}
	v := creds.(map[string]any)
	if !f.s.Admitted(f.ctx, v["room"].(string), p.Identity, v["grant"].(string)) {
		t.Fatal("approved denied")
	}
	f.exec(t, "remove", f.owner, Command{Target: p.ID}, "remove")
	if f.s.Admitted(f.ctx, v["room"].(string), p.Identity, v["grant"].(string)) {
		t.Fatal("old token admitted")
	}
	if _, e = f.s.Join(f.ctx, guest, Command{Number: f.number, Name: "换个名字"}, ""); e == nil {
		t.Fatal("removed guest returned")
	}
	if e = f.s.Sweep(f.ctx); e != nil {
		t.Fatal(e)
	}
	if f.media.removed != 1 {
		t.Fatal("media not removed")
	}
	other := Actor{User: identity.User{ID: "outsider"}}
	out, _ := f.s.List(f.ctx, other)
	if len(out.([]any)) != 0 {
		t.Fatal("history leaked")
	}
}
func TestIdempotencyAndConcurrentCapacity(t *testing.T) {
	f := setup(t, false)
	out, e := f.s.Create(f.ctx, f.owner, Command{Title: "测试会议"}, "create")
	if e != nil || out.(map[string]any)["id"] != f.id {
		t.Fatal("create not idempotent", e)
	}
	if _, e = f.s.Create(f.ctx, f.owner, Command{Title: "different"}, "create"); e == nil {
		t.Fatal("conflict accepted")
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	count := 0
	for i := 0; i < 35; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := f.s.Join(f.ctx, Actor{Credential: identity.ID()}, Command{Number: f.number, Name: "游客"}, "")
			if e == nil {
				mu.Lock()
				count++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if count != 24 {
		t.Fatalf("admitted %d guests, expected 24 + host", count)
	}
}
func TestScheduleAndAuthorization(t *testing.T) {
	f := setup(t, true)
	if _, e := f.s.Credentials(f.ctx, f.id, f.owner); e == nil {
		t.Fatal("scheduled credentials")
	}
	guest := Actor{Credential: identity.ID()}
	p := f.join(t, guest)
	if _, e := f.s.Execute(f.ctx, f.id, "start", guest, Command{}, "bad"); e == nil {
		t.Fatal("guest started")
	}
	f.exec(t, "edit", f.owner, Command{Title: "改期", Starts: time.Now().UTC().Format(time.RFC3339), Duration: 30}, "edit")
	f.exec(t, "start", f.owner, Command{}, "start")
	f.exec(t, "lock", f.owner, Command{Enabled: true}, "lock")
	if _, e := f.s.Join(f.ctx, Actor{Credential: identity.ID()}, Command{Number: f.number, Name: "later"}, ""); e == nil {
		t.Fatal("lock bypassed")
	}
	f.exec(t, "approve", f.owner, Command{Target: p.ID}, "approve")
	if _, e := f.s.Credentials(f.ctx, f.id, guest); e != nil {
		t.Fatal("locked approved member denied", e)
	}
	if _, e := f.s.Execute(f.ctx, f.id, "end", guest, Command{}, "end"); e == nil {
		t.Fatal("guest ended meeting")
	}
}
func TestDurableEndAndReconciliation(t *testing.T) {
	f := setup(t, false)
	p := f.join(t, f.owner)
	f.media.peers = []Peer{{Identity: p.Identity}}
	if e := f.s.Sweep(f.ctx); e != nil {
		t.Fatal(e)
	}
	f.exec(t, "end", f.owner, Command{}, "end")
	f.media.unavailable = true
	f.s.Sweep(f.ctx)
	m, _ := f.s.load(f.ctx, f.id)
	if m.State != "ending" || m.Jobs[0].State != "retrying" {
		t.Fatal("cleanup failure hidden")
	}
	if _, e := f.s.Credentials(f.ctx, f.id, f.owner); e == nil {
		t.Fatal("issued after end")
	}
	s, e := New(f.s.DB, f.media)
	if e != nil {
		t.Fatal(e)
	}
	f.media.unavailable = false
	if e = s.Sweep(f.ctx); e != nil {
		t.Fatal(e)
	}
	m, _ = s.load(f.ctx, f.id)
	if m.State != "ended" || f.media.deleted != 1 {
		t.Fatal("restart failed cleanup")
	}
	if _, e = s.Execute(f.ctx, f.id, "end", f.owner, Command{}, "end"); e != nil {
		t.Fatal("end retry not idempotent", e)
	}
}
func TestHostTimeoutAndSharing(t *testing.T) {
	f := setup(t, false)
	now := time.Now()
	f.s.Now = func() time.Time { return now }
	owner := f.join(t, f.owner)
	a := Actor{User: identity.User{ID: "second", Name: "second"}}
	p := f.join(t, a)
	f.exec(t, "approve", f.owner, Command{Target: p.ID}, "approve")
	f.media.peers = []Peer{{Identity: owner.Identity}, {Identity: p.Identity}}
	f.s.Sweep(f.ctx)
	f.exec(t, "share", a, Command{Enabled: true}, "share")
	if _, e := f.s.Execute(f.ctx, f.id, "share", f.owner, Command{Enabled: true}, "share"); e == nil {
		t.Fatal("concurrent sharing")
	}
	f.exec(t, "share", a, Command{Enabled: false}, "stopshare")
	f.media.peers = []Peer{{Identity: p.Identity}}
	f.s.Sweep(f.ctx)
	now = now.Add(121 * time.Second)
	f.s.Sweep(f.ctx)
	m, _ := f.s.load(f.ctx, f.id)
	if m.Host != p.ID {
		t.Fatal("host did not transfer")
	}
}
func TestHTTPGuestAndAdmission(t *testing.T) {
	f := setup(t, false)
	auth, e := identity.New(f.s.DB)
	if e != nil {
		t.Fatal(e)
	}
	h := NewHandler(f.s, auth, "a-secret-with-at-least-24-characters")
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", Prefix+"guest", strings.NewReader(`{}`))
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var data map[string]string
	json.Unmarshal(w.Body.Bytes(), &data)
	if len(data["guest_token"]) != 48 {
		t.Fatal("weak guest token")
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		w = httptest.NewRecorder()
		r = httptest.NewRequest(method, Prefix+"admission", strings.NewReader(`{}`))
		h.ServeHTTP(w, r)
		if w.Code < 400 {
			t.Fatal("unprotected admission")
		}
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", Prefix+"meetings", strings.NewReader(`{"title":"x"}`))
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("guest created meeting", w.Code)
	}
}

func TestPasswordScopeAndWaitingExpiry(t *testing.T) {
	f := setup(t, true)
	now := time.Now()
	f.s.Now = func() time.Time { return now }
	f.exec(t, "edit", f.owner, Command{Title: "有密码的预约", Password: "123456", Starts: now.UTC().Format(time.RFC3339)}, "edit-password")
	guest := Actor{Credential: identity.ID()}
	if _, e := f.s.Join(f.ctx, guest, Command{Number: f.number, Name: "访客", Password: "wrong"}, ""); e == nil {
		t.Fatal("wrong password accepted")
	}
	if _, e := f.s.Join(f.ctx, guest, Command{Number: f.number, Name: "访客", Password: "123456"}, ""); e != nil {
		t.Fatal(e)
	}
	m, _ := f.s.load(f.ctx, f.id)
	p := member(m, guest)
	if f.s.Admitted(f.ctx, m.Room, p.Identity, p.Grant) {
		t.Fatal("scheduled waiting grant accepted")
	}
	if f.s.Admitted(f.ctx, m.Room, "other", p.Grant) {
		t.Fatal("grant not identity scoped")
	}
	now = now.Add(121 * time.Second)
	if e := f.s.Sweep(f.ctx); e != nil {
		t.Fatal(e)
	}
	m, _ = f.s.load(f.ctx, f.id)
	if member(m, guest).Status != "left" {
		t.Fatal("abandoned wait not released")
	}
	f.exec(t, "cancel", f.owner, Command{}, "cancel")
	if _, e := f.s.Join(f.ctx, guest, Command{Number: f.number, Name: "访客", Password: "123456"}, ""); e == nil {
		t.Fatal("cancelled meeting admitted")
	}
}
func TestConcurrentEndBlocksFurtherCredentials(t *testing.T) {
	f := setup(t, false)
	a := Actor{Credential: identity.ID()}
	p := f.join(t, a)
	f.exec(t, "approve", f.owner, Command{Target: p.ID}, "approve")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = f.s.Credentials(f.ctx, f.id, a) }()
	}
	f.exec(t, "end", f.owner, Command{}, "end")
	wg.Wait()
	for i := 0; i < 10; i++ {
		if _, e := f.s.Credentials(f.ctx, f.id, a); e == nil {
			t.Fatal("issued token after end committed")
		}
	}
}

func TestPendingCleanupSerializesShareAndRejoin(t *testing.T) {
	f := setup(t, false)
	a := Actor{User: identity.User{ID: "second", Name: "second"}}
	p := f.join(t, a)
	f.exec(t, "approve", f.owner, Command{Target: p.ID}, "approve")
	f.exec(t, "share", a, Command{Enabled: true}, "share-on")
	f.exec(t, "share", a, Command{Enabled: false}, "share-off")
	if _, e := f.s.Execute(f.ctx, f.id, "share", f.owner, Command{Enabled: true}, "share-owner"); e == nil {
		t.Fatal("second publisher admitted before prior permission was revoked")
	}
	if e := f.s.Sweep(f.ctx); e != nil {
		t.Fatal(e)
	}
	f.exec(t, "share", f.owner, Command{Enabled: true}, "share-owner")
	f.exec(t, "leave", a, Command{}, "leave")
	if _, e := f.s.Join(f.ctx, a, Command{Number: f.number, Name: "second"}, ""); e == nil {
		t.Fatal("rejoin allowed before old removal command completed")
	}
	if e := f.s.Sweep(f.ctx); e != nil {
		t.Fatal(e)
	}
	f.join(t, a)
}

func TestEndSupersedesPendingJobsAndTimesAreUTC(t *testing.T) {
	f := setup(t, true)
	f.exec(t, "edit", f.owner, Command{Title: "UTC", Starts: "2026-10-28T10:00:00+08:00"}, "edit")
	m, _ := f.s.load(f.ctx, f.id)
	if m.Starts != "2026-10-28T02:00:00Z" {
		t.Fatal("time was not normalized", m.Starts)
	}
	f.exec(t, "start", f.owner, Command{}, "start")
	for i := 0; i < 5; i++ {
		f.exec(t, "mute", f.owner, Command{Target: m.Host}, "mute-"+string(rune('a'+i)))
	}
	f.exec(t, "end", f.owner, Command{}, "end")
	if e := f.s.Sweep(f.ctx); e != nil {
		t.Fatal(e)
	}
	m, _ = f.s.load(f.ctx, f.id)
	if m.State != "ended" {
		t.Fatal("cleanup was starved by pending member commands")
	}
	for _, j := range m.Jobs {
		if j.State != "done" {
			t.Fatal("obsolete command was not superseded")
		}
	}
}

func TestMeetingNumberCollisionRetries(t *testing.T) {
	f := setup(t, false)
	attempts := 0
	fresh := "100000000"
	if fresh == f.number {
		fresh = "100000001"
	}
	f.s.number = func() (string, error) {
		attempts++
		if attempts == 1 {
			return f.number, nil
		}
		return fresh, nil
	}
	out, e := f.s.Create(f.ctx, f.owner, Command{Title: "collision"}, "second")
	if e != nil {
		t.Fatal(e)
	}
	if attempts != 2 || out.(map[string]any)["number"] != fresh {
		t.Fatal("collision was not retried", out)
	}
}
