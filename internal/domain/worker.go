package domain

import "time"

type WorkerPresence struct {
	WorkerID   string
	InstanceID string
	Version    string
	LastSeen   time.Time
}
