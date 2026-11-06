package v2

import (
	"av-platform/business/internal/platform/control"
	"context"
	"encoding/json"
	"strings"
)

// AdminFilter is deliberately independent of the client-facing list contracts.
type AdminFilter struct {
	Business, State, Owner, Query, ResourceID string
	From, To                                  int64
	Limit, Offset                             int
}

type AdminPage[T any] struct {
	Items  []T   `json:"items"`
	Total  int64 `json:"total"`
	Limit  int   `json:"limit"`
	Offset int   `json:"offset"`
}

type AdminResource struct {
	ID             string `json:"id"`
	OrganizationID string `json:"organization_id"`
	Business       string `json:"business"`
	Kind           string `json:"kind"`
	Title          string `json:"title"`
	State          string `json:"state"`
	Owner          string `json:"owner"`
	Version        int64  `json:"version"`
	Created        int64  `json:"created"`
	Updated        int64  `json:"updated"`
	ModuleEnabled  bool   `json:"module_enabled"`
}

func businessForKind(kind string) string {
	for _, m := range businessModules {
		if m.kind == kind {
			return m.name
		}
	}
	return ""
}

func kindForBusiness(name string) string {
	for _, m := range businessModules {
		if m.name == name {
			return m.kind
		}
	}
	return ""
}

func adminAllowed(p control.Principal, platform bool) error {
	if !p.IsUser() || p.ID == "" || p.ResourceID != "" {
		return control.Fail(403, "forbidden")
	}
	if platform {
		if p.Scope != "app" || !p.Can("*") {
			return control.Fail(403, "platform_admin_required")
		}
		return nil
	}
	return permission(p, "av.manage")
}

func (s *Service) adminResource(r control.Resource) AdminResource {
	return AdminResource{r.ID, strings.TrimPrefix(r.Scope, "org:"), businessForKind(r.Kind), r.Kind, r.Title, r.State, r.Owner, r.Version, r.Created, r.Updated, s.moduleForKind(r.Kind)}
}

func adminWhere(p control.Principal, platform bool, f AdminFilter) (string, []any) {
	where := "r.kind IN ('cast','assist','meeting','broadcast')"
	args := []any{}
	if !platform {
		where += " AND r.scope=?"
		args = append(args, p.Scope)
	}
	if f.Business != "" {
		where += " AND r.kind=?"
		args = append(args, kindForBusiness(f.Business))
	}
	if f.State != "" {
		where += " AND r.state=?"
		args = append(args, f.State)
	}
	if f.Owner != "" {
		where += " AND r.owner=?"
		args = append(args, f.Owner)
	}
	if f.ResourceID != "" {
		where += " AND r.id=?"
		args = append(args, f.ResourceID)
	}
	if f.Query != "" {
		where += " AND (LOWER(r.title) LIKE ? ESCAPE '!' OR LOWER(r.id) LIKE ? ESCAPE '!')"
		v := "%" + strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(strings.ToLower(f.Query)) + "%"
		args = append(args, v, v)
	}
	if f.From > 0 {
		where += " AND r.created>=?"
		args = append(args, f.From)
	}
	if f.To > 0 {
		where += " AND r.created<=?"
		args = append(args, f.To)
	}
	return where, args
}

func validAdminFilter(f AdminFilter) bool {
	return f.Limit >= 1 && f.Limit <= 100 && f.Offset >= 0 && f.Offset <= 10000000 && len(f.Query) <= 200 && len(f.Owner) <= 128 && len(f.State) <= 40 && (f.Business == "" || kindForBusiness(f.Business) != "") && f.From >= 0 && f.To >= 0 && (f.To == 0 || f.From <= f.To)
}

func (s *Service) AdminResources(ctx context.Context, p control.Principal, platform bool, f AdminFilter) (AdminPage[AdminResource], error) {
	out := AdminPage[AdminResource]{Items: []AdminResource{}, Limit: f.Limit, Offset: f.Offset}
	if e := adminAllowed(p, platform); e != nil {
		return out, e
	}
	if !validAdminFilter(f) {
		return out, control.Fail(400, "invalid_filter")
	}
	where, args := adminWhere(p, platform, f)
	if e := s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT COUNT(*) FROM av_resources r WHERE "+where), args...).Scan(&out.Total); e != nil {
		return out, e
	}
	rows, e := s.P.Store.DB.QueryContext(ctx, s.P.Store.Q("SELECT r.id,r.scope,r.kind,r.owner,r.title,r.state,r.version,r.created,r.updated FROM av_resources r WHERE "+where+" ORDER BY r.created DESC,r.id DESC LIMIT ? OFFSET ?"), append(args, f.Limit, f.Offset)...)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		var r control.Resource
		if e = rows.Scan(&r.ID, &r.Scope, &r.Kind, &r.Owner, &r.Title, &r.State, &r.Version, &r.Created, &r.Updated); e != nil {
			return out, e
		}
		out.Items = append(out.Items, s.adminResource(r))
	}
	return out, rows.Err()
}

