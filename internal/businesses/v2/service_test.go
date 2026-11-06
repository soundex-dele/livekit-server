package v2

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"av-platform/business/internal/platform/control"
	"platform-foundation/server/httpx"
	"platform-foundation/server/persistence"
	foundation "platform-foundation/server/runtime"
)

type mediaFixture struct {
	published bool
	fail      bool
	present   map[string]bool
}

type failedLeaseStore struct{ control.Ephemeral }

func (f failedLeaseStore) Create(context.Context, string, string, time.Duration) (bool, error) {
	return false, errors.New("redis unavailable")
}

func (m *mediaFixture) Status(context.Context, control.Node) (control.NodeStatus, error) {
	return control.NodeStatus{ID: "test", Incarnation: "boot-1", Ready: true, AVMode: true}, nil
}
func (m *mediaFixture) Execute(context.Context, control.Node, control.MediaCommand) error { return nil }
func (m *mediaFixture) Published(context.Context, control.Node, string) (bool, error) {
	if m.fail {
		return false, errors.New("unavailable")
	}
	return m.published, nil
}
func (m *mediaFixture) Present(context.Context, control.Node, string) (map[string]bool, error) {
	if m.fail {
		return nil, errors.New("unavailable")
	}
	return m.present, nil
}

type fixture struct {
	a, b          *Service
	clock         atomic.Int64
	memory        *control.Memory
	media         *mediaFixture
	store, store2 *persistence.Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{memory: control.NewMemory(), media: &mediaFixture{present: map[string]bool{}}}
	f.clock.Store(time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC).UnixMilli())
	now := func() time.Time { return time.UnixMilli(f.clock.Load()) }
	f.memory.Now = now
	c := foundation.DefaultConfig()
	c.Modules = []string{"organizations", "jobs", "events"}
	c.SQLite = filepath.Join(t.TempDir(), "av.db")
	if dsn := os.Getenv("AV_TEST_PG_DSN"); dsn != "" {
		db, e := sql.Open("pgx", dsn)
		if e != nil {
			t.Fatal(e)
		}
		schema := "av_v2_" + control.ID()
		if _, e = db.Exec("CREATE SCHEMA " + schema); e != nil {
			db.Close()
			t.Fatal(e)
		}
		t.Cleanup(func() { _, _ = db.Exec("DROP SCHEMA " + schema + " CASCADE"); _ = db.Close() })
		u, e := url.Parse(dsn)
		if e != nil {
			t.Fatal(e)
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		c.SQLite = ""
		c.DSN = u.String()
	}
	var e error
	f.store, e = foundation.OpenStore(c)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { f.store.DB.Close() })
	if e = control.Migrate(context.Background(), f.store, Schema); e != nil {
		t.Fatal(e)
	}
	f.store2, e = foundation.OpenStore(c)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { f.store2.DB.Close() })
	nodes := []control.Node{{ID: "test", Capacity: 1000}}
	for i, s := range []*persistence.Store{f.store, f.store2} {
		p := control.New(s, f.memory, nodes)
		p.Now = now
		p.Queue.Now = now
		p.Media = f.media
		if i == 0 {
			f.a = New(p)
		} else {
			f.b = New(p)
		}
	}
	if e = f.a.P.SyncNodes(context.Background()); e != nil {
		t.Fatal(e)
	}
	return f
}
func user(id string) control.Principal {
	return control.Principal{ID: id, Scope: "org:one", Kind: "user", Permissions: []string{"*"}}
}
func call(s *Service, p control.Principal, path string, body any, key string) (map[string]any, error) {
	raw, _ := json.Marshal(body)
	out, e := s.Handle(context.Background(), p, "POST", path, raw, key)
	if e != nil {
		return nil, e
	}
	raw, e = json.Marshal(out)
	if e != nil {
		return nil, e
	}
	var result map[string]any
	e = json.Unmarshal(raw, &result)
	return result, e
}
func mustCall(t *testing.T, s *Service, p control.Principal, path string, body any, key string) map[string]any {
	t.Helper()
	out, e := call(s, p, path, body, key)
	if e != nil {
		t.Fatalf("%s: %v", path, e)
	}
	return out
}
func str(m map[string]any, k string) string { v, _ := m[k].(string); return v }
func code(e error) string {
	var h *httpx.HTTPError
	if errors.As(e, &h) {
		return h.Code
	}
	if e == nil {
		return ""
	}
	return e.Error()
}
func (f *fixture) advance(t *testing.T, d time.Duration) {
	t.Helper()
	f.clock.Add(d.Milliseconds())
	if e := f.a.P.SyncNodes(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func (f *fixture) ready(t *testing.T) {
	t.Helper()
	if _, e := f.store.DB.Exec("UPDATE av_room_bindings SET state='ready' WHERE state='preparing'"); e != nil {
		t.Fatal(e)
	}
}

func TestPairingOneUseAcrossInstancesAndTenantIsolation(t *testing.T) {
	f := newFixture(t)
	owner := user("owner")
	created := mustCall(t, f.a, owner, "screen-cast/sessions", map[string]any{"title": "screen"}, "")
	id := str(created, "id")
	c := str(created, "pairing_code")
	var count atomic.Int32
	var winning map[string]any
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := f.a
			if i%2 == 1 {
				s = f.b
			}
			out, e := call(s, control.Principal{}, "screen-cast/redeem", map[string]string{"code": c}, "")
			if e == nil {
				count.Add(1)
				mu.Lock()
				winning = out
				mu.Unlock()
			} else if code(e) != "pairing_unavailable" {
				t.Errorf("redeem: %v", e)
			}
		}(i)
	}
	wg.Wait()
	if count.Load() != 1 {
		t.Fatalf("redemptions=%d", count.Load())
	}
	guest, e := f.a.P.Guest(context.Background(), str(winning, "credential"))
	if e != nil {
		t.Fatal(e)
	}
	out := mustCall(t, f.b, guest, "screen-cast/sessions/"+id+"/connect", nil, "")
	if str(out, "grant_id") == "" {
		t.Fatal("missing media grant")
	}
	other := owner
	other.Scope = "org:two"
	if _, e = f.b.Handle(context.Background(), other, "GET", "screen-cast/sessions/"+id, nil, ""); e == nil {
		t.Fatal("cross-tenant access")
	}
	second := mustCall(t, f.a, owner, "screen-cast/sessions", nil, "")
	guest.ResourceID = id
	if _, e = f.b.Handle(context.Background(), guest, "GET", "screen-cast/sessions/"+str(second, "id"), nil, ""); e == nil {
		t.Fatal("guest escaped resource")
	}
	if _, e = call(f.a, owner, "screen-cast/sessions", nil, "cache-secret"); code(e) != "idempotency_not_supported" {
		t.Fatalf("sensitive response: %v", e)
	}
	stopped := mustCall(t, f.a, owner, "screen-cast/sessions/"+id+"/stop", nil, "")
	operations, ok := stopped["operation_ids"].([]any)
	if stopped["accepted"] != true || !ok || len(operations) == 0 {
		t.Fatalf("stop must expose durable cleanup operations: %v", stopped)
	}
	for _, operation := range operations {
		var resource, scope string
		if e = f.store.DB.QueryRow(f.store.Q("SELECT resource_id,scope FROM av_media_operations WHERE id=?"), operation).Scan(&resource, &scope); e != nil || resource != id || scope != owner.Scope {
			t.Fatalf("unrelated cleanup operation: %v, %s, %s", e, resource, scope)
		}
	}
}

