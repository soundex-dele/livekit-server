package domain

import "time"

type Session struct {
	ID, Room, State, Reason                   string
	CodeHash, ReceiverHash, SenderHash        string
	Expires, Paired, ReceiverSeen, SenderSeen time.Time
	ReceiverConnected, SenderConnected        bool
	Cleanup                                   bool
	CleanupUntil                              time.Time
}

func (s Session) Terminal() bool { return s.State == "ended" || s.State == "expired" }
