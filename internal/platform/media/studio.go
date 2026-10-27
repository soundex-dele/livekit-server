package media

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

func (m *LiveKit) rpc(ctx context.Context, method, room string, body any) (map[string]any, error) {
	data, e := json.Marshal(body)
	if e != nil {
		return nil, e
	}
	req, e := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(m.URL, "/")+"/twirp/livekit.RoomService/"+method, bytes.NewReader(data))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.token("", map[string]any{"roomAdmin": true, "room": room, "roomList": true, "roomCreate": true}))
	response, e := m.Client.Do(req)
	if e != nil {
		return nil, e
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound && (method == "RemoveParticipant" || method == "DeleteRoom") {
		return map[string]any{}, nil
	}
	if response.StatusCode != 200 {
		return nil, &RPCError{Status: response.StatusCode}
	}
	var out map[string]any
	e = json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(&out)
	return out, e
}
func (m *LiveKit) ConnectStudio(ctx context.Context, room, who, grant string, publish, subscribe bool) (map[string]any, error) {
	if _, e := m.rpc(ctx, "CreateRoom", room, map[string]any{"name": room, "emptyTimeout": 60, "maxParticipants": 24}); e != nil {
		return nil, e
	}
	now := time.Now()
	claims := map[string]any{"iss": m.Key, "sub": who, "nbf": now.Add(-5 * time.Second).Unix(), "exp": now.Add(time.Minute).Unix(), "studio_grant": grant, "video": map[string]any{"roomJoin": true, "room": room, "canPublish": publish, "canSubscribe": subscribe, "canPublishData": false, "canUpdateOwnMetadata": false}}
	payload, e := json.Marshal(claims)
	if e != nil {
		return nil, e
	}
	enc := base64.RawURLEncoding.EncodeToString
	data := enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc(payload)
	mac := hmac.New(sha256.New, []byte(m.Secret))
	_, _ = mac.Write([]byte(data))
	token := data + "." + enc(mac.Sum(nil))
	return map[string]any{"media_url": m.PublicURL, "media_token": token, "participant_identity": who, "expires_at": now.Add(time.Minute).UTC().Format(time.RFC3339)}, nil
}
func (m *LiveKit) Published(ctx context.Context, room, who string) (bool, error) {
	p, e := m.rpc(ctx, "GetParticipant", room, map[string]string{"room": room, "identity": who})
	if e != nil {
		return false, e
	}
	tracks, _ := p["tracks"].([]any)
	return len(tracks) > 0, nil
}
func (m *LiveKit) Remove(ctx context.Context, room, who string) error {
	_, e := m.rpc(ctx, "RemoveParticipant", room, map[string]string{"room": room, "identity": who})
	return e
}
