package httpx

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
)

func Reply(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
func Fail(w http.ResponseWriter, status int, code string) {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	Reply(w, status, map[string]any{"error": map[string]any{"code": code, "message": code, "retryable": status >= 500}, "request_id": hex.EncodeToString(id[:])})
}
