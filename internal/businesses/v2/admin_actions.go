package v2

import (
	"av-platform/business/internal/platform/control"
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"platform-foundation/server/persistence"
)

type AdminAction struct {
	Action  string `json:"action"`
	Reason  string `json:"reason"`
	Target  string `json:"target,omitempty"`
	Version int64  `json:"version,omitempty"`
}

type AdminActionResult struct {
	Accepted     bool     `json:"accepted"`
	ActionID     string   `json:"action_id"`
	ID           string   `json:"id"`
	State        string   `json:"state"`
	Version      int64    `json:"version"`
	OperationIDs []string `json:"operation_ids"`
}

func operationIDsTx(ctx context.Context, t *persistence.Tx, resource string) ([]string, error) {
	rows, e := t.SQL.QueryContext(ctx, t.Store.Q("SELECT id FROM av_media_operations WHERE scope=? AND resource_id=? ORDER BY created,id"), t.Scope, resource)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			return nil, e
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// AdminAct never impersonates a room host and never grants media access to the
// operator. Authorization remains separate from participant-side commands.
func (s *Service) AdminAct(ctx context.Context, p control.Principal, id string, c AdminAction, key string) (any, error) {
	if e := adminAllowed(p, false); e != nil {
		return nil, e
	}
	c.Reason = strings.TrimSpace(c.Reason)
	if c.Reason == "" || utf8.RuneCountInString(c.Reason) > 1000 || len(c.Target) > 128 || c.Version < 0 {
		return nil, control.Fail(400, "invalid_admin_action")
	}
	if key == "" || len(key) > 128 {
		return nil, control.Fail(400, "idempotency_key_required")
	}
	allowed := map[string]bool{"end": true, "cancel": true, "revoke-control": true, "kick": true, "mute": true, "request-unmute": true, "chat-mute": true, "chat-unmute": true, "stage-remove": true}
	if !allowed[c.Action] {
		return nil, control.Fail(400, "invalid_admin_action")
	}
	var leases []controlGrant
	out, e := s.P.Mutate(ctx, p, "admin:"+id+":"+c.Action, key, c, func(t *persistence.Tx) (any, error) {
		r, e := s.P.ResourceTx(ctx, t, id)
		if e != nil {
			return nil, e
		}
		if businessForKind(r.Kind) == "" {
			return nil, control.Fail(404, "resource_not_found")
		}
		if c.Version != 0 && c.Version != r.Version {
			return nil, control.Fail(409, "version_conflict")
		}
		resources := []string{id}
		if r.Kind == "broadcast" && c.Action == "end" {
			b, x := broadcastTx(ctx, t, id)
			if x != nil {
				return nil, x
			}
			if b.PK != "" {
				var left, right string
				if x = t.Row(ctx, "SELECT left_id,right_id FROM av_live_pk WHERE scope=? AND id=?", t.Scope, b.PK).Scan(&left, &right); x != nil {
					return nil, x
				}
				for _, other := range []string{left, right} {
					if other != id {
						resources = append(resources, other)
					}
				}
			}
		}
		collect := func() ([]string, error) {
			ids := []string{}
			for _, resource := range resources {
				part, x := operationIDsTx(ctx, t, resource)
				if x != nil {
					return nil, x
				}
				ids = append(ids, part...)
			}
			return ids, nil
		}
		before, e := collect()
		if e != nil {
			return nil, e
		}
		switch c.Action {
		case "end", "cancel":
			if c.Target != "" {
				return nil, control.Fail(400, "invalid_target")
			}
			if c.Action == "cancel" && (r.Kind != "meeting" || (r.State != "scheduled" && r.State != "cancelled")) {
				return nil, control.Fail(409, "not_scheduled")
			}
			if r.Kind == "assist" {
				leases, e = s.adminRevokeControl(ctx, t, p, r)
				if e != nil {
					return nil, e
				}
			}
			if r.Kind == "broadcast" {
				b, x := broadcastTx(ctx, t, id)
				if x != nil {
					return nil, x
				}
				_, e = s.endBroadcast(ctx, t, p, r, b)
			} else {
				state := "ended"
				if r.Kind == "meeting" && (r.State == "scheduled" || r.State == "cancelled") {
					state = "cancelled"
				}
				_, e = s.finish(ctx, t, p, r, state)
			}
		case "revoke-control":
			if r.Kind != "assist" || c.Target != "" {
				return nil, control.Fail(400, "invalid_admin_action")
			}
			if isTerminal(r.State) {
				return nil, control.Fail(409, "session_ended")
			}
			leases, e = s.adminRevokeControl(ctx, t, p, r)
			if e == nil && len(leases) > 0 {
				r.State = "paired"
				r.Updated = s.now().UnixMilli()
				e = s.P.UpdateResource(ctx, t, r)
			}
		default:
			if isTerminal(r.State) {
				return nil, control.Fail(409, "session_ended")
			}
			if c.Target == "" {
				return nil, control.Fail(400, "target_required")
			}
			if r.Kind == "meeting" {
				e = s.adminMeetingMember(ctx, t, p, r, c)
			} else if r.Kind == "broadcast" {
				e = s.adminLiveMember(ctx, t, p, r, c)
			} else {
				e = control.Fail(400, "invalid_admin_action")
			}
		}
		if e != nil {
			return nil, e
		}
		after, e := collect()
		if e != nil {
			return nil, e
		}
		prior := map[string]bool{}
		for _, id := range before {
			prior[id] = true
		}
		operations := []string{}
		for _, id := range after {
			if !prior[id] {
				operations = append(operations, id)
			}
		}
		// Repeating a completed end exposes the existing cleanup, not fresh jobs.
		if (c.Action == "end" || c.Action == "cancel") && len(operations) == 0 {
			operations = after
		}
		r, e = s.P.ResourceTx(ctx, t, id)
		if e != nil {
			return nil, e
		}
		result := AdminActionResult{Accepted: true, ActionID: control.ID(), ID: id, State: r.State, Version: r.Version, OperationIDs: operations}
		raw, e := json.Marshal(operations)
		if e != nil {
			return nil, e
		}
		if _, e = t.Exec(ctx, "INSERT INTO av_admin_actions(id,scope,resource_id,actor,action,target,reason,operation_ids,created) VALUES(?,?,?,?,?,?,?,?,?)", result.ActionID, t.Scope, id, p.ID, c.Action, c.Target, c.Reason, string(raw), s.now().UnixMilli()); e != nil {
			return nil, e
		}
		// Audit targets the durable record containing the reason; avoid broadcasting
		// internal moderation text to ordinary room participants.
		if e = t.Audit(ctx, p.ID, "av.admin."+c.Action, result.ActionID, ""); e != nil {
			return nil, e
		}
		return result, nil
	})
	if e != nil {
		return nil, e
	}
	if len(leases) > 0 {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		for _, g := range leases {
			_ = s.P.Ephemeral.Delete(cleanup, leaseKey(g))
		}
	}
	return out, nil
}

func (s *Service) adminRevokeControl(ctx context.Context, t *persistence.Tx, p control.Principal, r control.Resource) ([]controlGrant, error) {
	rows, e := t.SQL.QueryContext(ctx, t.Store.Q("SELECT "+grantCols+" FROM av_control_grants WHERE scope=? AND resource_id=? AND state IN ('pending','active')"), t.Scope, r.ID)
	if e != nil {
		return nil, e
	}
	grants := []controlGrant{}
	for rows.Next() {
		g, x := scanControl(rows)
		if x != nil {
			rows.Close()
			return nil, x
		}
		grants = append(grants, g)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	if _, e = t.Exec(ctx, "UPDATE av_control_grants SET state='revoked' WHERE scope=? AND resource_id=? AND state IN ('pending','active')", t.Scope, r.ID); e != nil {
		return nil, e
	}
	if e = s.P.Revoke(ctx, t, r.ID, ""); e != nil {
		return nil, e
	}
	for _, g := range grants {
		if e = s.emit(ctx, t, p, r, "control.revoke", map[string]any{"grant_id": g.ID, "epoch": g.Epoch, "state": "revoked"}); e != nil {
			return nil, e
		}
	}
	return grants, nil
}

func (s *Service) adminMeetingMember(ctx context.Context, t *persistence.Tx, p control.Principal, r control.Resource, c AdminAction) error {
	if c.Action != "kick" && c.Action != "mute" && c.Action != "request-unmute" {
		return control.Fail(400, "invalid_admin_action")
	}
	var host string
	if e := t.Row(ctx, "SELECT host FROM av_meetings WHERE scope=? AND id=?", t.Scope, r.ID).Scan(&host); e != nil {
		return e
	}
	if c.Action == "kick" && c.Target == host {
		return control.Fail(409, "cannot_remove_host")
	}
	m, e := participantTx(ctx, t, r.ID, c.Target)
	if e != nil {
		return control.Fail(404, "participant_not_found")
	}
	if m.State != "active" && m.State != "accepted" && m.State != "waiting" && m.State != "disconnected" {
		return control.Fail(409, "participant_inactive")
	}
	switch c.Action {
	case "kick":
		if e = putParticipant(ctx, t, r.ID, c.Target, m.Role, "removed", s.now().UnixMilli()); e != nil {
			return e
		}
		if _, e = t.Exec(ctx, "UPDATE av_meetings SET sharing='' WHERE scope=? AND id=? AND sharing=?", t.Scope, r.ID, c.Target); e != nil {
			return e
		}
		e = s.P.Revoke(ctx, t, r.ID, c.Target)
	case "mute":
		if _, e = t.Exec(ctx, "UPDATE av_meeting_members SET muted=1,unmute_request=0 WHERE scope=? AND resource_id=? AND subject_id=?", t.Scope, r.ID, c.Target); e != nil {
			return e
		}
		if e = putParticipant(ctx, t, r.ID, c.Target, m.Role, m.State, s.now().UnixMilli()); e != nil {
			return e
		}
		e = s.P.Revoke(ctx, t, r.ID, c.Target)
	case "request-unmute":
		_, e = t.Exec(ctx, "UPDATE av_meeting_members SET unmute_request=? WHERE scope=? AND resource_id=? AND subject_id=?", s.now().UnixMilli(), t.Scope, r.ID, c.Target)
	}
	if e != nil {
		return e
	}
	r.Updated = s.now().UnixMilli()
	if e = s.P.UpdateResource(ctx, t, r); e != nil {
		return e
	}
	return s.emit(ctx, t, p, r, "meeting."+c.Action, map[string]any{"id": r.ID, "target": c.Target, "action": c.Action, "version": r.Version + 1})
}

func (s *Service) adminLiveMember(ctx context.Context, t *persistence.Tx, p control.Principal, r control.Resource, c AdminAction) error {
	if c.Action != "kick" && c.Action != "chat-mute" && c.Action != "chat-unmute" && c.Action != "stage-remove" {
		return control.Fail(400, "invalid_admin_action")
	}
	b, e := broadcastTx(ctx, t, r.ID)
	if e != nil {
		return e
	}
	if c.Target == b.Host {
		return control.Fail(409, "cannot_remove_host")
	}
	m, e := participantTx(ctx, t, r.ID, c.Target)
	if e != nil {
		return control.Fail(404, "participant_not_found")
	}
	if m.State != "active" && m.State != "accepted" {
		return control.Fail(409, "participant_inactive")
	}
	event := c.Action
	switch c.Action {
	case "chat-mute", "chat-unmute":
		_, e = t.Exec(ctx, "UPDATE av_live_members SET muted=? WHERE scope=? AND resource_id=? AND subject_id=?", boolInt(c.Action == "chat-mute"), t.Scope, r.ID, c.Target)
		event = "mute"
	case "kick":
		if _, e = t.Exec(ctx, "UPDATE av_live_members SET banned=1,stage='' WHERE scope=? AND resource_id=? AND subject_id=?", t.Scope, r.ID, c.Target); e != nil {
			return e
		}
		if e = putParticipant(ctx, t, r.ID, c.Target, "viewer", "removed", s.now().UnixMilli()); e != nil {
			return e
		}
		e = s.P.Revoke(ctx, t, r.ID, c.Target)
	case "stage-remove":
		var stage string
		if e = t.Row(ctx, "SELECT stage FROM av_live_members WHERE scope=? AND resource_id=? AND subject_id=?", t.Scope, r.ID, c.Target).Scan(&stage); e != nil {
			return e
		}
		if stage != "approved" {
			return control.Fail(409, "not_on_stage")
		}
		if _, e = t.Exec(ctx, "UPDATE av_live_members SET stage='' WHERE scope=? AND resource_id=? AND subject_id=?", t.Scope, r.ID, c.Target); e != nil {
			return e
		}
		if e = putParticipant(ctx, t, r.ID, c.Target, "viewer", "active", s.now().UnixMilli()); e != nil {
			return e
		}
		e = s.P.Revoke(ctx, t, r.ID, c.Target)
	}
	if e != nil {
		return e
	}
	r.Updated = s.now().UnixMilli()
	if e = s.P.UpdateResource(ctx, t, r); e != nil {
		return e
	}
	return s.emit(ctx, t, p, r, "live."+event, map[string]any{"id": r.ID, "target": c.Target, "action": event, "version": r.Version + 1})
}
