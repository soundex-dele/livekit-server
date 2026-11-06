package v2

import (
	"av-platform/business/internal/platform/control"
	"context"
)

// AuthorizeGrant verifies domain facts that cannot be inferred from a generic
// active participant. It is read-only, and called for issue/refresh/resume.
func (s *Service) AuthorizeGrant(ctx context.Context, scope, resourceID, subjectID string) error {
	var kind, state string
	if e := s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT kind,state FROM av_resources WHERE scope=? AND id=?"), scope, resourceID).Scan(&kind, &state); e != nil {
		return e
	}
	if !s.moduleForKind(kind) {
		return control.Fail(403, "module_disabled")
	}
	if isTerminal(state) {
		return control.Fail(403, "admission_denied")
	}
	switch kind {
	case "cast":
		if state != "paired" && state != "streaming" {
			return control.Fail(403, "admission_denied")
		}
	case "assist":
		if state != "active" {
			return control.Fail(403, "control_grant_inactive")
		}
		g, e := scanControl(s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT "+grantCols+" FROM av_control_grants WHERE scope=? AND resource_id=? AND state='active' AND expires>? AND (host=? OR controller=?) ORDER BY epoch DESC LIMIT 1"), scope, resourceID, s.now().UnixMilli(), subjectID, subjectID))
		if e != nil {
			return control.Fail(403, "control_grant_inactive")
		}
		var epoch int64
		if e = s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT epoch FROM av_assist WHERE scope=? AND id=?"), scope, resourceID).Scan(&epoch); e != nil {
			return e
		}
		if epoch != g.Epoch {
			return control.Fail(403, "stale_epoch")
		}
		lease, e := s.P.Ephemeral.Get(ctx, leaseKey(g))
		if e != nil || lease != leaseValue(g) {
			return control.Fail(403, "control_lease_inactive")
		}
	case "meeting":
		if state == "scheduled" {
			var starts int64
			if e := s.P.Store.DB.QueryRowContext(ctx, s.P.Store.Q("SELECT starts FROM av_meetings WHERE scope=? AND id=?"), scope, resourceID).Scan(&starts); e != nil {
				return e
			}
			if starts > s.now().UnixMilli() {
				return control.Fail(403, "meeting_not_started")
			}
		}
	case "broadcast":
		if state != "starting" && state != "live" && state != "reconnecting" {
			return control.Fail(403, "admission_denied")
		}
	default:
		return control.Fail(403, "admission_denied")
	}
	return nil
}