func TestDisabledBusinessClosesPersistedSessionsAndDeniesAdmission(t *testing.T) {
	f := newFixture(t)
	owner := user("owner")
	cast := mustCall(t, f.a, owner, "screen-cast/sessions", nil, "")
	id := str(cast, "id")
	mustCall(t, f.a, control.Principal{}, "screen-cast/redeem", map[string]any{"code": cast["pairing_code"]}, "")
	mustCall(t, f.a, owner, "screen-cast/sessions/"+id+"/connect", nil, "")
	limited := NewWithModules(f.b.P, []string{"conference"})
	if _, e := call(limited, owner, "screen-cast/sessions", nil, ""); code(e) != "module_disabled" {
		t.Fatalf("disabled command: %v", e)
	}
	if _, e := call(limited, owner, "control-grants/missing/approve", nil, ""); code(e) != "module_disabled" {
		t.Fatalf("control grants must follow remote-control ownership: %v", e)
	}
	if e := limited.AuthorizeGrant(context.Background(), owner.Scope, id, owner.ID); code(e) != "module_disabled" {
		t.Fatalf("disabled media admission: %v", e)
	}
	meeting := mustCall(t, limited, owner, "conference/meetings", map[string]any{"title": "still enabled"}, "")
	if e := limited.Sweep(context.Background()); e != nil {
		t.Fatal(e)
	}
	var state string
	if e := f.store.DB.QueryRow(f.store.Q("SELECT state FROM av_resources WHERE id=?"), id).Scan(&state); e != nil || state != "ended" {
		t.Fatalf("disabled session state=%s error=%v", state, e)
	}
	var count int
	if e := f.store.DB.QueryRow(f.store.Q("SELECT COUNT(*) FROM av_media_operations WHERE resource_id=? AND action='DeleteRoom'"), id).Scan(&count); e != nil || count != 1 {
		t.Fatalf("disabled media cleanup count=%d error=%v", count, e)
	}
	if e := f.store.DB.QueryRow(f.store.Q("SELECT state FROM av_resources WHERE id=?"), str(meeting, "id")).Scan(&state); e != nil || isTerminal(state) {
		t.Fatalf("enabled meeting state=%s error=%v", state, e)
	}
}

