package conference

import (
	"av-platform/business/internal/platform/media"
	"context"
	"errors"
)

type LiveMedia struct{ Client *media.LiveKit }

func (m *LiveMedia) Credentials(c context.Context, r, i, n, g string, share bool) (map[string]any, error) {
	return m.Client.ConnectConference(c, r, i, n, g, share)
}
func (m *LiveMedia) Participants(c context.Context, r string) ([]Peer, error) {
	raw, e := m.Client.ConferenceRPC(c, "ListParticipants", r, map[string]any{"room": r})
	if e != nil {
		return nil, e
	}
	out := []Peer{}
	ps, _ := raw["participants"].([]any)
	for _, v := range ps {
		p, ok := v.(map[string]any)
		if !ok {
			continue
		}
		who, _ := p["identity"].(string)
		peer := Peer{Identity: who}
		ts, _ := p["tracks"].([]any)
		for _, v := range ts {
			t, ok := v.(map[string]any)
			if !ok {
				continue
			}
			sid, _ := t["sid"].(string)
			muted, _ := t["muted"].(bool)
			source := 0
			switch t["source"] {
			case "MICROPHONE", float64(2):
				source = 2
			case "SCREEN_SHARE", float64(3):
				source = 3
			}
			peer.Tracks = append(peer.Tracks, Track{sid, source, muted})
		}
		out = append(out, peer)
	}
	return out, nil
}
func absentIsDone(e error) error {
	var rpc *media.RPCError
	if errors.As(e, &rpc) && rpc.Status == 404 {
		return nil
	}
	return e
}
func (m *LiveMedia) Remove(c context.Context, r, i string) error {
	return absentIsDone(m.Client.Remove(c, r, i))
}
func (m *LiveMedia) Delete(c context.Context, r string) error {
	_, e := m.Client.ConferenceRPC(c, "DeleteRoom", r, map[string]any{"room": r})
	return absentIsDone(e)
}
func (m *LiveMedia) Mute(c context.Context, r, i, t string) error {
	_, e := m.Client.ConferenceRPC(c, "MutePublishedTrack", r, map[string]any{"room": r, "identity": i, "trackSid": t, "muted": true})
	return absentIsDone(e)
}
func (m *LiveMedia) Sharing(c context.Context, r, i string, on bool) error {
	sources := []string{"CAMERA", "MICROPHONE"}
	if on {
		sources = append(sources, "SCREEN_SHARE", "SCREEN_SHARE_AUDIO")
	}
	_, e := m.Client.ConferenceRPC(c, "UpdateParticipant", r, map[string]any{"room": r, "identity": i, "permission": map[string]any{"canPublish": true, "canSubscribe": true, "canPublishData": true, "canPublishSources": sources}})
	var rpc *media.RPCError
	if errors.As(e, &rpc) && rpc.Status == 404 {
		return nil
	}
	return e
}
