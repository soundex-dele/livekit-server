package control

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"platform-foundation/server/persistence"
	"platform-foundation/server/taskqueue"
	"time"
)

func (s *Platform) Notify(ctx context.Context, j taskqueue.Job, _ func(int) error) (json.RawMessage, error) {
	var event struct {
		ResourceID string `json:"resource_id"`
	}
	if e := json.Unmarshal(j.Payload, &event); e != nil {
		return nil, e
	}
	e := s.Ephemeral.Publish(ctx, "events:"+j.Scope+":"+event.ResourceID, string(j.Payload))
	return json.RawMessage(`{"delivered":true}`), e
}
func (s *Platform) Execute(ctx context.Context, j taskqueue.Job, _ func(int) error) (json.RawMessage, error) {
	var spec struct {
		ID string `json:"operation_id"`
	}
	if e := json.Unmarshal(j.Payload, &spec); e != nil {
		return nil, e
	}
	var bindingID, method, raw, state string
	var revision int64
	e := s.Store.DB.QueryRowContext(ctx, s.Store.Q("SELECT binding_id,action,revision,payload,state FROM av_media_operations WHERE scope=? AND id=?"), j.Scope, spec.ID).Scan(&bindingID, &method, &revision, &raw, &state)
	if e != nil {
		return nil, e
	}
	if state == "succeeded" || state == "superseded" {
		return json.RawMessage(`{"done":true}`), nil
	}
	b, e := scanBinding(s.Store.DB.QueryRowContext(ctx, s.Store.Q("SELECT "+bindingCols+" FROM av_room_bindings WHERE scope=? AND id=?"), j.Scope, bindingID))
	if e != nil {
		return nil, e
	}
	var payload map[string]any
	if e = json.Unmarshal([]byte(raw), &payload); e != nil {
		return nil, e
	}
	var incarnation string
	e = s.Store.DB.QueryRowContext(ctx, s.Store.Q("SELECT incarnation FROM av_media_nodes WHERE id=?"), b.Node).Scan(&incarnation)
	if e != nil {
		return nil, e
	}
	// A create intent cannot resurrect an ended resource. Removal commands use
	// unique immutable room names, and the SFU additionally fences revisions.
	var resourceState string
	if e = s.Store.DB.QueryRowContext(ctx, s.Store.Q("SELECT state FROM av_resources WHERE scope=? AND id=?"), j.Scope, b.Resource).Scan(&resourceState); e != nil {
		return nil, e
	}
	skip := incarnation != b.Incarnation || method == "CreateRoom" && (b.State != "preparing" || resourceState == "ended" || resourceState == "expired" || resourceState == "failed")
	if !skip {
		n, ok := s.node(b.Node)
		if !ok {
			return nil, errors.New("media node missing from configuration")
		}
		command := MediaCommand{OperationID: spec.ID, NodeID: b.Node, NodeIncarnation: b.Incarnation, Room: b.Room, BindingGeneration: b.Generation, TargetPolicyRevision: revision, Method: method, Payload: payload}
		if e = s.Media.Execute(ctx, n, command); e != nil {
			if errors.Is(e, ErrSuperseded) {
				skip = true
			} else {
				_, _ = s.Store.DB.ExecContext(ctx, s.Store.Q("UPDATE av_media_operations SET state='retrying',updated=? WHERE id=? AND state NOT IN ('succeeded','superseded')"), s.Now().UnixMilli(), spec.ID)
				return nil, e
			}
		}
	}
	e = s.Store.Within(ctx, j.Scope, func(t *persistence.Tx) error {
		// A worker which lost its fence must not publish success or grant access.
		if e := s.Queue.CheckLease(ctx, t, j); e != nil {
			return e
		}
		next := "succeeded"
		if skip {
			next = "superseded"
		}
		if _, e := t.Exec(ctx, "UPDATE av_media_operations SET state=?,updated=? WHERE id=?", next, s.Now().UnixMilli(), spec.ID); e != nil {
			return e
		}
		if method == "CreateRoom" && !skip {
			_, e := t.Exec(ctx, "UPDATE av_room_bindings SET state='ready' WHERE id=? AND state='preparing'", b.ID)
			return e
		}
		if method == "DeleteRoom" {
			_, e := t.Exec(ctx, "UPDATE av_room_bindings SET state='ended' WHERE id=?", b.ID)
			return e
		}
		return nil
	})
	return json.RawMessage(`{"done":true}`), e
}
func (s *Platform) Operation(ctx context.Context, p Principal, id string) (map[string]any, error) {
	var resource, state, action string
	var updated int64
	e := s.Store.DB.QueryRowContext(ctx, s.Store.Q("SELECT resource_id,state,action,updated FROM av_media_operations WHERE scope=? AND id=?"), p.Scope, id).Scan(&resource, &state, &action, &updated)
	if e != nil {
		return nil, Fail(404, "operation_not_found")
	}
	if e = s.CanObserve(ctx, p, resource); e != nil {
		return nil, e
	}
	return map[string]any{"id": id, "resource_id": resource, "state": state, "action": action, "updated": updated}, nil
}
func (s *Platform) Operations(ctx context.Context, p Principal, resource string) ([]map[string]any, error) {
	if e := s.CanObserve(ctx, p, resource); e != nil {
		return nil, e
	}
	rows, e := s.Store.DB.QueryContext(ctx, s.Store.Q("SELECT id,state,action,updated FROM av_media_operations WHERE scope=? AND resource_id=? ORDER BY created DESC,id LIMIT 200"), p.Scope, resource)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, state, action string
		var updated int64
		if e = rows.Scan(&id, &state, &action, &updated); e != nil {
			return nil, e
		}
		out = append(out, map[string]any{"id": id, "resource_id": resource, "state": state, "action": action, "updated": updated})
	}
	return out, rows.Err()
}
func (s *Platform) Reconcile(ctx context.Context) error {
	if e := s.SyncNodes(ctx); e != nil {
		slog.Warn("media discovery incomplete", "error", e)
	}
	// Task exhaustion remains visible; it must not masquerade as completion.
	rows, e := s.Store.DB.QueryContext(ctx, "SELECT id,scope FROM av_media_operations WHERE state IN ('queued','retrying') ORDER BY updated LIMIT 200")
	if e != nil {
		return e
	}
	type entry struct{ id, scope string }
	var pending []entry
	for rows.Next() {
		var v entry
		if e = rows.Scan(&v.id, &v.scope); e != nil {
			rows.Close()
			return e
		}
		pending = append(pending, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, v := range pending {
		job, e := s.Queue.Get(ctx, v.scope, Hash(v.scope+"|system|job|"+v.id))
		if e != nil {
			return e
		}
		if job.Status == "failed" {
			if _, e = s.Store.DB.ExecContext(ctx, s.Store.Q("UPDATE av_media_operations SET state='failed',updated=? WHERE id=? AND state IN ('queued','retrying')"), s.Now().UnixMilli(), v.id); e != nil {
				return e
			}
		}
	}
	return nil
}
func (s *Platform) RunMaintenance(ctx context.Context, extra func(context.Context) error) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	nextNodes := time.Time{}
	for {
		if !s.Now().Before(nextNodes) {
			if e := s.Reconcile(ctx); e != nil && ctx.Err() == nil {
				slog.Error("AV reconciliation failed", "error", e)
			}
			nextNodes = s.Now().Add(10 * time.Second)
		}
		if extra != nil {
			if e := extra(ctx); e != nil && ctx.Err() == nil {
				slog.Error("business maintenance failed", "error", e)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
