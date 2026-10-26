package infrastructure

import (
	"av-platform/business/internal/businesses/remotecontrol/domain"
	"av-platform/business/internal/platform/media"
	"context"
	"fmt"
)

// Media translates remote-control roles into shared media capabilities.
type Media struct{ Client *media.LiveKit }

func (m *Media) Connect(ctx context.Context, s domain.Session, role string) (map[string]any, error) {
	if role != "host" && role != "controller" {
		return nil, fmt.Errorf("unknown remote-control role")
	}
	return m.Client.Connect(ctx, media.JoinRequest{Room: "rc_" + s.ID, Identity: role + "_" + s.ID,
		MaxParticipants: 2, CanPublish: role == "host", CanSubscribe: true, CanPublishData: true})
}
func (m *Media) Delete(ctx context.Context, room string) error { return m.Client.Delete(ctx, room) }
