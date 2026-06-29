package models

import "time"

// DeliveryStatus is the lifecycle of a single (event, route, provider) delivery.
// The deliveries table doubles as a durable work queue, so these statuses also
// describe queue position.
type DeliveryStatus string

const (
	// DeliveryPending is queued and ready (or scheduled via NextAttemptAt).
	DeliveryPending DeliveryStatus = "pending"
	// DeliveryInProgress has been claimed by a worker.
	DeliveryInProgress DeliveryStatus = "in_progress"
	// DeliverySuccess was delivered.
	DeliverySuccess DeliveryStatus = "success"
	// DeliveryFailed failed but has retries remaining (NextAttemptAt set).
	DeliveryFailed DeliveryStatus = "failed"
	// DeliveryDead exhausted all retries.
	DeliveryDead DeliveryStatus = "dead"
)

// Delivery is one attempt-tracked notification target plus its history.
type Delivery struct {
	ID            int64          `json:"id"`
	Fingerprint   string         `json:"fingerprint"`
	EventRef      int64          `json:"event_ref"`
	Route         string         `json:"route"`
	Provider      string         `json:"provider"`
	Status        DeliveryStatus `json:"status"`
	AttemptCount  int            `json:"attempt_count"`
	MaxAttempts   int            `json:"max_attempts"`
	NextAttemptAt time.Time      `json:"next_attempt_at"`
	LastError     string         `json:"last_error,omitempty"`
	LastDuration  Duration       `json:"last_duration,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}
