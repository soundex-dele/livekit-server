package control

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"platform-foundation/server/persistence"
	"platform-foundation/server/taskqueue"
)

type Node struct {
	ID               string `json:"id"`
	APIURL           string `json:"api_url"`
	PublicURL        string `json:"public_url"`
	RTCKey           string `json:"rtc_key"`
	RTCSecret        string `json:"rtc_secret"`
	ManagementKey    string `json:"management_key"`
	ManagementSecret string `json:"management_secret"`
	AdmissionSecret  string `json:"admission_secret"`
	Capacity         int    `json:"capacity"`
}
type NodeStatus struct {
	ID          string `json:"node_id"`
	Incarnation string `json:"node_incarnation"`
	Ready       bool   `json:"ready"`
	AVMode      bool   `json:"av_mode"`
}
type MediaCommand struct {
	OperationID          string         `json:"operation_id"`
	NodeID               string         `json:"node_id"`
	NodeIncarnation      string         `json:"node_incarnation"`
	Room                 string         `json:"room"`
	BindingGeneration    int64          `json:"binding_generation"`
	TargetPolicyRevision int64          `json:"target_policy_revision"`
	Method               string         `json:"method"`
	Payload              map[string]any `json:"payload"`
}
type MediaClient interface {
	Status(context.Context, Node) (NodeStatus, error)
	Execute(context.Context, Node, MediaCommand) error
}
type HTTPMedia struct{ Client *http.Client }

var ErrSuperseded = errors.New("media operation superseded")

type mediaFailure struct {
	Status int
	Code   string
}

func (e *mediaFailure) Error() string { return fmt.Sprintf("media status %d (%s)", e.Status, e.Code) }

