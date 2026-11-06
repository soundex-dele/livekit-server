package control

import (
	"context"
	"errors"
	"github.com/redis/go-redis/v9"
	"sync"
	"time"
)

type Redis struct {
	Client *redis.Client
	Prefix string
}

func OpenRedis(address string) (*Redis, error) {
	o, e := redis.ParseURL(address)
	if e != nil {
		return nil, errors.New("invalid AV_REDIS_URL")
	}
	o.DialTimeout = 2 * time.Second
	o.ReadTimeout = 2 * time.Second
	o.WriteTimeout = 2 * time.Second
	return &Redis{redis.NewClient(o), "av:"}, nil
}
func (r *Redis) key(k string) string            { return r.Prefix + k }
func (r *Redis) Ping(ctx context.Context) error { return r.Client.Ping(ctx).Err() }
func (r *Redis) Close() error                   { return r.Client.Close() }
func (r *Redis) Set(c context.Context, k, v string, t time.Duration) error {
	return r.Client.Set(c, r.key(k), v, t).Err()
}
func (r *Redis) Get(c context.Context, k string) (string, error) {
	v, e := r.Client.Get(c, r.key(k)).Result()
	if errors.Is(e, redis.Nil) {
		e = ErrMissing
	}
	return v, e
}
func (r *Redis) Delete(c context.Context, k string) error { return r.Client.Del(c, r.key(k)).Err() }
func (r *Redis) Create(c context.Context, k, v string, t time.Duration) (bool, error) {
	return r.Client.SetNX(c, r.key(k), v, t).Result()
}

var renewScript = redis.NewScript(`if redis.call('GET',KEYS[1]) ~= ARGV[1] then return 0 end; redis.call('PEXPIRE',KEYS[1],ARGV[2]); return 1`)

func (r *Redis) Renew(c context.Context, k, v string, t time.Duration) (bool, error) {
	n, e := renewScript.Run(c, r.Client, []string{r.key(k)}, v, t.Milliseconds()).Int()
	return n == 1, e
}
func (r *Redis) Take(c context.Context, k string) (string, error) {
	v, e := takeScript.Run(c, r.Client, []string{r.key(k)}).Text()
	if errors.Is(e, redis.Nil) {
		e = ErrMissing
	}
	return v, e
}

var takeScript = redis.NewScript(`local v=redis.call('GET',KEYS[1]); if v then redis.call('DEL',KEYS[1]) end; return v`)

var limitScript = redis.NewScript(`local n=redis.call('INCR',KEYS[1]); if n==1 then redis.call('PEXPIRE',KEYS[1],ARGV[1]) end; return n`)

func (r *Redis) Allow(c context.Context, k string, n int, t time.Duration) (bool, error) {
	v, e := limitScript.Run(c, r.Client, []string{r.key("limit:" + k)}, t.Milliseconds()).Int()
	return v <= n, e
}
func (r *Redis) Publish(c context.Context, k, v string) error {
	return r.Client.Publish(c, r.key(k), v).Err()
}
func (r *Redis) Subscribe(ctx context.Context, k string) (<-chan string, error) {
	sub := r.Client.Subscribe(ctx, r.key(k))
	if _, e := sub.Receive(ctx); e != nil {
		sub.Close()
		return nil, e
	}
	out := make(chan string, 16)
	go func() {
		defer close(out)
		defer sub.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case m, ok := <-sub.Channel():
				if !ok {
					return
				}
				select {
				case out <- m.Payload:
				default: /* hints may coalesce; clients replay from SQL */
				}
			}
		}
	}()
	return out, nil
}

// Memory is an explicit test/development adapter, never a production fallback.
type Memory struct {
	mu          sync.Mutex
	values      map[string]memoryValue
	subscribers map[string]map[chan string]bool
	Now         func() time.Time
}
type memoryValue struct {
	value string
	until time.Time
	count int
}

func NewMemory() *Memory {
	return &Memory{values: map[string]memoryValue{}, subscribers: map[string]map[chan string]bool{}, Now: time.Now}
}
func (m *Memory) Ping(context.Context) error { return nil }
func (m *Memory) current(k string) (memoryValue, bool) {
	v, ok := m.values[k]
	if ok && !m.Now().Before(v.until) {
		delete(m.values, k)
		ok = false
	}
	return v, ok
}
func (m *Memory) Set(c context.Context, k, v string, t time.Duration) error {
	if e := c.Err(); e != nil {
		return e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[k] = memoryValue{value: v, until: m.Now().Add(t)}
	return nil
}
func (m *Memory) Get(c context.Context, k string) (string, error) {
	if e := c.Err(); e != nil {
		return "", e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.current(k)
	if !ok {
		return "", ErrMissing
	}
	return v.value, nil
}
func (m *Memory) Delete(c context.Context, k string) error {
	if e := c.Err(); e != nil {
		return e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.values, k)
	return nil
}
func (m *Memory) Create(c context.Context, k, v string, t time.Duration) (bool, error) {
	if e := c.Err(); e != nil {
		return false, e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.current(k); ok {
		return false, nil
	}
	m.values[k] = memoryValue{value: v, until: m.Now().Add(t)}
	return true, nil
}
func (m *Memory) Renew(c context.Context, k, v string, t time.Duration) (bool, error) {
	if e := c.Err(); e != nil {
		return false, e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.current(k)
	if !ok || old.value != v {
		return false, nil
	}
	old.until = m.Now().Add(t)
	m.values[k] = old
	return true, nil
}
func (m *Memory) Take(c context.Context, k string) (string, error) {
	if e := c.Err(); e != nil {
		return "", e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.current(k)
	delete(m.values, k)
	if !ok {
		return "", ErrMissing
	}
	return v.value, nil
}
func (m *Memory) Allow(c context.Context, k string, n int, t time.Duration) (bool, error) {
	if e := c.Err(); e != nil {
		return false, e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k = "limit:" + k
	v, ok := m.current(k)
	if !ok {
		v.until = m.Now().Add(t)
	}
	v.count++
	m.values[k] = v
	return v.count <= n, nil
}
func (m *Memory) Publish(c context.Context, k, v string) error {
	if e := c.Err(); e != nil {
		return e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for ch := range m.subscribers[k] {
		select {
		case ch <- v:
		default:
		}
	}
	return nil
}
func (m *Memory) Subscribe(ctx context.Context, k string) (<-chan string, error) {
	m.mu.Lock()
	ch := make(chan string, 16)
	if m.subscribers[k] == nil {
		m.subscribers[k] = map[chan string]bool{}
	}
	m.subscribers[k][ch] = true
	m.mu.Unlock()
	go func() { <-ctx.Done(); m.mu.Lock(); delete(m.subscribers[k], ch); close(ch); m.mu.Unlock() }()
	return ch, nil
}
