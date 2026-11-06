// Package control supplies AV-specific control-plane capabilities. Media data
// never traverses this package; durable authorization and room ownership do.
package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"platform-foundation/server/access"
	"platform-foundation/server/httpx"
	"platform-foundation/server/persistence"
	"platform-foundation/server/taskqueue"
)

type Principal struct {
	ID          string   `json:"id"`
	Scope       string   `json:"scope"`
	Kind        string   `json:"kind"`
	ResourceID  string   `json:"resource_id,omitempty"`
	Permissions []string `json:"permissions,omitempty"`
}

func (p Principal) Can(permission string) bool {
	for _, v := range p.Permissions {
		if v == "*" || v == permission {
			return true
		}
	}
	return false
}
func (p Principal) IsUser() bool { return p.Kind == "user" }
func FromActor(a access.Actor) Principal {
	kind := "user"
	if a.APIKey {
		kind = "api_key"
	}
	return Principal{ID: a.User.ID, Scope: a.Scope, Kind: kind, Permissions: a.Permissions}
}

type Resource struct {
	ID      string `json:"id"`
	Scope   string `json:"scope"`
	Kind    string `json:"kind"`
	Owner   string `json:"owner"`
	Title   string `json:"title"`
	State   string `json:"state"`
	Version int64  `json:"version"`
	Created int64  `json:"created"`
	Updated int64  `json:"updated"`
}
type Capabilities struct {
	Publish   bool     `json:"publish"`
	Subscribe bool     `json:"subscribe"`
	Data      bool     `json:"data"`
	Sources   []string `json:"sources,omitempty"`
}
type RTCGrant struct {
	ID            string `json:"id"`
	Identity      string `json:"identity"`
	BindingID     string `json:"binding_id"`
	PolicyVersion int64  `json:"policy_version"`
}
type Event struct {
	ID         string `json:"id"`
	ResourceID string `json:"resource_id"`
	Sequence   int64  `json:"sequence"`
	Type       string `json:"type"`
	Payload    any    `json:"payload"`
	Created    int64  `json:"created"`
}

// Missing ephemeral keys return ErrMissing. Renew MUST NOT recreate an absent
// key: in particular, an expired control lease needs a new local approval.
var ErrMissing = errors.New("ephemeral_missing")

type Ephemeral interface {
	Set(context.Context, string, string, time.Duration) error
	Get(context.Context, string) (string, error)
	Delete(context.Context, string) error
	Create(context.Context, string, string, time.Duration) (bool, error)
	Renew(context.Context, string, string, time.Duration) (bool, error)
	Take(context.Context, string) (string, error)
	Allow(context.Context, string, int, time.Duration) (bool, error)
	Publish(context.Context, string, string) error
}
type Subscription interface {
	Subscribe(context.Context, string) (<-chan string, error)
}
type Health interface{ Ping(context.Context) error }

type Platform struct {
	Store           *persistence.Store
	Queue           *taskqueue.Queue
	Ephemeral       Ephemeral
	Now             func() time.Time
	Nodes           []Node
	Media           MediaClient
	ValidateSubject func(context.Context, Principal) error
	AuthorizeGrant  func(context.Context, string, string, string) error
}

func New(s *persistence.Store, e Ephemeral, nodes []Node) *Platform {
	return &Platform{Store: s, Queue: taskqueue.New(s), Ephemeral: e, Now: time.Now, Nodes: nodes, Media: &HTTPMedia{}}
}
func ID() string {
	var b [24]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func Hash(v string) string               { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func Fail(status int, code string) error { return httpx.NewHTTPError(status, code, status >= 500, nil) }
func validScope(scope string) bool       { return len(scope) > 4 && scope[:4] == "org:" }
func (p Principal) AuthorizeResource(r Resource) error {
	if !validScope(p.Scope) || p.Scope != r.Scope || p.ID == "" || (p.ResourceID != "" && p.ResourceID != r.ID) {
		return Fail(403, "resource_forbidden")
	}
	return nil
}
func AuthorizeResource(p Principal, r Resource) error { return p.AuthorizeResource(r) }
