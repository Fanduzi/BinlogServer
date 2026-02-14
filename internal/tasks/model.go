package tasks

import "time"

type State string

const (
	StateCreated      State = "CREATED"
	StateStarting     State = "STARTING"
	StateRunning      State = "RUNNING"
	StateRetryBackoff State = "RETRY_BACKOFF"
	StateFailed       State = "FAILED"
	StateStopping     State = "STOPPING"
	StateStopped      State = "STOPPED"
)

type Task struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	State     State     `json:"state"`
	LastError string    `json:"last_error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}