func sign(key, secret string, claims map[string]any) string {
	claims["iss"] = key
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	raw, _ := json.Marshal(claims)
	body := header + "." + base64.RawURLEncoding.EncodeToString(raw)
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
func (m *HTTPMedia) call(ctx context.Context, n Node, method, path string, in, out any) error {
	var reader io.Reader
	if in != nil {
		raw, e := json.Marshal(in)
		if e != nil {
			return e
		}
		reader = bytes.NewReader(raw)
	}
	r, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(n.APIURL, "/")+path, reader)
	if e != nil {
		return e
	}
	r.Header.Set("Content-Type", "application/json")
	video := map[string]any{"roomList": true, "roomCreate": true, "roomAdmin": true}
	switch v := in.(type) {
	case MediaCommand:
		video["room"] = v.Room
	case map[string]string:
		video["room"] = v["room"]
	case map[string]any:
		if room, ok := v["room"]; ok {
			video["room"] = room
		}
	}
	r.Header.Set("Authorization", "Bearer "+sign(n.ManagementKey, n.ManagementSecret, map[string]any{"nbf": time.Now().Add(-5 * time.Second).Unix(), "exp": time.Now().Add(time.Minute).Unix(), "video": video}))
	client := m.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	resp, e := client.Do(r)
	if e != nil {
		return fmt.Errorf("media request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var failure struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&failure)
		code := "media_error"
		switch failure.Error.Code {
		case "room_ended", "participant_ended", "stale_operation", "binding_generation_mismatch", "node_incarnation_mismatch":
			code = failure.Error.Code
		}
		return &mediaFailure{resp.StatusCode, code}
	}
	if out == nil {
		_, e = io.Copy(io.Discard, io.LimitReader(resp.Body, 8192))
		return e
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(out)
}
func (m *HTTPMedia) Status(c context.Context, n Node) (NodeStatus, error) {
	var s NodeStatus
	e := m.call(c, n, "GET", "/internal/v1/media/node", nil, &s)
	return s, e
}
func (m *HTTPMedia) Execute(c context.Context, n Node, v MediaCommand) error {
	e := m.call(c, n, "POST", "/internal/v1/media/operations", v, nil)
	var failure *mediaFailure
	if errors.As(e, &failure) && failure.Status == 409 {
		switch failure.Code {
		case "room_ended", "participant_ended", "stale_operation", "binding_generation_mismatch":
			return ErrSuperseded
		}
	}
	return e
}
func (m *HTTPMedia) Published(ctx context.Context, n Node, room string) (bool, error) {
	var result struct {
		Participants []struct {
			Tracks []json.RawMessage `json:"tracks"`
		} `json:"participants"`
	}
	if e := m.call(ctx, n, "POST", "/twirp/livekit.RoomService/ListParticipants", map[string]string{"room": room}, &result); e != nil {
		return false, e
	}
	for _, p := range result.Participants {
		if len(p.Tracks) > 0 {
			return true, nil
		}
	}
	return false, nil
}
func (m *HTTPMedia) Present(ctx context.Context, n Node, room string) (map[string]bool, error) {
	var result struct {
		Participants []struct {
			Identity string `json:"identity"`
		} `json:"participants"`
	}
	if e := m.call(ctx, n, "POST", "/twirp/livekit.RoomService/ListParticipants", map[string]string{"room": room}, &result); e != nil {
		return nil, e
	}
	out := map[string]bool{}
	for _, p := range result.Participants {
		out[p.Identity] = true
	}
	return out, nil
}
func (s *Platform) Present(ctx context.Context, scope, resource, purpose string) (map[string]bool, error) {
	b, e := scanBinding(s.Store.DB.QueryRowContext(ctx, s.Store.Q("SELECT "+bindingCols+" FROM av_room_bindings WHERE scope=? AND resource_id=? AND purpose=? AND state='ready' ORDER BY generation DESC LIMIT 1"), scope, resource, purpose))
	if e != nil {
		return nil, e
	}
	n, ok := s.node(b.Node)
	if !ok {
		return nil, Fail(503, "media_node_unavailable")
	}
	if observer, ok := s.Media.(interface {
		Present(context.Context, Node, string) (map[string]bool, error)
	}); ok {
		return observer.Present(ctx, n, b.Room)
	}
	return nil, errors.New("media presence observation unavailable")
}
func (s *Platform) Published(ctx context.Context, scope, resource, purpose string) (bool, error) {
	b, e := scanBinding(s.Store.DB.QueryRowContext(ctx, s.Store.Q("SELECT "+bindingCols+" FROM av_room_bindings WHERE scope=? AND resource_id=? AND purpose=? AND state='ready' ORDER BY generation DESC LIMIT 1"), scope, resource, purpose))
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	n, ok := s.node(b.Node)
	if !ok {
		return false, Fail(503, "media_node_unavailable")
	}
	if observer, ok := s.Media.(interface {
		Published(context.Context, Node, string) (bool, error)
	}); ok {
		return observer.Published(ctx, n, b.Room)
	}
	return false, errors.New("media publication observation unavailable")
}
func (s *Platform) node(id string) (Node, bool) {
	for _, n := range s.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

// Node discovery never runs in a resource transaction, and a process restart
// never takes over another SFU's incarnation or erases valid room bindings.
func (s *Platform) SyncNodes(ctx context.Context) error {
	var first error
	for _, n := range s.Nodes {
		status, e := s.Media.Status(ctx, n)
		healthy := 0
		if e == nil && status.ID == n.ID && status.Incarnation != "" && status.Ready && status.AVMode {
			healthy = 1
		} else if e == nil {
			e = errors.New("media identity or capability mismatch")
		}
		if e != nil && first == nil {
			first = e
		}
		err := s.Store.Within(ctx, "app", func(t *persistence.Tx) error {
			if healthy == 0 {
				_, e := t.Exec(ctx, "UPDATE av_media_nodes SET healthy=0,updated=? WHERE id=?", s.Now().UnixMilli(), n.ID)
				return e
			}
			_, e := t.Exec(ctx, "INSERT INTO av_media_nodes(id,incarnation,enabled,healthy,capacity,updated) VALUES(?,?,1,1,?,?) ON CONFLICT(id) DO UPDATE SET incarnation=excluded.incarnation,enabled=1,healthy=1,capacity=excluded.capacity,updated=excluded.updated", n.ID, status.Incarnation, n.Capacity, s.Now().UnixMilli())
			return e
		})
		if err != nil {
			return err
		}
	}
	return first
}

type binding struct {
	ID, Scope, Resource, Purpose, Node, Incarnation, Room, State string
	Generation, Revision                                         int64
}

const bindingCols = "id,scope,resource_id,purpose,node_id,node_incarnation,room,state,generation,revision"

func scanBinding(row interface{ Scan(...any) error }) (binding, error) {
	var b binding
	e := row.Scan(&b.ID, &b.Scope, &b.Resource, &b.Purpose, &b.Node, &b.Incarnation, &b.Room, &b.State, &b.Generation, &b.Revision)
	return b, e
}
func (s *Platform) ensureBinding(ctx context.Context, t *persistence.Tx, r Resource, purpose string) (binding, error) {
	b, e := scanBinding(t.Row(ctx, "SELECT "+bindingCols+" FROM av_room_bindings WHERE scope=? AND resource_id=? AND purpose=? ORDER BY generation DESC LIMIT 1", t.Scope, r.ID, purpose))
	generation := int64(1)
	if e == nil {
		generation = b.Generation + 1
		var inc string
		var healthy int
		e = t.Row(ctx, "SELECT incarnation,healthy FROM av_media_nodes WHERE id=?", b.Node).Scan(&inc, &healthy)
		_, configured := s.node(b.Node)
		if e == nil && configured && inc == b.Incarnation && healthy == 1 && (b.State == "ready" || b.State == "preparing") {
			return b, nil
		}
		if b.State != "ended" {
			if _, e = t.Exec(ctx, "UPDATE av_room_bindings SET state='unavailable' WHERE id=?", b.ID); e != nil {
				return b, e
			}
			if _, e = t.Exec(ctx, "UPDATE av_rtc_grants SET status='revoked' WHERE binding_id=?", b.ID); e != nil {
				return b, e
			}
		}
	} else if !errors.Is(e, sql.ErrNoRows) {
		return b, e
	}
	// The node UPDATE serializes reservations across organizations, so a node
	// cannot be overbooked by two otherwise independent scope transactions.
	rows, e := t.SQL.QueryContext(ctx, t.Store.Q("SELECT id,incarnation FROM av_media_nodes WHERE enabled=1 AND healthy=1 AND updated>? ORDER BY id"), s.Now().Add(-30*time.Second).UnixMilli())
	if e != nil {
		return b, e
	}
	type candidate struct{ id, inc string }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if e = rows.Scan(&c.id, &c.inc); e != nil {
			rows.Close()
			return b, e
		}
		candidates = append(candidates, c)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return b, e
	}
	chosen := candidate{}
	for _, c := range candidates {
		if _, configured := s.node(c.id); !configured {
			continue
		}
		res, e := t.Exec(ctx, "UPDATE av_media_nodes SET updated=updated WHERE id=? AND healthy=1 AND incarnation=?", c.id, c.inc)
		if e != nil {
			return b, e
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			continue
		}
		var capacity, used int
		if e = t.Row(ctx, "SELECT capacity FROM av_media_nodes WHERE id=?", c.id).Scan(&capacity); e != nil {
			return b, e
		}
		if e = t.Row(ctx, "SELECT COUNT(*) FROM av_room_bindings WHERE node_id=? AND node_incarnation=? AND state IN ('ready','preparing')", c.id, c.inc).Scan(&used); e != nil {
			return b, e
		}
		if used < capacity {
			chosen = c
			break
		}
	}
	if chosen.id == "" {
		return b, Fail(503, "media_capacity_unavailable")
	}
	b = binding{ID: ID(), Scope: r.Scope, Resource: r.ID, Purpose: purpose, Node: chosen.id, Incarnation: chosen.inc, Room: "av_" + ID(), State: "preparing", Generation: generation, Revision: 1}
	_, e = t.Exec(ctx, "INSERT INTO av_room_bindings("+bindingCols+",created) VALUES(?,?,?,?,?,?,?,?,?,?,?)", b.ID, b.Scope, b.Resource, b.Purpose, b.Node, b.Incarnation, b.Room, b.State, b.Generation, b.Revision, s.Now().UnixMilli())
	if e != nil {
		return b, e
	}
	_, e = s.operation(ctx, t, b, "", "CreateRoom", map[string]any{"name": b.Room, "emptyTimeout": 300, "maxParticipants": 1000})
	return b, e
}
func (s *Platform) Grant(ctx context.Context, t *persistence.Tx, r Resource, subject, role, purpose string, c Capabilities) (RTCGrant, error) {
	var g RTCGrant
	if r.Scope != t.Scope || r.State == "ended" || r.State == "expired" || r.State == "failed" {
		return g, Fail(409, "resource_ended")
	}
	var version int64
	var state string
	if e := t.Row(ctx, "SELECT policy_version,state FROM av_participants WHERE scope=? AND resource_id=? AND subject_id=?", t.Scope, r.ID, subject).Scan(&version, &state); e != nil {
		return g, e
	}
	if state != "accepted" && state != "active" {
		return g, Fail(403, "participant_not_admitted")
	}
	b, e := s.ensureBinding(ctx, t, r, purpose)
	if e != nil {
		return g, e
	}
	raw, e := json.Marshal(c)
	if e != nil {
		return g, e
	}
	e = t.Row(ctx, "SELECT id,identity,binding_id,policy_version FROM av_rtc_grants WHERE scope=? AND resource_id=? AND subject_id=? AND binding_id=? AND policy_version=? AND capabilities=? AND status='active' AND expires>? ORDER BY created DESC LIMIT 1", t.Scope, r.ID, subject, b.ID, version, string(raw), s.Now().Add(time.Minute).Unix()).Scan(&g.ID, &g.Identity, &g.BindingID, &g.PolicyVersion)
	if e == nil {
		return g, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return g, e
	}
	g = RTCGrant{ID: ID(), Identity: "p_" + ID(), BindingID: b.ID, PolicyVersion: version}
	_, e = t.Exec(ctx, "INSERT INTO av_rtc_grants(id,scope,resource_id,subject_id,role,binding_id,identity,policy_version,capabilities,status,expires,created) VALUES(?,?,?,?,?,?,?,?,?,'active',?,?)", g.ID, t.Scope, r.ID, subject, role, b.ID, g.Identity, version, string(raw), s.Now().Add(24*time.Hour).Unix(), s.Now().UnixMilli())
	return g, e
}
func (s *Platform) operation(ctx context.Context, t *persistence.Tx, b binding, subject, method string, payload map[string]any) (string, error) {
	id := ID()
	raw, _ := json.Marshal(payload)
	now := s.Now().UnixMilli()
	if _, e := t.Exec(ctx, "UPDATE av_room_bindings SET revision=revision+1 WHERE id=?", b.ID); e != nil {
		return "", e
	}
	var revision int64
	if e := t.Row(ctx, "SELECT revision FROM av_room_bindings WHERE id=?", b.ID).Scan(&revision); e != nil {
		return "", e
	}
	if _, e := t.Exec(ctx, "INSERT INTO av_media_operations(id,scope,resource_id,binding_id,subject_id,action,revision,payload,state,created,updated) VALUES(?,?,?,?,?,?,?,?,'queued',?,?)", id, t.Scope, b.Resource, b.ID, subject, method, revision, string(raw), now, now); e != nil {
		return "", e
	}
	body, _ := json.Marshal(map[string]string{"operation_id": id})
	job, e := s.Queue.Submit(ctx, t, "system", taskqueue.Spec{Kind: "job", Type: "av.media", Key: id, Payload: body, MaxAttempts: 10})
	if e == nil {
		_, e = t.Exec(ctx, "UPDATE av_media_operations SET job_id=? WHERE scope=? AND id=?", job, t.Scope, id)
	}
	return id, e
}
func (s *Platform) Revoke(ctx context.Context, t *persistence.Tx, resource, subject string) error {
	query := "SELECT g.id,g.identity,g.binding_id,g.subject_id FROM av_rtc_grants g WHERE g.scope=? AND g.resource_id=? AND g.status='active'"
	args := []any{t.Scope, resource}
	if subject != "" {
		query += " AND g.subject_id=?"
		args = append(args, subject)
	}
	rows, e := t.SQL.QueryContext(ctx, t.Store.Q(query), args...)
	if e != nil {
		return e
	}
	type target struct{ id, identity, binding, subject string }
	var targets []target
	for rows.Next() {
		var v target
		if e = rows.Scan(&v.id, &v.identity, &v.binding, &v.subject); e != nil {
			rows.Close()
			return e
		}
		targets = append(targets, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, g := range targets {
		if _, e = t.Exec(ctx, "UPDATE av_rtc_grants SET status='revoked' WHERE id=?", g.id); e != nil {
			return e
		}
		b, e := scanBinding(t.Row(ctx, "SELECT "+bindingCols+" FROM av_room_bindings WHERE id=?", g.binding))
		if e != nil {
			return e
		}
		if _, e = s.operation(ctx, t, b, g.subject, "RemoveParticipant", map[string]any{"room": b.Room, "identity": g.identity}); e != nil {
			return e
		}
	}
	if subject == "" {
		rows, e = t.SQL.QueryContext(ctx, t.Store.Q("SELECT "+bindingCols+" FROM av_room_bindings WHERE scope=? AND resource_id=? AND state IN ('ready','preparing')"), t.Scope, resource)
		if e != nil {
			return e
		}
		var all []binding
		for rows.Next() {
			b, err := scanBinding(rows)
			if err != nil {
				rows.Close()
				return err
			}
			all = append(all, b)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		for _, b := range all {
			if _, e = t.Exec(ctx, "UPDATE av_room_bindings SET state='ending' WHERE id=?", b.ID); e != nil {
				return e
			}
			if _, e = s.operation(ctx, t, b, "", "DeleteRoom", map[string]any{"room": b.Room}); e != nil {
				return e
			}
		}
	}
	return nil
}
