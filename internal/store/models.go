package store

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/makez-dev/makez/internal/core"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrInvalidInput = errors.New("invalid input")
	ErrConflict     = errors.New("conflicting request or state")
	ErrStale        = errors.New("stale observer")
)

type SubmitInput struct {
	Prompt         string          `json:"prompt"`
	Config         json.RawMessage `json:"config,omitempty"`
	AgentID        string          `json:"agent_id"`
	AgentVersion   string          `json:"agent_version"`
	ProfileID      string          `json:"profile_id"`
	ProfileVersion string          `json:"profile_version"`
	Deadline       time.Time       `json:"deadline"`
	TimeoutSeconds int             `json:"timeout_seconds,omitempty"`
}

type Task struct {
	ID              string          `json:"id"`
	WorkspaceID     string          `json:"workspace_id"`
	Prompt          string          `json:"prompt"`
	Config          json.RawMessage `json:"config,omitempty"`
	AgentID         string          `json:"agent_id"`
	AgentVersion    string          `json:"agent_version"`
	ProfileID       string          `json:"profile_id"`
	ProfileVersion  string          `json:"profile_version"`
	State           core.TaskState  `json:"state"`
	Reason          string          `json:"reason,omitempty"`
	Deadline        time.Time       `json:"deadline"`
	CancelRequested bool            `json:"cancel_requested"`
	Run             *Run            `json:"run,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	DispatchError   string          `json:"dispatch_error,omitempty"`
}

type Run struct {
	ID              string            `json:"id"`
	TaskID          string            `json:"task_id"`
	State           core.RunState     `json:"state"`
	BackendID       string            `json:"backend_id"`
	Sandbox         *core.SandboxRef  `json:"sandbox,omitempty"`
	Process         *core.ProcessRef  `json:"process,omitempty"`
	Cursor          string            `json:"cursor"`
	ParserState     []byte            `json:"-"`
	ObserverEpoch   int64             `json:"observer_epoch"`
	Exit            *core.ProcessExit `json:"exit,omitempty"`
	Cleanup         core.CleanupState `json:"cleanup_state"`
	VerifiedStopped bool              `json:"verified_stopped"`
	Result          *PreparedResult   `json:"-"`
	Plan            *core.AgentPlan   `json:"-"`
}

type RunUpdate struct {
	State           *core.RunState
	Sandbox         *core.SandboxRef
	Process         *core.ProcessRef
	Cursor          *string
	ParserState     *[]byte
	Exit            *core.ProcessExit
	Cleanup         *core.CleanupState
	VerifiedStopped *bool
	Result          *PreparedResult
}

type Artifact struct {
	ID         string `json:"id"`
	RunID      string `json:"run_id"`
	Path       string `json:"path"`
	StorageKey string `json:"-"`
	SHA256     string `json:"sha256"`
	MediaType  string `json:"media_type"`
	Size       int64  `json:"size"`
}

type Event struct {
	Seq       int64           `json:"seq"`
	RunID     string          `json:"run_id"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

type Outbox struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	TaskID      string `json:"task_id"`
	RunID       string `json:"run_id"`
	Command     string `json:"command"`
	Attempts    int    `json:"attempts"`
	LastError   string `json:"last_error"`
}

func IsTerminal(state core.TaskState) bool {
	return state == core.TaskSucceeded || state == core.TaskFailed || state == core.TaskCanceled
}

// PreparedResult is durable before deleting the only workspace copy.
type PreparedResult struct {
	State     core.TaskState `json:"state"`
	Reason    string         `json:"reason"`
	Artifacts []Artifact     `json:"artifacts"`
}

type storedArtifact struct {
	Artifact
	StorageKey string `json:"storage_key"`
}

func (r PreparedResult) MarshalJSON() ([]byte, error) {
	artifacts := make([]storedArtifact, len(r.Artifacts))
	for i, a := range r.Artifacts {
		artifacts[i] = storedArtifact{Artifact: a, StorageKey: a.StorageKey}
	}
	return json.Marshal(struct {
		State     core.TaskState   `json:"state"`
		Reason    string           `json:"reason"`
		Artifacts []storedArtifact `json:"artifacts"`
	}{r.State, r.Reason, artifacts})
}
func (r *PreparedResult) UnmarshalJSON(data []byte) error {
	var stored struct {
		State     core.TaskState   `json:"state"`
		Reason    string           `json:"reason"`
		Artifacts []storedArtifact `json:"artifacts"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}
	r.State, r.Reason = stored.State, stored.Reason
	r.Artifacts = make([]Artifact, len(stored.Artifacts))
	for i, a := range stored.Artifacts {
		r.Artifacts[i] = a.Artifact
		r.Artifacts[i].StorageKey = a.StorageKey
	}
	return nil
}
