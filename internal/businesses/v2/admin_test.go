package v2

import (
	"av-platform/business/internal/platform/control"
	"context"
	"encoding/json"
	"errors"
	"platform-foundation/server/persistence"
	"reflect"
	"strings"
	"testing"
	"time"
)

func adminAct(t *testing.T, s *Service, p control.Principal, id string, c AdminAction, key string) AdminActionResult {
	t.Helper()
	out, e := s.AdminAct(context.Background(), p, id, c, key)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(out)
	if e != nil {
		t.Fatal(e)
	}
	var result AdminActionResult
	if e = json.Unmarshal(raw, &result); e != nil {
		t.Fatal(e)
	}
	return result
}

func TestAdminQueriesPermissionIsolationAndPagination(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	owner := user("owner")
	admin := user("operator")
	id := str(mustCall(t, f.a, owner, "conference/meetings", map[string]any{"title": "Alpha 100%", "password": "hidden-password"}, "m1"), "id")
	mustCall(t, f.a, owner, "conference/meetings", map[string]string{"title": "Beta"}, "m2")
	other := owner
	other.Scope = "org:two"
	mustCall(t, f.a, other, "conference/meetings", map[string]string{"title": "Other tenant"}, "m3")
	filter := AdminFilter{Limit: 1}
	page, e := f.a.AdminResources(ctx, admin, false, filter)
	if e != nil || page.Total != 2 || len(page.Items) != 1 {
		t.Fatalf("page=%+v err=%v", page, e)
	}
	filter.Offset = 1
	next, e := f.a.AdminResources(ctx, admin, false, filter)
	if e != nil || len(next.Items) != 1 || next.Items[0].ID == page.Items[0].ID {
		t.Fatalf("next=%+v err=%v", next, e)
	}
	filter = AdminFilter{Limit: 20, Query: "100%"}
	page, e = f.a.AdminResources(ctx, admin, false, filter)
	if e != nil || page.Total != 1 {
		t.Fatalf("literal filter=%+v %v", page, e)
	}
	member := admin
	member.Permissions = []string{"av.conference.create"}
	if _, e = f.a.AdminResources(ctx, member, false, filter); code(e) != "forbidden" {
		t.Fatalf("member access: %v", e)
	}
	if _, e = f.a.AdminDetail(ctx, other, id); code(e) != "resource_not_found" {
		t.Fatalf("cross scope access: %v", e)
	}
	if _, e = f.a.meetingSnapshot(ctx, admin, id); code(e) != "not_a_participant" {
		t.Fatalf("normal API relaxed: %v", e)
	}
	detail, e := f.a.AdminDetail(ctx, admin, id)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(detail)
	if strings.Contains(string(raw), "hidden-password") || strings.Contains(string(raw), "password_hash") || strings.Contains(string(raw), "password_salt") {
		t.Fatal("password material leaked")
	}
	f.a.modules["conference"] = false
	detail, e = f.a.AdminDetail(ctx, admin, id)
	if e != nil || detail.Resource.ModuleEnabled {
		t.Fatalf("disabled history: %+v %v", detail, e)
	}
	platform := admin
	platform.Scope = "app"
	overview, e := f.a.AdminOverview(ctx, platform, true)
	if e != nil || overview.CreatedLast24h != 3 {
		t.Fatalf("overview=%+v %v", overview, e)
	}
	if _, e = f.a.AdminOverview(ctx, admin, true); code(e) != "platform_admin_required" {
		t.Fatalf("organization wildcard became platform: %v", e)
	}
}