type AdminCount struct {
	Business string `json:"business,omitempty"`
	State    string `json:"state"`
	Count    int64  `json:"count"`
}
type AdminOverview struct {
	GeneratedAt    int64        `json:"generated_at"`
	Resources      []AdminCount `json:"resources"`
	CreatedLast24h int64        `json:"created_last_24h"`
	Operations     []AdminCount `json:"operations"`
}

func (s *Service) AdminOverview(ctx context.Context, p control.Principal, platform bool) (AdminOverview, error) {
	out := AdminOverview{GeneratedAt: s.now().UnixMilli(), Resources: []AdminCount{}, Operations: []AdminCount{}}
	if e := adminAllowed(p, platform); e != nil {
		return out, e
	}
	where, args := adminWhere(p, platform, AdminFilter{})
	rows, e := s.P.Store.DB.QueryContext(ctx, s.P.Store.Q("SELECT r.kind,r.state,COUNT(*) FROM av_resources r WHERE "+where+" GROUP BY r.kind,r.state ORDER BY r.kind,r.state"), args...)
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var c AdminCount
		var kind string
		if e = rows.Scan(&kind, &c.State, &c.Count); e != nil {
			rows.Close()
			return out, e
		}
		c.Business = businessForKind(kind)
		out.Resources = append(out.Resources, c)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	if e = s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT COUNT(*) FROM av_resources r WHERE "+where+" AND r.created>=?"), append(args, out.GeneratedAt-86400000)...).Scan(&out.CreatedLast24h); e != nil {
		return out, e
	}
	rows, e = s.P.Store.DB.QueryContext(ctx, s.P.Store.Q("SELECT o.state,COUNT(*) FROM av_media_operations o JOIN av_resources r ON r.scope=o.scope AND r.id=o.resource_id WHERE "+where+" GROUP BY o.state ORDER BY o.state"), args...)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		var c AdminCount
		if e = rows.Scan(&c.State, &c.Count); e != nil {
			return out, e
		}
		out.Operations = append(out.Operations, c)
	}
	return out, rows.Err()
}

type AdminOperation struct {
	ID             string `json:"id"`
	OrganizationID string `json:"organization_id"`
	ResourceID     string `json:"resource_id"`
	BindingID      string `json:"binding_id"`
	SubjectID      string `json:"subject_id"`
	Action         string `json:"action"`
	State          string `json:"state"`
	JobID          string `json:"job_id"`
	Created        int64  `json:"created"`
	Updated        int64  `json:"updated"`
}

func (s *Service) AdminOperations(ctx context.Context, p control.Principal, platform bool, f AdminFilter) (AdminPage[AdminOperation], error) {
	out := AdminPage[AdminOperation]{Items: []AdminOperation{}, Limit: f.Limit, Offset: f.Offset}
	if e := adminAllowed(p, platform); e != nil {
		return out, e
	}
	if !validAdminFilter(f) {
		return out, control.Fail(400, "invalid_filter")
	}
	state := f.State
	f.State = ""
	where, args := adminWhere(p, platform, f)
	if state != "" {
		where += " AND o.state=?"
		args = append(args, state)
	}
	from := " FROM av_media_operations o JOIN av_resources r ON r.scope=o.scope AND r.id=o.resource_id WHERE " + where
	if e := s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT COUNT(*)"+from), args...).Scan(&out.Total); e != nil {
		return out, e
	}
	rows, e := s.P.Store.DB.QueryContext(ctx, s.P.Store.Q("SELECT o.id,o.scope,o.resource_id,o.binding_id,o.subject_id,o.action,o.state,o.job_id,o.created,o.updated"+from+" ORDER BY o.created DESC,o.id DESC LIMIT ? OFFSET ?"), append(args, f.Limit, f.Offset)...)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		var o AdminOperation
		if e = rows.Scan(&o.ID, &o.OrganizationID, &o.ResourceID, &o.BindingID, &o.SubjectID, &o.Action, &o.State, &o.JobID, &o.Created, &o.Updated); e != nil {
			return out, e
		}
		if o.JobID == "" {
			o.JobID = control.Hash(o.OrganizationID + "|system|job|" + o.ID)
		}
		o.OrganizationID = strings.TrimPrefix(o.OrganizationID, "org:")
		out.Items = append(out.Items, o)
	}
	return out, rows.Err()
}

