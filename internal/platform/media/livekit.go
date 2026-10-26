package media

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type LiveKit struct {
	URL, PublicURL, Key, Secret string
	Client                      *http.Client
}

func (m *LiveKit) token(identity string, grant map[string]any) string {
	now := time.Now()
	payload, _ := json.Marshal(map[string]any{"iss": m.Key, "sub": identity, "nbf": now.Add(-5 * time.Second).Unix(), "exp": now.Add(time.Minute).Unix(), "video": grant})
	enc := base64.RawURLEncoding.EncodeToString
	text := enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc(payload)
	h := hmac.New(sha256.New, []byte(m.Secret))
	h.Write([]byte(text))
	return text + "." + enc(h.Sum(nil))
}
func (m *LiveKit) call(ctx context.Context, method, room string, maxParticipants int) error {
	body, _ := json.Marshal(map[string]any{"name": room, "emptyTimeout": 30, "maxParticipants": maxParticipants})
	if method == "DeleteRoom" {
		body, _ = json.Marshal(map[string]string{"room": room})
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(m.URL, "/")+"/twirp/livekit.RoomService/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.token("", map[string]any{"roomCreate": true}))
	response, err := m.Client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8192))
	if response.StatusCode == 404 && method == "DeleteRoom" {
		return nil
	}
	if response.StatusCode != 200 {
		return fmt.Errorf("media API status %d", response.StatusCode)
	}
	return nil
}

// JoinRequest contains policy already granted by a business use case.
type JoinRequest struct {
	Room, Identity                           string
	MaxParticipants                          int
	CanPublish, CanSubscribe, CanPublishData bool
}

func (m *LiveKit) Connect(ctx context.Context, req JoinRequest) (map[string]any, error) {
	if err := m.call(ctx, "CreateRoom", req.Room, req.MaxParticipants); err != nil {
		return nil, err
	}
	return map[string]any{"media_url": m.PublicURL, "media_token": m.token(req.Identity, map[string]any{
		"roomJoin": true, "room": req.Room, "canPublish": req.CanPublish, "canSubscribe": req.CanSubscribe,
		"canPublishData": req.CanPublishData, "canUpdateOwnMetadata": false}),
		"participant_identity": req.Identity, "expires_at": time.Now().Add(time.Minute).UTC().Format(time.RFC3339)}, nil
}
func (m *LiveKit) Delete(ctx context.Context, room string) error {
	return m.call(ctx, "DeleteRoom", room, 0)
}