func TestAdminMeetingModerationAuditAndIdempotency(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	host := user("host")
	admin := user("operator")
	guest := user("member")
	created := mustCall(t, f.a, host, "conference/meetings", map[string]string{"title": "Meeting"}, "create")
	id := str(created, "id")
	mustCall(t, f.a, guest, "conference/join", map[string]string{"number": str(created, "number")}, "")
	mustCall(t, f.a, guest, "conference/meetings/"+id+"/share-start", nil, "")
	mustCall(t, f.a, guest, "conference/meetings/"+id+"/connect", nil, "")
	f.ready(t)
	cmd := AdminAction{Action: "mute", Target: guest.ID, Reason: "moderation request"}
	first := adminAct(t, f.a, admin, id, cmd, "mute")
	if !first.Accepted || len(first.OperationIDs) == 0 {
		t.Fatalf("no revocation %+v", first)
	}
	again := adminAct(t, f.b, admin, id, cmd, "mute")
	if !reflect.DeepEqual(first, again) {
		t.Fatal("idempotency changed response")
	}
	cmd.Reason = "changed"
	if _, e := f.a.AdminAct(ctx, admin, id, cmd, "mute"); code(e) != "idempotency_conflict" {
		t.Fatalf("key mismatch %v", e)
	}
	detail, e := f.a.AdminDetail(ctx, admin, id)
	if e != nil {
		t.Fatal(e)
	}
	for _, m := range detail.Members {
		if m.SubjectID == guest.ID && !m.Muted {
			t.Fatal("mute not persisted")
		}
	}
	adminAct(t, f.a, admin, id, AdminAction{Action: "request-unmute", Target: guest.ID, Reason: "allow user to choose"}, "invite")
	detail, e = f.a.AdminDetail(ctx, admin, id)
	if e != nil {
		t.Fatal(e)
	}
	for _, m := range detail.Members {
		if m.SubjectID == guest.ID && (!m.Muted || m.UnmuteRequest == 0) {
			t.Fatal("invite forced microphone open")
		}
	}
	if _, e = f.a.AdminAct(ctx, admin, id, AdminAction{Action: "kick", Target: host.ID, Reason: "test"}, "host-kick"); code(e) != "cannot_remove_host" {
		t.Fatalf("host removed %v", e)
	}
	adminAct(t, f.a, admin, id, AdminAction{Action: "kick", Target: guest.ID, Reason: "remove member"}, "kick")
	var sharing string
	if e = f.store.DB.QueryRow(f.store.Q("SELECT sharing FROM av_meetings WHERE scope=? AND id=?"), host.Scope, id).Scan(&sharing); e != nil || sharing != "" {
		t.Fatalf("share not cleared %s %v", sharing, e)
	}
	if _, e = call(f.a, guest, "conference/join", map[string]string{"number": str(created, "number")}, ""); code(e) != "participant_removed" && code(e) != "not_admitted" {
		if e == nil {
			t.Fatal("removed member rejoined")
		}
	}
	if _, e = f.a.AdminAct(ctx, admin, id, AdminAction{Action: "end", Reason: "stale", Version: 1}, "stale"); code(e) != "version_conflict" {
		t.Fatalf("stale write %v", e)
	}
	f.a.modules["conference"] = false
	ended := adminAct(t, f.a, admin, id, AdminAction{Action: "end", Reason: "meeting finished"}, "end")
	if ended.State != "ended" {
		t.Fatalf("not ended %+v", ended)
	}
	ops, e := f.a.AdminOperations(ctx, admin, false, AdminFilter{Limit: 100, ResourceID: id})
	if e != nil {
		t.Fatal(e)
	}
	for _, o := range ops.Items {
		if o.JobID == "" {
			t.Fatal("missing job association")
		}
	}
	for {
		j, x := f.a.P.Queue.Claim(ctx, "job", []string{"av.media"}, 30*time.Second)
		if errors.Is(x, persistence.ErrNotFound) {
			break
		}
		if x != nil {
			t.Fatal(x)
		}
		result, x := f.a.P.Execute(ctx, j, func(int) error { return nil })
		if x != nil {
			t.Fatal(x)
		}
		if x = f.a.P.Queue.Finish(ctx, j, result, nil); x != nil {
			t.Fatal(x)
		}
	}
	ops, e = f.a.AdminOperations(ctx, admin, false, AdminFilter{Limit: 100, ResourceID: id})
	if e != nil {
		t.Fatal(e)
	}
	for _, o := range ops.Items {
		if o.State != "succeeded" && o.State != "superseded" {
			t.Fatalf("cleanup not final %+v", o)
		}
	}
	detail, e = f.a.AdminDetail(ctx, admin, id)
	if e != nil {
		t.Fatal(e)
	}
	if len(detail.Actions) != 4 {
		t.Fatalf("audit entries %d", len(detail.Actions))
	}
	for _, a := range detail.Actions {
		if a.Actor != admin.ID || a.Reason == "" {
			t.Fatal("lost audit actor or reason")
		}
	}
}

func TestAdminCancelAndCastEnd(t *testing.T) {
	f := newFixture(t)
	admin := user("operator")
	host := user("host")
	scheduled := mustCall(t, f.a, host, "conference/meetings", map[string]any{"title": "Scheduled", "scheduled": true, "starts_at": f.a.now().Add(time.Hour).Format(time.RFC3339)}, "create")
	result := adminAct(t, f.a, admin, str(scheduled, "id"), AdminAction{Action: "cancel", Reason: "cancel appointment"}, "cancel")
	if result.State != "cancelled" {
		t.Fatal(result)
	}
	created := mustCall(t, f.a, host, "screen-cast/sessions", map[string]string{"title": "Cast"}, "")
	id := str(created, "id")
	paired := mustCall(t, f.a, control.Principal{}, "screen-cast/redeem", map[string]string{"code": str(created, "pairing_code")}, "")
	guest, e := f.a.P.Guest(context.Background(), str(paired, "credential"))
	if e != nil {
		t.Fatal(e)
	}
	mustCall(t, f.a, guest, "screen-cast/sessions/"+id+"/connect", nil, "")
	f.ready(t)
	result = adminAct(t, f.a, admin, id, AdminAction{Action: "end", Reason: "stop cast"}, "end")
	if result.State != "ended" || len(result.OperationIDs) == 0 {
		t.Fatal(result)
	}
	if e = f.a.AuthorizeGrant(context.Background(), host.Scope, id, host.ID); e == nil {
		t.Fatal("ended cast still authorized")
	}
}