func (s *Service) AdminOperation(ctx context.Context, p control.Principal, id string) (AdminOperation, error) {
	var o AdminOperation
	if e := adminAllowed(p, false); e != nil {
		return o, e
	}
	e := s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT id,scope,resource_id,binding_id,subject_id,action,state,job_id,created,updated FROM av_media_operations WHERE scope=? AND id=?"), p.Scope, id).Scan(&o.ID, &o.OrganizationID, &o.ResourceID, &o.BindingID, &o.SubjectID, &o.Action, &o.State, &o.JobID, &o.Created, &o.Updated)
	if e != nil {
		return o, control.Fail(404, "operation_not_found")
	}
	if o.JobID == "" {
		o.JobID = control.Hash(o.OrganizationID + "|system|job|" + o.ID)
	}
	o.OrganizationID = strings.TrimPrefix(o.OrganizationID, "org:")
	return o, nil
}

type AdminMember struct {
	SubjectID     string `json:"subject_id"`
	Name          string `json:"name"`
	Role          string `json:"role"`
	State         string `json:"state"`
	PolicyVersion int64  `json:"policy_version"`
	Updated       int64  `json:"updated"`
	Muted         bool   `json:"muted"`
	UnmuteRequest int64  `json:"unmute_request"`
	Stage         string `json:"stage"`
	Banned        bool   `json:"banned"`
}
type AdminBinding struct {
	ID         string `json:"id"`
	NodeID     string `json:"node_id"`
	Purpose    string `json:"purpose"`
	State      string `json:"state"`
	Generation int64  `json:"generation"`
	Created    int64  `json:"created"`
}
type AdminActionRecord struct {
	ID           string   `json:"id"`
	Actor        string   `json:"actor"`
	Action       string   `json:"action"`
	Target       string   `json:"target"`
	Reason       string   `json:"reason"`
	Created      int64    `json:"created"`
	OperationIDs []string `json:"operation_ids"`
}
type AdminDetail struct {
	Resource         AdminResource       `json:"resource"`
	Members          []AdminMember       `json:"members"`
	Bindings         []AdminBinding      `json:"bindings"`
	Actions          []AdminActionRecord `json:"actions"`
	Details          map[string]any      `json:"details"`
	MembersTruncated bool                `json:"members_truncated"`
}