func TestRemoteLeaseCannotResurrectAndRequiresNewConsent(t *testing.T) {
	f := newFixture(t)
	host := user("host")
	created := mustCall(t, f.a, host, "remote-control/sessions", nil, "")
	id := str(created, "id")
	paired := mustCall(t, f.b, control.Principal{}, "remote-control/redeem", map[string]string{"code": str(created, "pairing_code"), "name": "helper"}, "")
	controller, e := f.a.P.Guest(context.Background(), str(paired, "credential"))
	if e != nil {
		t.Fatal(e)
	}
	request := mustCall(t, f.b, controller, "remote-control/sessions/"+id+"/request", nil, "request-1")
	gid := str(request, "id")
	if _, e = call(f.a, controller, "control-grants/"+gid+"/approve", map[string]any{"local_consent": true}, ""); code(e) != "local_consent_required" {
		t.Fatalf("controller approved: %v", e)
	}
	approved := mustCall(t, f.a, host, "control-grants/"+gid+"/approve", map[string]any{"local_consent": true}, "")
	grant := approved["grant"].(map[string]any)
	if str(grant, "host_identity") == "" || str(grant, "controller_identity") == "" || str(grant, "host_identity") == str(grant, "controller_identity") {
		t.Fatal("invalid peer identities")
	}
	epoch := int64(approved["epoch"].(float64))
	if e = f.a.AuthorizeGrant(context.Background(), host.Scope, id, host.ID); e != nil {
		t.Fatal(e)
	}
	f.advance(t, 9*time.Second)
	mustCall(t, f.b, host, "control-grants/"+gid+"/renew", map[string]any{"epoch": epoch}, "")
	f.advance(t, 11*time.Second)
	if e = f.b.AuthorizeGrant(context.Background(), host.Scope, id, host.ID); code(e) != "control_lease_inactive" {
		t.Fatalf("expired remote media admitted: %v", e)
	}
	mustCall(t, f.a, host, "remote-control/sessions/"+id+"/heartbeat", nil, "")
	mustCall(t, f.a, controller, "remote-control/sessions/"+id+"/heartbeat", nil, "")
	if _, e = call(f.b, host, "control-grants/"+gid+"/renew", map[string]any{"epoch": epoch}, ""); code(e) != "lease_expired" {
		t.Fatalf("expired lease renewed: %v", e)
	}
	if e = f.a.Sweep(context.Background()); e != nil {
		t.Fatal(e)
	}
	second := mustCall(t, f.b, controller, "remote-control/sessions/"+id+"/request", nil, "request-2")
	if second["epoch"].(float64) <= float64(epoch) {
		t.Fatal("epoch not advanced")
	}
	secondID := str(second, "id")
	mustCall(t, f.a, host, "control-grants/"+secondID+"/approve", map[string]any{"local_consent": true}, "")
	mustCall(t, f.b, controller, "control-grants/"+secondID+"/revoke", nil, "revoke")
	if _, e = call(f.a, host, "control-grants/"+secondID+"/renew", map[string]any{"epoch": epoch + 1}, ""); code(e) != "control_grant_inactive" {
		t.Fatalf("revoked lease renewed: %v", e)
	}
}

