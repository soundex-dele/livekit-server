package media

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

type RPCError struct{ Status int }

func (e *RPCError) Error() string { return fmt.Sprintf("media API status %d", e.Status) }
func (m *LiveKit) ConferenceRPC(c context.Context, method, room string, body any) (map[string]any, error) {
	return m.rpc(c, method, room, body)
}
func (m *LiveKit) ConnectConference(c context.Context, room, who, name, grant string, share bool) (map[string]any, error) {
	if _, e := m.rpc(c, "CreateRoom", room, map[string]any{"name": room, "emptyTimeout": 180, "maxParticipants": 25}); e != nil {
		return nil, e
	}
	now := time.Now()
	sources := []string{"camera", "microphone"}
	if share {
		sources = append(sources, "screen_share", "screen_share_audio")
	}
	payload, e := json.Marshal(map[string]any{"iss": m.Key, "sub": who, "name": name, "nbf": now.Add(-5 * time.Second).Unix(), "exp": now.Add(time.Minute).Unix(), "conference_grant": grant, "video": map[string]any{"roomJoin": true, "room": room, "canPublish": true, "canSubscribe": true, "canPublishData": true, "canUpdateOwnMetadata": false, "canPublishSources": sources}})
	if e != nil {
		return nil, e
	}
	enc := base64.RawURLEncoding.EncodeToString
	data := enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc(payload)
	mac := hmac.New(sha256.New, []byte(m.Secret))
	mac.Write([]byte(data))
	return map[string]any{"media_url": m.PublicURL, "media_token": data + "." + enc(mac.Sum(nil)), "participant_identity": who, "room": room, "expires_at": now.Add(time.Minute).UTC().Format(time.RFC3339)}, nil
}