func (s *Service) AdminDetail(ctx context.Context, p control.Principal, id string) (AdminDetail, error) {
	out := AdminDetail{Members: []AdminMember{}, Bindings: []AdminBinding{}, Actions: []AdminActionRecord{}, Details: map[string]any{}}
	if e := adminAllowed(p, false); e != nil {
		return out, e
	}
	r, e := s.P.GetResource(ctx, p, id)
	if e != nil {
		return out, e
	}
	if businessForKind(r.Kind) == "" {
		return out, control.Fail(404, "resource_not_found")
	}
	out.Resource = s.adminResource(r)
	rows, e := s.P.Store.DB.QueryContext(ctx, s.P.Store.Q("SELECT p.subject_id,COALESCE(m.name,p.subject_id),p.role,p.state,p.policy_version,p.updated,COALESCE(m.muted,l.muted,0),COALESCE(m.unmute_request,0),COALESCE(l.stage,''),COALESCE(l.banned,0) FROM av_participants p LEFT JOIN av_meeting_members m ON m.scope=p.scope AND m.resource_id=p.resource_id AND m.subject_id=p.subject_id LEFT JOIN av_live_members l ON l.scope=p.scope AND l.resource_id=p.resource_id AND l.subject_id=p.subject_id WHERE p.scope=? AND p.resource_id=? ORDER BY p.updated,p.subject_id LIMIT 1001"), p.Scope, id)
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var m AdminMember
		var muted, banned int
		if e = rows.Scan(&m.SubjectID, &m.Name, &m.Role, &m.State, &m.PolicyVersion, &m.Updated, &muted, &m.UnmuteRequest, &m.Stage, &banned); e != nil {
			rows.Close()
			return out, e
		}
		m.Muted = muted != 0
		m.Banned = banned != 0
		out.Members = append(out.Members, m)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	if len(out.Members) > 1000 {
		out.Members = out.Members[:1000]
		out.MembersTruncated = true
	}
	rows, e = s.P.Store.DB.QueryContext(ctx, s.P.Store.Q("SELECT id,node_id,purpose,state,generation,created FROM av_room_bindings WHERE scope=? AND resource_id=? ORDER BY created DESC,id DESC LIMIT 100"), p.Scope, id)
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var b AdminBinding
		if e = rows.Scan(&b.ID, &b.NodeID, &b.Purpose, &b.State, &b.Generation, &b.Created); e != nil {
			rows.Close()
			return out, e
		}
		out.Bindings = append(out.Bindings, b)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	rows, e = s.P.Store.DB.QueryContext(ctx, s.P.Store.Q("SELECT id,actor,action,target,reason,operation_ids,created FROM av_admin_actions WHERE scope=? AND resource_id=? ORDER BY created DESC,id DESC LIMIT 100"), p.Scope, id)
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var a AdminActionRecord
		var raw string
		if e = rows.Scan(&a.ID, &a.Actor, &a.Action, &a.Target, &a.Reason, &raw, &a.Created); e != nil {
			rows.Close()
			return out, e
		}
		if e = json.Unmarshal([]byte(raw), &a.OperationIDs); e != nil {
			rows.Close()
			return out, e
		}
		out.Actions = append(out.Actions, a)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	out.Details, e = s.adminBusinessDetail(ctx, p, r)
	return out, e
}

func (s *Service) adminBusinessDetail(ctx context.Context, p control.Principal, r control.Resource) (map[string]any, error) {
	out := map[string]any{}
	row := func(query string) interface{ Scan(...any) error } {
		return s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q(query), p.Scope, r.ID)
	}
	switch r.Kind {
	case "meeting":
		var number, host, sharing string
		var starts int64
		var duration, waiting, locked int
		if e := row("SELECT number,host,starts,duration,waiting,locked,sharing FROM av_meetings WHERE scope=? AND id=?").Scan(&number, &host, &starts, &duration, &waiting, &locked, &sharing); e != nil {
			return nil, e
		}
		out["meeting"] = map[string]any{"number": number, "host": host, "starts_at": starts, "duration_minutes": duration, "waiting_room": waiting != 0, "locked": locked != 0, "sharing_member": sharing}
	case "cast":
		var sender, receiver string
		if e := row("SELECT sender,receiver FROM av_cast WHERE scope=? AND id=?").Scan(&sender, &receiver); e != nil {
			return nil, e
		}
		out["cast"] = map[string]any{"sender": sender, "receiver": receiver}
	case "assist":
		var host, controller string
		var epoch int64
		if e := row("SELECT host,controller,epoch FROM av_assist WHERE scope=? AND id=?").Scan(&host, &controller, &epoch); e != nil {
			return nil, e
		}
		grants := []map[string]any{}
		rows, e := s.P.Store.DB.QueryContext(ctx, s.P.Store.Q("SELECT id,state,epoch,expires,created FROM av_control_grants WHERE scope=? AND resource_id=? ORDER BY created DESC,id DESC LIMIT 100"), p.Scope, r.ID)
		if e != nil {
			return nil, e
		}
		defer rows.Close()
		for rows.Next() {
			var id, state string
			var epoch, expires, created int64
			if e = rows.Scan(&id, &state, &epoch, &expires, &created); e != nil {
				return nil, e
			}
			grants = append(grants, map[string]any{"id": id, "state": state, "epoch": epoch, "expires": expires, "created": created})
		}
		if e = rows.Err(); e != nil {
			return nil, e
		}
		out["remote"] = map[string]any{"host": host, "controller": controller, "epoch": epoch, "control_grants": grants}
	case "broadcast":
		var room, host, pk string
		var started int64
		if e := row("SELECT room_id,host,pk_id,started FROM av_broadcasts WHERE scope=? AND id=?").Scan(&room, &host, &pk, &started); e != nil {
			return nil, e
		}
		out["live"] = map[string]any{"room_id": room, "host": host, "pk_id": pk, "started": started}
	}
	return out, nil
}
