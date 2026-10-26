package domain

import "time"

type Session struct {
	ID             string    `json:"id"`
	HostHash       string    `json:"host_hash"`
	ControllerHash string    `json:"controller_hash"`
	CodeHash       string    `json:"code_hash"`
	Name           string    `json:"name"`
	State          string    `json:"state"`
	Expires        time.Time `json:"expires"`
	LeaseUntil     time.Time `json:"lease_until"`
	Epoch          uint64    `json:"epoch"`
	Cleanup        bool      `json:"cleanup"`
}
