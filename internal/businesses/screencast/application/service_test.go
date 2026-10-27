package application

import (
	"av-platform/business/internal/businesses/screencast/domain"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type memory struct {
	rows   map[string]domain.Session
	broken bool
}

func (m *memory) Load(context.Context) ([]domain.Session, error) {
	var a []domain.Session
	for _, v := range m.rows {
		a = append(a, v)
	}
	return a, nil
}
func (m *memory) Save(_ context.Context, v domain.Session) error {
	if m.broken {
		return errors.New("disk")
	}
	m.rows[v.ID] = v
	return nil
}

type fakeMedia struct {
	deletes int
	broken  bool
	roles   []string
}

func (m *fakeMedia) Connect(_ context.Context, v domain.Session, r string) (map[string]any, error) {
	m.roles = append(m.roles, r)
	return map[string]any{"room": v.Room}, nil
}
func (m *fakeMedia) Delete(context.Context, string) error {
	m.deletes++
	if m.broken {
		return errors.New("offline")
	}
	return nil
}
func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	db := &memory{rows: map[string]domain.Session{}}
	media := &fakeMedia{}
	s, e := New(ctx, db, media, func() time.Time { return now })
	if e != nil {
		t.Fatal(e)
	}
	exec := func(c Command) map[string]any {
		t.Helper()
		v, e := s.Execute(ctx, c)
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	a := exec(Command{Action: "create"})
	id := a["id"].(string)
	receiver := a["credential"].(string)
	code := a["code"].(string)
	if len(code) != 10 || db.rows[id].ReceiverHash == receiver || db.rows[id].CodeHash == code {
		t.Fatal("plaintext or bad code")
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	var sender string
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, e := s.Execute(ctx, Command{Action: "redeem", Code: code})
			if e == nil {
				mu.Lock()
				winners++
				sender = v["credential"].(string)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Fatal(winners)
	}
	if _, e = s.Execute(ctx, Command{Action: "get", ID: id, Credential: "bad"}); e == nil {
		t.Fatal("unauthorized accepted")
	}
	for _, key := range []string{receiver, sender} {
		exec(Command{Action: "connect", ID: id, Credential: key})
		exec(Command{Action: "heartbeat", ID: id, Credential: key, Connected: true})
	}
	if media.roles[0] != "receiver" || media.roles[1] != "sender" || db.rows[id].State != "streaming" {
		t.Fatal("role/state")
	}
	now = now.Add(29 * time.Second)
	exec(Command{Action: "heartbeat", ID: id, Credential: sender, Connected: true})
	s.Sweep(ctx)
	if db.rows[id].Terminal() {
		t.Fatal("early expiry")
	}
	now = now.Add(time.Second)
	s.Sweep(ctx)
	if db.rows[id].Reason != "heartbeat_timeout" {
		t.Fatal(db.rows[id])
	}
	media.broken = true
	s.Sweep(ctx)
	if !db.rows[id].Cleanup {
		t.Fatal("lost cleanup")
	}
	media.broken = false
	now = now.Add(66 * time.Second)
	s.Sweep(ctx)
	if db.rows[id].Cleanup {
		t.Fatal("cleanup not completed")
	}
	exec(Command{Action: "stop", ID: id, Credential: receiver})
	exec(Command{Action: "stop", ID: id, Credential: sender})
	a = exec(Command{Action: "create"})
	now = now.Add(5 * time.Minute)
	if _, e = s.Execute(ctx, Command{Action: "redeem", Code: a["code"].(string)}); e == nil {
		t.Fatal("expired accepted")
	}
	a = exec(Command{Action: "create"})
	exec(Command{Action: "redeem", Code: a["code"].(string)})
	now = now.Add(30 * time.Second)
	s.Sweep(ctx)
	if db.rows[a["id"].(string)].Reason != "connect_timeout" {
		t.Fatal("connection timeout")
	}
	a = exec(Command{Action: "create"})
	exec(Command{Action: "redeem", Code: a["code"].(string)})
	_, e = New(ctx, db, media, func() time.Time { return now })
	if e != nil || db.rows[a["id"].(string)].Reason != "service_restart" {
		t.Fatal("restart")
	}
}
func TestStorageFailureAndRateLimit(t *testing.T) {
	ctx := context.Background()
	db := &memory{rows: map[string]domain.Session{}}
	s, _ := New(ctx, db, &fakeMedia{}, nil)
	a, _ := s.Execute(ctx, Command{Action: "create"})
	db.broken = true
	if _, e := s.Execute(ctx, Command{Action: "redeem", Code: a["code"].(string)}); e == nil {
		t.Fatal("disk failure accepted")
	}
	db.broken = false
	if _, e := s.Execute(ctx, Command{Action: "redeem", Code: a["code"].(string)}); e != nil {
		t.Fatal("failed commit consumed code")
	}
	for i := 0; i < 9; i++ {
		if _, e := s.Execute(ctx, Command{Action: "create"}); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := s.Execute(ctx, Command{Action: "create"}); e == nil {
		t.Fatal("no rate limit")
	}
}
