package application

import (
	"av-platform/business/internal/businesses/remotecontrol/domain"
	"context"
)

// Store commits the session and audit action atomically. Its lifecycle belongs to bootstrap.
type Store interface {
	Load(context.Context) ([]domain.Session, error)
	Save(context.Context, domain.Session, string) error
}
type Command struct {
	Action, ID, Credential, Code, Name, Source string
}
type Result struct {
	Data    map[string]any
	Created bool
}
type Error struct{ Code string }

func (e *Error) Error() string { return e.Code }
