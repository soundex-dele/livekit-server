package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type grantRecord struct {
	ID, Scope, Resource, Subject, Identity, Binding, Status, Capabilities string
	Version, Expires                                                      int64
}

func (s *Platform) readGrant(ctx context.Context, id string) (grantRecord, error) {
	var g grantRecord
	e := s.Store.DB.QueryRowContext(ctx, s.Store.Q("SELECT id,scope,resource_id,subject_id,identity,binding_id,status,capabilities,policy_version,expires FROM av_rtc_grants WHERE id=?"), id).Scan(&g.ID, &g.Scope, &g.Resource, &g.Subject, &g.Identity, &g.Binding, &g.Status, &g.Capabilities, &g.Version, &g.Expires)
	if errors.Is(e, sql.ErrNoRows) {
		e = Fail(403, "admission_denied")
	}
	return g, e
}
func (s *Platform) checkGrant(ctx context.Context, g grantRecord) (binding, error) {
	var b binding
	if g.Status != "active" || g.Expires <= s.Now().Unix() {
		return b, Fail(403, "admission_denied")
	}
	var state, memberState string
	var version int64
	e := s.Store.DB.QueryRowContext(ctx, s.Store.Q("SELECT r.state,p.state,p.policy_version FROM av_resources r JOIN av_participants p ON p.scope=r.scope AND p.resource_id=r.id WHERE r.scope=? AND r.id=? AND p.subject_id=?"), g.Scope, g.Resource, g.Subject).Scan(&state, &memberState, &version)
	if e != nil || state == "ended" || state == "expired" || state == "failed" || state == "cancelled" || memberState != "active" && memberState != "accepted" || version != g.Version {
		return b, Fail(403, "admission_denied")
	}
	if s.ValidateSubject != nil {
		if e = s.ValidateSubject(ctx, Principal{ID: g.Subject, Scope: g.Scope, ResourceID: g.Resource}); e != nil {
			return b, Fail(403, "admission_denied")
		}
	}
	if s.AuthorizeGrant != nil {
		if e = s.AuthorizeGrant(ctx, g.Scope, g.Resource, g.Subject); e != nil {
			return b, e
		}
	}
	b, e = scanBinding(s.Store.DB.QueryRowContext(ctx, s.Store.Q("SELECT "+bindingCols+" FROM av_room_bindings WHERE id=?"), g.Binding))
	if e != nil {
		return b, e
	}
	if b.State != "ready" {
		return b, Fail(409, "media_preparing")
	}
	var inc string
	var healthy int
	var updated int64
	e = s.Store.DB.QueryRowContext(ctx, s.Store.Q("SELECT incarnation,healthy,updated FROM av_media_nodes WHERE id=?"), b.Node).Scan(&inc, &healthy, &updated)
	if e != nil || inc != b.Incarnation || healthy != 1 || updated < s.Now().Add(-30*time.Second).UnixMilli() {
		return b, Fail(503, "media_node_unavailable")
	}
	return b, nil
}
func (s *Platform) Credentials(ctx context.Context, p Principal, id string) (map[string]any, error) {
	g, e := s.readGrant(ctx, id)
	if e != nil {
		return nil, e
	}
	if g.Scope != p.Scope || g.Subject != p.ID || p.ResourceID != "" && g.Resource != p.ResourceID {
		return nil, Fail(403, "grant_forbidden")
	}
	b, e := s.checkGrant(ctx, g)
	if e != nil {
		return nil, e
	}
	n, ok := s.node(b.Node)
	if !ok {
		return nil, Fail(503, "media_node_unavailable")
	}
	var c Capabilities
	if e = json.Unmarshal([]byte(g.Capabilities), &c); e != nil {
		return nil, e
	}
	exp := min(s.Now().Add(time.Minute).Unix(), g.Expires)
	video := map[string]any{"roomJoin": true, "room": b.Room, "canPublish": c.Publish, "canSubscribe": c.Subscribe, "canPublishData": c.Data, "canUpdateOwnMetadata": false}
	if len(c.Sources) > 0 {
		video["canPublishSources"] = c.Sources
	}
	token := sign(n.RTCKey, n.RTCSecret, map[string]any{"sub": g.Identity, "nbf": s.Now().Add(-5 * time.Second).Unix(), "exp": exp, "video": video, "av_grant": g.ID, "av_policy_version": g.Version, "av_binding_generation": b.Generation, "av_node_id": b.Node, "av_node_incarnation": b.Incarnation, "av_grant_expires_at": g.Expires})
	return map[string]any{"grant_id": g.ID, "media_url": n.PublicURL, "media_token": token, "participant_identity": g.Identity, "room": b.Room, "expires_at": time.Unix(exp, 0).UTC().Format(time.RFC3339), "granted_capabilities": c, "policy_version": g.Version}, nil
}

type AdmissionRequest struct {
	NodeID            string `json:"node_id"`
	NodeIncarnation   string `json:"node_incarnation"`
	Room              string `json:"room"`
	Identity          string `json:"identity"`
	GrantID           string `json:"grant_id"`
	PolicyVersion     int64  `json:"policy_version"`
	BindingGeneration int64  `json:"binding_generation"`
}

func (s *Platform) Admit(ctx context.Context, q AdmissionRequest) (map[string]any, error) {
	g, e := s.readGrant(ctx, q.GrantID)
	if e != nil {
		return nil, e
	}
	b, e := s.checkGrant(ctx, g)
	if e != nil {
		return nil, e
	}
	if q.NodeID != b.Node || q.NodeIncarnation != b.Incarnation || q.Room != b.Room || q.Identity != g.Identity || q.PolicyVersion != g.Version || q.BindingGeneration != b.Generation {
		return nil, Fail(403, "admission_denied")
	}
	return map[string]any{"allowed": true, "policy_version": g.Version, "binding_generation": b.Generation, "expires_at": g.Expires}, nil
}
