package api

import (
	"encoding/json"
	"time"

	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/store"
)

// ErrorResponse contains the public API error message.
type ErrorResponse struct {
	Error     string `json:"error"`
	Code      string `json:"code"`
	RequestID string `json:"request_id"`
}

// SubmitRequest contains the accepted task submission fields.
type SubmitRequest struct {
	// Prompt is required and must contain 1–65536 UTF-8 bytes after checking for blank input.
	Prompt         string          `json:"prompt" validate:"required"`
	Config         json.RawMessage `json:"config,omitempty" swaggertype:"object"`
	AgentID        string          `json:"agent_id" default:"demo"`
	AgentVersion   string          `json:"agent_version" default:"1"`
	ProfileID      string          `json:"profile_id" default:"default"`
	ProfileVersion string          `json:"profile_version" default:"1"`
	// Deadline and timeout_seconds are mutually exclusive; an explicit deadline must be 5 seconds to 90 minutes in the future.
	Deadline       time.Time `json:"deadline"`
	TimeoutSeconds int       `json:"timeout_seconds,omitempty" minimum:"5" maximum:"5400" default:"3600"`
}

// TaskResponse contains the public task state and its current run.
type TaskResponse struct {
	ID              string          `json:"id"`
	WorkspaceID     string          `json:"workspace_id"`
	Prompt          string          `json:"prompt"`
	Config          json.RawMessage `json:"config,omitempty" swaggertype:"object"`
	AgentID         string          `json:"agent_id"`
	AgentVersion    string          `json:"agent_version"`
	ProfileID       string          `json:"profile_id"`
	ProfileVersion  string          `json:"profile_version"`
	State           core.TaskState  `json:"state"`
	Reason          string          `json:"reason,omitempty"`
	Deadline        time.Time       `json:"deadline"`
	CancelRequested bool            `json:"cancel_requested"`
	Run             *RunResponse    `json:"run,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	DispatchError   string          `json:"dispatch_error,omitempty"`
}

// RunResponse exposes execution state without private parser or plan data.
type RunResponse struct {
	ID              string            `json:"id"`
	TaskID          string            `json:"task_id"`
	State           core.RunState     `json:"state"`
	BackendID       string            `json:"backend_id"`
	Sandbox         *core.SandboxRef  `json:"sandbox,omitempty"`
	Process         *core.ProcessRef  `json:"process,omitempty"`
	Cursor          string            `json:"cursor"`
	ObserverEpoch   int64             `json:"observer_epoch"`
	Exit            *core.ProcessExit `json:"exit,omitempty"`
	Cleanup         core.CleanupState `json:"cleanup_state"`
	VerifiedStopped bool              `json:"verified_stopped"`
}

// ArtifactResponse exposes artifact metadata without its storage key.
type ArtifactResponse struct {
	ID        string `json:"id"`
	RunID     string `json:"run_id"`
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
}

// EventResponse contains a replayable task event.
type EventResponse struct {
	Seq       int64           `json:"seq"`
	RunID     string          `json:"run_id"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload" swaggertype:"object"`
	CreatedAt time.Time       `json:"created_at"`
}

func taskResponseDTO(t store.Task) TaskResponse {
	v := TaskResponse{ID: t.ID, WorkspaceID: t.WorkspaceID, Prompt: t.Prompt, Config: t.Config, AgentID: t.AgentID, AgentVersion: t.AgentVersion, ProfileID: t.ProfileID, ProfileVersion: t.ProfileVersion, State: t.State, Reason: t.Reason, Deadline: t.Deadline, CancelRequested: t.CancelRequested, CreatedAt: t.CreatedAt, DispatchError: t.DispatchError}
	if r := t.Run; r != nil {
		v.Run = &RunResponse{ID: r.ID, TaskID: r.TaskID, State: r.State, BackendID: r.BackendID, Sandbox: r.Sandbox, Process: r.Process, Cursor: r.Cursor, ObserverEpoch: r.ObserverEpoch, Exit: r.Exit, Cleanup: r.Cleanup, VerifiedStopped: r.VerifiedStopped}
	}
	return v
}
func taskResponses(tasks []store.Task) []TaskResponse {
	result := make([]TaskResponse, 0, len(tasks))
	for _, t := range tasks {
		result = append(result, taskResponseDTO(t))
	}
	return result
}
func artifactResponse(a store.Artifact) ArtifactResponse {
	return ArtifactResponse{ID: a.ID, RunID: a.RunID, Path: a.Path, SHA256: a.SHA256, MediaType: a.MediaType, Size: a.Size}
}
