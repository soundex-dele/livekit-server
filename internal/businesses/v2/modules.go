package v2

import (
	"av-platform/business/internal/platform/control"
	"context"
)

type commandHandler func(*Service, context.Context, control.Principal, string, []string, command, string) (any, error)

// A business owns its URL prefixes, resource kind and command entry points.
// Registration is compile-time: there is no runtime plugin or service locator.
type businessModule struct {
	name, kind string
	handlers   map[string]commandHandler
}

var businessModules = []businessModule{
	{"screen-cast", "cast", map[string]commandHandler{"screen-cast": (*Service).cast}},
	{"conference", "meeting", map[string]commandHandler{"conference": (*Service).meeting}},
	{"remote-control", "assist", map[string]commandHandler{"remote-control": (*Service).assist, "control-grants": (*Service).controlGrant}},
	{"live-studio", "broadcast", map[string]commandHandler{"live-studio": (*Service).live}},
}

// NewWithModules enables only explicitly selected businesses. An empty list
// disables all business commands and admission; maintenance still closes their
// persisted sessions so removing a module cannot strand an active media room.
// Configuration validation belongs to the composition root.
func NewWithModules(p *control.Platform, names []string) *Service {
	s := &Service{P: p, modules: make(map[string]bool, len(names))}
	for _, name := range names {
		s.modules[name] = true
	}
	return s
}

func (s *Service) moduleForKind(kind string) bool {
	for _, module := range businessModules {
		if module.kind == kind {
			return s.modules[module.name]
		}
	}
	return false
}

func (s *Service) handler(prefix string) (commandHandler, bool) {
	for _, module := range businessModules {
		if handler, ok := module.handlers[prefix]; ok {
			return handler, s.modules[module.name]
		}
	}
	return nil, false
}