func TestApprovalWithoutLeaseFailsClosed(t *testing.T) {
	f := newFixture(t)
	host := user("host")
	created := mustCall(t, f.a, host, "remote-control/sessions", nil, "")
	id := str(created, "id")
	paired := mustCall(t, f.b, control.Principal{}, "remote-control/redeem", map[string]string{"code": str(created, "pairing_code")}, "")
	controller, e := f.a.P.Guest(context.Background(), str(paired, "credential"))
	if e != nil {
		t.Fatal(e)
	}
	request := mustCall(t, f.b, controller, "remote-control/sessions/"+id+"/request", nil, "")
	gid := str(request, "id")
	f.a.P.Ephemeral = failedLeaseStore{f.memory}
	if _, e = call(f.a, host, "control-grants/"+gid+"/approve", map[string]bool{"local_consent": true}, ""); e == nil {
		t.Fatal("approved without control lease")
	}
	if e = f.b.AuthorizeGrant(context.Background(), host.Scope, id, host.ID); e == nil {
		t.Fatal("media allowed after failed approval")
	}
	var active int
	if e = f.store.DB.QueryRow(f.store.Q("SELECT COUNT(*) FROM av_rtc_grants WHERE scope=? AND resource_id=? AND status='active'"), host.Scope, id).Scan(&active); e != nil {
		t.Fatal(e)
	}
	if active != 0 {
		t.Fatal("approval compensation retained media grants")
	}
}

func TestMeetingCapacityAdmissionShareAndRecovery(t *testing.T) {
	f := newFixture(t)
	host := user("host")
	created := mustCall(t, f.a, host, "conference/meetings", map[string]any{"title": "meeting"}, "create")
	id, number := str(created, "id"), str(created, "number")
	again := mustCall(t, f.b, host, "conference/meetings", map[string]any{"title": "meeting"}, "create")
	if str(again, "id") != id {
		t.Fatal("create idempotency")
	}
	if _, e := call(f.b, host, "conference/meetings", map[string]any{"title": "changed"}, "create"); code(e) != "idempotency_conflict" {
		t.Fatalf("key conflict=%v", e)
	}
	for i := 0; i < 23; i++ {
		mustCall(t, f.a, user(fmt.Sprintf("m%02d", i)), "conference/join", map[string]string{"number": number}, "")
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := f.a
			if i%2 == 1 {
				s = f.b
			}
			_, e := call(s, user(fmt.Sprintf("last%d", i)), "conference/join", map[string]string{"number": number}, "")
			if e == nil {
				accepted.Add(1)
			} else if code(e) != "meeting_full" {
				t.Errorf("join: %v", e)
			}
		}(i)
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("last slot accepted %d", accepted.Load())
	}
	mustCall(t, f.a, user("m00"), "conference/meetings/"+id+"/share-start", map[string]string{"target": "m01"}, "")
	var sharing string
	if e := f.store.DB.QueryRow(f.store.Q("SELECT sharing FROM av_meetings WHERE id=?"), id).Scan(&sharing); e != nil {
		t.Fatal(e)
	}
	if sharing != "m00" {
		t.Fatalf("sharing=%s", sharing)
	}
	if _, e := call(f.b, user("m01"), "conference/meetings/"+id+"/share-start", nil, ""); code(e) != "sharing_busy" {
		t.Fatalf("share collision: %v", e)
	}
	mustCall(t, f.a, host, "conference/meetings/"+id+"/lock", map[string]bool{"enabled": true}, "")
	mustCall(t, f.b, user("m00"), "conference/join", map[string]string{"number": number}, "")
	f.ready(t)
	ids, e := f.a.identities(context.Background(), host.Scope, id, "conference")
	if e != nil {
		t.Fatal(e)
	}
	f.media.present[ids["m00"]] = true
	f.advance(t, 31*time.Second)
	if e = f.a.Sweep(context.Background()); e != nil {
		t.Fatal(e)
	}
	f.advance(t, 121*time.Second)
	if e = f.b.Sweep(context.Background()); e != nil {
		t.Fatal(e)
	}
	var next string
	if e = f.store.DB.QueryRow(f.store.Q("SELECT host FROM av_meetings WHERE id=?"), id).Scan(&next); e != nil {
		t.Fatal(e)
	}
	if next != "m00" {
		t.Fatalf("host did not transfer: %s", next)
	}
}