func TestAdminRemoteRevocationRequiresFreshConsent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	host := user("host")
	admin := user("operator")
	created := mustCall(t, f.a, host, "remote-control/sessions", nil, "")
	id := str(created, "id")
	paired := mustCall(t, f.a, control.Principal{}, "remote-control/redeem", map[string]string{"code": str(created, "pairing_code")}, "")
	controller, e := f.a.P.Guest(ctx, str(paired, "credential"))
	if e != nil {
		t.Fatal(e)
	}
	requested := mustCall(t, f.a, controller, "remote-control/sessions/"+id+"/request", nil, "request")
	gid := str(requested, "id")
	approved := mustCall(t, f.a, host, "control-grants/"+gid+"/approve", map[string]bool{"local_consent": true}, "")
	result := adminAct(t, f.a, admin, id, AdminAction{Action: "revoke-control", Reason: "operator revoked access"}, "revoke")
	if result.State != "paired" || len(result.OperationIDs) == 0 {
		t.Fatal(result)
	}
	if _, e = call(f.a, host, "control-grants/"+gid+"/renew", map[string]any{"epoch": approved["epoch"]}, ""); code(e) != "control_grant_inactive" {
		t.Fatalf("renewed revoked grant %v", e)
	}
	if _, e = f.a.AdminAct(ctx, admin, id, AdminAction{Action: "approve", Reason: "invalid"}, "approve"); code(e) != "invalid_admin_action" {
		t.Fatalf("admin approved %v", e)
	}
	next := mustCall(t, f.a, controller, "remote-control/sessions/"+id+"/request", nil, "new-request")
	if next["state"] != "pending" {
		t.Fatal("new consent bypassed")
	}
	result = adminAct(t, f.a, admin, id, AdminAction{Action: "end", Reason: "end remote assistance"}, "end")
	if result.State != "ended" {
		t.Fatal(result)
	}
}

func TestAdminLiveModerationAndPKCleanup(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	host := user("host")
	admin := user("operator")
	viewer := user("viewer")
	id := startLive(t, f, host)
	mustCall(t, f.a, viewer, "live-studio/broadcasts/"+id+"/watch", nil, "")
	adminAct(t, f.a, admin, id, AdminAction{Action: "chat-mute", Target: viewer.ID, Reason: "moderate chat"}, "mute")
	if _, e := call(f.a, viewer, "live-studio/broadcasts/"+id+"/chat", map[string]string{"content": "hello"}, ""); code(e) != "chat_muted" {
		t.Fatalf("chat not muted %v", e)
	}
	adminAct(t, f.a, admin, id, AdminAction{Action: "chat-unmute", Target: viewer.ID, Reason: "restore chat"}, "unmute")
	mustCall(t, f.a, viewer, "live-studio/broadcasts/"+id+"/stage-request", nil, "")
	mustCall(t, f.a, host, "live-studio/broadcasts/"+id+"/stage-approve", map[string]string{"target": viewer.ID}, "")
	adminAct(t, f.a, admin, id, AdminAction{Action: "stage-remove", Target: viewer.ID, Reason: "end guest slot"}, "remove-stage")
	detail, e := f.a.AdminDetail(ctx, admin, id)
	if e != nil {
		t.Fatal(e)
	}
	for _, m := range detail.Members {
		if m.SubjectID == viewer.ID && (m.Stage != "" || m.Role != "viewer") {
			t.Fatal("stage not removed")
		}
	}
	adminAct(t, f.a, admin, id, AdminAction{Action: "kick", Target: viewer.ID, Reason: "remove viewer"}, "kick")
	if _, e = call(f.a, viewer, "live-studio/broadcasts/"+id+"/watch", nil, ""); code(e) != "viewer_banned" {
		t.Fatalf("viewer rejoined %v", e)
	}
	other := startLive(t, f, user("other"))
	pk := mustCall(t, f.a, host, "live-studio/broadcasts/"+id+"/pk-request", map[string]string{"target": other}, "")
	mustCall(t, f.a, user("other"), "live-studio/broadcasts/"+other+"/pk-accept", nil, "")
	adminAct(t, f.a, admin, id, AdminAction{Action: "end", Reason: "end stream"}, "end")
	var state string
	if e = f.store.DB.QueryRow(f.store.Q("SELECT state FROM av_live_pk WHERE id=?"), str(pk, "id")).Scan(&state); e != nil || state != "ended" {
		t.Fatalf("PK still active %s %v", state, e)
	}
}
