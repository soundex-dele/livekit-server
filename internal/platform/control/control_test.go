package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"platform-foundation/server/db"
	"platform-foundation/server/persistence"
	foundation "platform-foundation/server/runtime"
	"platform-foundation/server/taskqueue"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeMedia struct {
	mu    sync.Mutex
	calls []MediaCommand
	hook  func()
	inc   string
}

func (f *fakeMedia) Status(context.Context, Node) (NodeStatus, error) {
	return NodeStatus{ID: "node", Incarnation: f.inc, Ready: true, AVMode: true}, nil
}
func (f *fakeMedia) Execute(_ context.Context, _ Node, c MediaCommand) error {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	if f.hook != nil {
		f.hook()
	}
	return nil
}
func controlStore(t *testing.T, postgres bool) *persistence.Store {
	t.Helper()
	c := foundation.DefaultConfig()
	c.Modules = []string{"organizations", "jobs", "events"}
	c.SQLite = filepath.Join(t.TempDir(), "control.db")
	if postgres {
		dsn := os.Getenv("AV_TEST_PG_DSN")
		if dsn == "" {
			t.Skip("AV_TEST_PG_DSN not set")
		}
		admin, e := db.OpenPG(dsn)
		if e != nil {
			t.Fatal(e)
		}
		schema := "av_test_" + ID()[:16]
		if _, e = admin.Exec("CREATE SCHEMA " + schema); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE"); _ = admin.Close() })
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
	s, e := foundation.OpenStore(c)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = s.DB.Close() })
	if e = Migrate(context.Background(), s, ""); e != nil {
		t.Fatal(e)
	}
	if e = Migrate(context.Background(), s, ""); e != nil {
		t.Fatal("repeat migration", e)
	}
	return s
}
func platformTest(t *testing.T, postgres bool) (*Platform, *fakeMedia) {
	t.Helper()
	s := controlStore(t, postgres)
	n := Node{ID: "node", APIURL: "http://127.0.0.1:1", PublicURL: "ws://127.0.0.1:2", RTCKey: "rtc", RTCSecret: strings.Repeat("r", 32), ManagementKey: "manage", ManagementSecret: strings.Repeat("m", 32), AdmissionSecret: strings.Repeat("a", 32), Capacity: 10}
	p := New(s, NewMemory(), []Node{n})
	f := &fakeMedia{inc: "boot-1"}
	p.Media = f
	if e := p.SyncNodes(context.Background()); e != nil {
		t.Fatal(e)
	}
	return p, f
}
func seedGrant(t *testing.T, p *Platform, scope, id, subject string) RTCGrant {
	t.Helper()
	var g RTCGrant
	e := p.Store.Within(context.Background(), scope, func(tx *persistence.Tx) error {
		r := Resource{ID: id, Scope: scope, Kind: "meeting", Owner: subject, Title: "test", State: "active", Version: 1, Created: 1, Updated: 1}
		if e := InsertResource(context.Background(), tx, r); e != nil {
			return e
		}
		if _, e := tx.Exec(context.Background(), "INSERT INTO av_participants(scope,resource_id,subject_id,role,state,policy_version,updated) VALUES(?,?,?,'host','active',1,1)", scope, id, subject); e != nil {
			return e
		}
		var e error
		g, e = p.Grant(context.Background(), tx, r, subject, "host", "main", Capabilities{Publish: true, Subscribe: true, Sources: []string{"camera", "microphone"}})
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
	return g
}
func drainMedia(t *testing.T, p *Platform) {
	t.Helper()
	for {
		j, e := p.Queue.Claim(context.Background(), "job", []string{"av.media"}, 30*time.Second)
		if errors.Is(e, persistence.ErrNotFound) {
			return
		}
		if e != nil {
			t.Fatal(e)
		}
		result, e := p.Execute(context.Background(), j, func(int) error { return nil })
		if e != nil {
			t.Fatal(e)
		}
		if e = p.Queue.Finish(context.Background(), j, result, nil); e != nil {
			t.Fatal(e)
		}
	}
}
func TestGrantRevocationNodeAndPermissionVersions(t *testing.T) {
	p, f := platformTest(t, false)
	ctx := context.Background()
	g := seedGrant(t, p, "org:one", "meeting", "user")
	actor := Principal{ID: "user", Scope: "org:one", Kind: "user"}
	if _, e := p.Credentials(ctx, actor, g.ID); e == nil {
		t.Fatal("issued credentials before room exists")
	}
	drainMedia(t, p)
	credentials, e := p.Credentials(ctx, actor, g.ID)
	if e != nil {
		t.Fatal(e)
	}
	token := credentials["media_token"].(string)
	part, e := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
	if e != nil {
		t.Fatal(e)
	}
	var claims map[string]any
	if e = json.Unmarshal(part, &claims); e != nil {
		t.Fatal(e)
	}
	if claims["iss"] != "rtc" || claims["av_grant"] != g.ID {
		t.Fatal("wrong signing purpose or grant")
	}
	q := AdmissionRequest{NodeID: "node", NodeIncarnation: "boot-1", Room: credentials["room"].(string), Identity: g.Identity, GrantID: g.ID, PolicyVersion: 1, BindingGeneration: 1}
	if _, e = p.Admit(ctx, q); e != nil {
		t.Fatal(e)
	}
	if _, e = p.Credentials(ctx, Principal{ID: "user", Scope: "org:two"}, g.ID); e == nil {
		t.Fatal("cross organization credentials")
	}
	bad := q
	bad.NodeID = "another"
	if _, e = p.Admit(ctx, bad); e == nil {
		t.Fatal("wrong node admitted")
	}
	if _, e = p.Store.DB.Exec("UPDATE av_participants SET policy_version=2"); e != nil {
		t.Fatal(e)
	}
	if _, e = p.Admit(ctx, q); e == nil {
		t.Fatal("old permissions admitted")
	}
	if _, e = p.Store.DB.Exec("UPDATE av_participants SET policy_version=1"); e != nil {
		t.Fatal(e)
	}
	f.inc = "boot-2"
	if e = p.SyncNodes(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = p.Admit(ctx, q); e == nil {
		t.Fatal("old incarnation admitted")
	}
	f.inc = "boot-1"
	_ = p.SyncNodes(ctx)
	if e = p.Store.Within(ctx, actor.Scope, func(tx *persistence.Tx) error { return p.Revoke(ctx, tx, "meeting", "user") }); e != nil {
		t.Fatal(e)
	}
	if _, e = p.Admit(ctx, q); e == nil {
		t.Fatal("revoked grant admitted")
	}
	drainMedia(t, p)
	if f.calls[len(f.calls)-1].Method != "RemoveParticipant" {
		t.Fatal("missing durable cleanup")
	}
}
func TestMediaWorkerCannotCommitAfterLosingLease(t *testing.T) {
	p, f := platformTest(t, false)
	g := seedGrant(t, p, "org:one", "meeting", "user")
	j, e := p.Queue.Claim(context.Background(), "job", []string{"av.media"}, 30*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	f.hook = func() {
		_, e = p.Store.DB.ExecContext(context.Background(), p.Store.Q("UPDATE pf_queue SET fence=fence+1 WHERE id=?"), j.ID)
		if e != nil {
			t.Fatal(e)
		}
	}
	_, e = p.Execute(context.Background(), j, func(int) error { return nil })
	if !errors.Is(e, taskqueue.ErrStale) {
		t.Fatalf("expected stale lease: %v", e)
	}
	var state string
	_ = p.Store.DB.QueryRowContext(context.Background(), p.Store.Q("SELECT state FROM av_room_bindings WHERE id=?"), g.BindingID).Scan(&state)
	if state != "preparing" {
		t.Fatal("stale worker made room ready")
	}
}
func TestAtomicMutationReplayAndScopeIsolation(t *testing.T) {
	for _, pg := range []bool{false, true} {
		name := "sqlite"
		if pg {
			name = "postgres"
		}
		t.Run(name, func(t *testing.T) {
			p, _ := platformTest(t, pg)
			p2 := New(p.Store, p.Ephemeral, p.Nodes)
			actor := Principal{ID: "u", Scope: "org:a"}
			var calls int
			fn := func(tx *persistence.Tx) (any, error) {
				calls++
				e := InsertResource(context.Background(), tx, Resource{ID: "once", Scope: actor.Scope, Kind: "cast", Owner: "u", Title: "once", State: "waiting", Version: 1})
				return map[string]string{"id": "once"}, e
			}
			if _, e := p.Mutate(context.Background(), actor, "create", "key", map[string]int{"n": 1}, fn); e != nil {
				t.Fatal(e)
			}
			if _, e := p2.Mutate(context.Background(), actor, "create", "key", map[string]int{"n": 1}, fn); e != nil {
				t.Fatal(e)
			}
			if calls != 1 {
				t.Fatal("duplicate mutation")
			}
			if _, e := p2.Mutate(context.Background(), actor, "create", "key", map[string]int{"n": 2}, fn); e == nil {
				t.Fatal("changed payload replayed")
			}
			if _, e := p.GetResource(context.Background(), Principal{ID: "u", Scope: "org:b"}, "once"); e == nil {
				t.Fatal("cross scope read")
			}
		})
	}
}
func TestLeaseCannotResurrect(t *testing.T) {
	m := NewMemory()
	now := time.Now()
	m.Now = func() time.Time { return now }
	ctx := context.Background()
	ok, e := m.Create(ctx, "control:1:1", "epoch1", 10*time.Second)
	if e != nil || !ok {
		t.Fatal(e)
	}
	now = now.Add(11 * time.Second)
	if ok, e = m.Renew(ctx, "control:1:1", "epoch1", 10*time.Second); e != nil || ok {
		t.Fatal("expired consent revived")
	}
	if _, e = m.Get(ctx, "control:1:1"); !errors.Is(e, ErrMissing) {
		t.Fatal(e)
	}
}
func TestRedisLeaseTicketsAndDistributedLimits(t *testing.T) {
	address := os.Getenv("AV_TEST_REDIS_URL")
	if address == "" {
		t.Skip("AV_TEST_REDIS_URL not set")
	}
	r, e := OpenRedis(address)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	r.Prefix = "av-test:" + ID() + ":"
	ctx := context.Background()
	if e = r.Ping(ctx); e != nil {
		t.Fatal(e)
	}
	if ok, e := r.Create(ctx, "lease", "epoch", time.Minute); e != nil || !ok {
		t.Fatal(e)
	}
	if ok, e := r.Renew(ctx, "lease", "wrong", time.Minute); e != nil || ok {
		t.Fatal("wrong epoch")
	}
	_ = r.Delete(ctx, "lease")
	if ok, e := r.Renew(ctx, "lease", "epoch", time.Minute); e != nil || ok {
		t.Fatal("missing lease recreated")
	}
	_ = r.Set(ctx, "ticket", "bound", time.Second)
	if v, e := r.Take(ctx, "ticket"); e != nil || v != "bound" {
		t.Fatal(e)
	}
	if _, e = r.Take(ctx, "ticket"); !errors.Is(e, ErrMissing) {
		t.Fatal("ticket replay")
	}
	for i := 0; i < 3; i++ {
		ok, e := r.Allow(ctx, "limit", 2, time.Second)
		if e != nil || ok != (i < 2) {
			t.Fatal("limit", i, e)
		}
	}
}
