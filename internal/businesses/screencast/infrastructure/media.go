package infrastructure

import (
	"av-platform/business/internal/businesses/screencast/domain"
	"av-platform/business/internal/platform/media"
	"context"
)

type Media struct{ Client *media.LiveKit }

func (m *Media) Connect(ctx context.Context, v domain.Session, role string) (map[string]any, error) {
	return m.Client.Connect(ctx, media.JoinRequest{Room: v.Room, Identity: role + "-" + v.ID, MaxParticipants: 2, CanPublish: role == "sender", CanSubscribe: role == "receiver", CanPublishData: false})
}
func (m *Media) Delete(ctx context.Context, room string) error { return m.Client.Delete(ctx, room) }
