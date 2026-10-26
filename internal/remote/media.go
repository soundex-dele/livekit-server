package remote

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
func (m *LiveKit) call(ctx context.Context, method, room string) error {
	body, _ := json.Marshal(map[string]any{"name": room, "emptyTimeout": 30, "maxParticipants": 2})
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
func (m *LiveKit) Connect(ctx context.Context, s Session, role string) (map[string]any, error) {
	room := "rc_" + s.ID
	if err := m.call(ctx, "CreateRoom", room); err != nil {
		return nil, err
	}
	identity := role + "_" + s.ID
	return map[string]any{"media_url": m.PublicURL, "media_token": m.token(identity, map[string]any{"roomJoin": true, "room": room, "canPublish": role == "host", "canSubscribe": true, "canPublishData": true, "canUpdateOwnMetadata": false}), "participant_identity": identity, "expires_at": time.Now().Add(time.Minute).UTC().Format(time.RFC3339)}, nil
}
func (m *LiveKit) Delete(ctx context.Context, room string) error {
	return m.call(ctx, "DeleteRoom", room)
}