func TestGuestWaitingNeverBecomesActiveFromPresence(t *testing.T) {
	f := newFixture(t)
	host := user("host")
	created := mustCall(t, f.a, host, "conference/meetings", map[string]any{"title": "meeting"}, "")
	id := str(created, "id")
	join := mustCall(t, f.a, control.Principal{}, "conference/join", map[string]string{"number": str(created, "number")}, "")
	guest, e := f.a.P.Guest(context.Background(), str(join, "credential"))
	if e != nil {
		t.Fatal(e)
	}
	if str(join, "state") != "waiting" {
		t.Fatal("guest auto-admitted")
	}
	mustCall(t, f.a, host, "conference/meetings/"+id+"/connect", nil, "")
	f.ready(t)
	f.advance(t, 31*time.Second)
	if e = f.a.Sweep(context.Background()); e != nil {
		t.Fatal(e)
	}
	mustCall(t, f.b, guest, "conference/meetings/"+id+"/heartbeat", nil, "")
	if e = f.b.Sweep(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e = call(f.a, guest, "conference/meetings/"+id+"/connect", nil, ""); code(e) != "waiting_for_admission" {
		t.Fatalf("waiting bypass: %v", e)
	}
	mustCall(t, f.a, host, "conference/meetings/"+id+"/admit", map[string]string{"target": guest.ID}, "")
	mustCall(t, f.b, guest, "conference/meetings/"+id+"/connect", nil, "")
}

func startLive(t *testing.T, f *fixture, host control.Principal) string {
	t.Helper()
	room := mustCall(t, f.a, host, "live-studio/rooms", map[string]string{"title": "show"}, "room")
	started := mustCall(t, f.a, host, "live-studio/rooms/"+str(room, "id")+"/start", nil, "start")
	return str(started, "id")
}
func TestLiveStageCapacityGiftAtomicityAndPK(t *testing.T) {
	f := newFixture(t)
	host := user("host")
	id := startLive(t, f, host)
	for i := 0; i < 4; i++ {
		viewer := user(fmt.Sprintf("v%d", i))
		mustCall(t, f.b, viewer, "live-studio/broadcasts/"+id+"/watch", nil, "")
		mustCall(t, f.b, viewer, "live-studio/broadcasts/"+id+"/stage-request", nil, "")
	}
	var count atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := f.a
			if i%2 == 1 {
				s = f.b
			}
			_, e := call(s, host, "live-studio/broadcasts/"+id+"/stage-approve", map[string]string{"target": fmt.Sprintf("v%d", i)}, "")
			if e == nil {
				count.Add(1)
			} else if code(e) != "stage_full" {
				t.Errorf("stage: %v", e)
			}
		}(i)
	}
	wg.Wait()
	if count.Load() != 3 {
		t.Fatalf("stage count=%d", count.Load())
	}
	other := startLive(t, f, user("other"))
	if _, e := call(f.a, host, "live-studio/broadcasts/"+id+"/pk-request", map[string]string{"target": other}, ""); code(e) != "stage_busy" {
		t.Fatalf("PK during guests: %v", e)
	}
	for i := 0; i < 4; i++ {
		mustCall(t, f.a, host, "live-studio/broadcasts/"+id+"/stage-remove", map[string]string{"target": fmt.Sprintf("v%d", i)}, "")
	}
	request := mustCall(t, f.a, host, "live-studio/broadcasts/"+id+"/pk-request", map[string]string{"target": other}, "")
	pkID := str(request, "id")
	mustCall(t, f.b, user("other"), "live-studio/broadcasts/"+other+"/pk-accept", nil, "")
	leftStage := mustCall(t, f.a, host, "live-studio/broadcasts/"+id+"/connect", map[string]bool{"stage": true}, "")
	rightStage := mustCall(t, f.b, user("other"), "live-studio/broadcasts/"+other+"/connect", map[string]bool{"stage": true}, "")
	var lb, rb string
	if e := f.store.DB.QueryRow(f.store.Q("SELECT binding_id FROM av_rtc_grants WHERE id=?"), str(leftStage, "grant_id")).Scan(&lb); e != nil {
		t.Fatal(e)
	}
	if e := f.store.DB.QueryRow(f.store.Q("SELECT binding_id FROM av_rtc_grants WHERE id=?"), str(rightStage, "grant_id")).Scan(&rb); e != nil {
		t.Fatal(e)
	}
	if lb != rb || str(rightStage, "resource_id") != id {
		t.Fatalf("PK hosts isolated: %s %s", lb, rb)
	}
	viewer := user("v0")
	for i := 0; i < 2; i++ {
		mustCall(t, f.a, viewer, "live-studio/broadcasts/"+id+"/gift", map[string]int{"amount": 50}, "same-gift")
	}
	wallet := mustCall(t, f.a, viewer, "live-studio/broadcasts/"+id+"/gift", map[string]int{"amount": 50}, "second-gift")
	if wallet["balance"].(float64) != 0 {
		t.Fatal("wallet debit")
	}
	if _, e := call(f.b, viewer, "live-studio/broadcasts/"+id+"/gift", map[string]int{"amount": 1}, "overdraft"); code(e) != "insufficient_points" {
		t.Fatalf("overdraft: %v", e)
	}
	var score int64
	if e := f.store.DB.QueryRow(f.store.Q("SELECT left_score FROM av_live_pk WHERE id=?"), pkID).Scan(&score); e != nil {
		t.Fatal(e)
	}
	if score != 100 {
		t.Fatalf("PK double counted %d", score)
	}
	if _, e := call(f.a, host, "live-studio/broadcasts/"+id+"/gift", map[string]int{"amount": 1}, ""); code(e) != "gift_not_allowed" {
		t.Fatalf("self gift: %v", e)
	}
	mustCall(t, f.a, host, "live-studio/broadcasts/"+id+"/end", nil, "")
	var state string
	if e := f.store.DB.QueryRow(f.store.Q("SELECT state FROM av_live_pk WHERE id=?"), pkID).Scan(&state); e != nil || state != "ended" {
		t.Fatalf("PK not ended: %s %v", state, e)
	}
}

func TestUnknownMediaDoesNotEndLiveAndNewProcessPreservesState(t *testing.T) {
	f := newFixture(t)
	id := startLive(t, f, user("host"))
	f.ready(t)
	f.media.published = true
	if e := f.a.Sweep(context.Background()); e != nil {
		t.Fatal(e)
	}
	f.media.fail = true
	f.advance(t, 10*time.Minute)
	if e := f.b.Sweep(context.Background()); e != nil {
		t.Fatal(e)
	}
	var state string
	if e := f.store.DB.QueryRow(f.store.Q("SELECT state FROM av_resources WHERE id=?"), id).Scan(&state); e != nil || state != "live" {
		t.Fatalf("unknown media ended broadcast %s %v", state, e)
	}
	f.media.fail = false
	f.media.published = false
	if e := f.b.Sweep(context.Background()); e != nil {
		t.Fatal(e)
	}
	f.advance(t, 61*time.Second)
	if e := f.a.Sweep(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e := f.store.DB.QueryRow(f.store.Q("SELECT state FROM av_resources WHERE id=?"), id).Scan(&state); e != nil || state != "ended" {
		t.Fatalf("known absent media not ended %s %v", state, e)
	}
}
