package control

import (
	"context"
	"sort"
	"time"
)

// AdminNode contains only observability data. Never serialize Node credentials.
type AdminNode struct {
	ID          string `json:"id"`
	Incarnation string `json:"incarnation"`
	Enabled     bool   `json:"enabled"`
	Healthy     bool   `json:"healthy"`
	Status      string `json:"status"`
	Capacity    int    `json:"capacity"`
	Bindings    int64  `json:"bindings"`
	Updated     int64  `json:"updated"`
	Configured  bool   `json:"configured"`
}

func (s *Platform) AdminNodes(ctx context.Context) ([]AdminNode, error) {
	configured := map[string]Node{}
	for _, n := range s.Nodes {
		configured[n.ID] = n
	}
	rows, e := s.Store.DB.QueryContext(ctx, "SELECT n.id,n.incarnation,n.enabled,n.healthy,n.capacity,n.updated,(SELECT COUNT(*) FROM av_room_bindings b WHERE b.node_id=n.id AND b.state IN ('preparing','ready','ending')) FROM av_media_nodes n ORDER BY n.id")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []AdminNode{}
	seen := map[string]bool{}
	for rows.Next() {
		var n AdminNode
		var enabled, healthy int
		if e = rows.Scan(&n.ID, &n.Incarnation, &enabled, &healthy, &n.Capacity, &n.Updated, &n.Bindings); e != nil {
			return nil, e
		}
		_, n.Configured = configured[n.ID]
		n.Enabled = enabled != 0 && n.Configured
		n.Healthy = healthy != 0
		n.Status = "unhealthy"
		if n.Healthy {
			n.Status = "healthy"
		}
		if n.Updated <= 0 {
			n.Status = "unknown"
		} else if s.Now().Sub(time.UnixMilli(n.Updated)) > 30*time.Second {
			n.Status = "stale"
		}
		if !n.Configured {
			n.Enabled = false
		}
		seen[n.ID] = true
		out = append(out, n)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	for _, n := range s.Nodes {
		if !seen[n.ID] {
			out = append(out, AdminNode{ID: n.ID, Enabled: true, Status: "unknown", Capacity: n.Capacity, Configured: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
