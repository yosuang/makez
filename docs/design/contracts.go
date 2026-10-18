// Package design contains the MVP contract proposal, not a runtime implementation.
// Types passed here are Activity-local unless explicitly documented otherwise.
package design

import (
	"context"
	"encoding/json"
	"io"
	"time"
)

type TaskState string

const (
	TaskQueued    TaskState = "QUEUED"
	TaskRunning   TaskState = "RUNNING"
	TaskCanceling TaskState = "CANCELING"
	TaskSucceeded TaskState = "SUCCEEDED"
	TaskFailed    TaskState = "FAILED"
	TaskCanceled  TaskState = "CANCELED"
)

type RunState string

const (
	RunPreparing  RunState = "PREPARING"
	RunStarting   RunState = "STARTING"
	RunRunning    RunState = "RUNNING"
	RunStopping   RunState = "STOPPING"
	RunFinalizing RunState = "FINALIZING"
	RunSucceeded  RunState = "SUCCEEDED"
	RunFailed     RunState = "FAILED"
	RunCanceled   RunState = "CANCELED"
)

type CleanupState string

const (
	CleanupNotRequired CleanupState = "NOT_REQUIRED"
	CleanupPending     CleanupState = "PENDING"
	CleanupDone        CleanupState = "DONE"
	CleanupBlocked     CleanupState = "BLOCKED"
)

type FailureCode string

const (
	InvalidInput     FailureCode = "INVALID_INPUT"
	Unsupported      FailureCode = "UNSUPPORTED_CAPABILITY"
	PermissionDenied FailureCode = "PERMISSION_DENIED"
	Transient        FailureCode = "TRANSIENT"
	OutcomeUnknown   FailureCode = "OUTCOME_UNKNOWN"
	ResourceLost     FailureCode = "RESOURCE_LOST"
	OutputGap        FailureCode = "OUTPUT_GAP"
	Conflict         FailureCode = "CONFLICT"
)

// Retry policy is chosen by the caller; a transient transport error may still
// have an unknown external outcome for a mutating operation.
type ProviderError interface {
	error
	Code() FailureCode
}

type OperationKey struct {
	WorkspaceID string
	TaskID      string
	RunID       string
	Step        string // Stable logical step; never a Temporal Activity attempt.
}

type Capability string

const (
	BackgroundProcess Capability = "background_process"
	OperationLookup   Capability = "operation_lookup"
	ReplayableOutput  Capability = "replayable_output"
	ProcessStop       Capability = "process_stop"
	FileAccess        Capability = "file_access"
	FiniteLifetime    Capability = "finite_lifetime"
	NetworkAllowlist  Capability = "network_allowlist"
)

type SandboxCapabilities struct {
	Supported   []Capability
	MaxLifetime time.Duration
	Renewable   bool
}

type ResourceLimits struct {
	MilliCPU    int64
	MemoryBytes int64
	DiskBytes   int64
}

type SandboxRequest struct {
	Key                   OperationKey
	RuntimeProfileID      string
	RuntimeProfileVersion string
	ExpiresAt             time.Time
	Limits                ResourceLimits
	NetworkPolicyRef      string
}

type SandboxRef struct {
	BackendID string
	ID        string
	Key       OperationKey
}

type SandboxState string

const (
	SandboxCreating SandboxState = "CREATING"
	SandboxReady    SandboxState = "READY"
	SandboxDeleting SandboxState = "DELETING"
	SandboxAbsent   SandboxState = "ABSENT"
	SandboxUnknown  SandboxState = "UNKNOWN"
)

type SandboxObservation struct {
	State      SandboxState
	Matches    []SandboxRef // READY requires exactly one owned resource.
	ExpiresAt  time.Time
	ObservedAt time.Time
}

type CommandSpec struct {
	Argv    []string // Passed as arguments, never joined into an unescaped shell command.
	Workdir string
	Env     map[string]string // Non-secret values only.
}

type StartRequest struct {
	Key        OperationKey
	Command    CommandSpec
	SecretRefs map[string]string // Resolved at execution time; values are not secrets.
	Deadline   time.Time
}

type ProcessRef struct {
	Sandbox SandboxRef
	ID      string // Stable execution receipt, not an unqualified reusable OS PID.
	Key     OperationKey
}

type ProcessState string

const (
	ProcessNotStarted ProcessState = "NOT_STARTED" // Authoritative receipt guarantees no launch.
	ProcessRunning    ProcessState = "RUNNING"
	ProcessExited     ProcessState = "EXITED"
	ProcessStopped    ProcessState = "STOPPED" // Launch key is closed; no process can start later.
	ProcessUnknown    ProcessState = "UNKNOWN"
)

type ProcessExit struct {
	Code       int
	Signal     string
	FinishedAt time.Time
}

type ProcessObservation struct {
	State      ProcessState
	Ref        *ProcessRef
	Exit       *ProcessExit
	ObservedAt time.Time
}

type OutputRecord struct {
	Offset string // Stable source position across reconnects.
	Stream string // stdout or stderr.
	Data   []byte
}

type OutputPage struct {
	Records    []OutputRecord
	NextCursor string
	EOF        bool // True only when the process exited and output has been drained.
}

type StopPolicy struct {
	Signal      string
	GracePeriod time.Duration
}

// Each configured backend is one provider instance. Ensure/Start are keyed
// operations: an uncertain outcome must be reconciled, never blindly repeated.
type SandboxProvider interface {
	BackendID() string
	Capabilities(ctx context.Context, profileID, profileVersion string) (SandboxCapabilities, error)
	Ensure(ctx context.Context, request SandboxRequest) (SandboxRef, error)
	Inspect(ctx context.Context, key OperationKey) (SandboxObservation, error)
	Renew(ctx context.Context, sandbox SandboxRef, expiresAt time.Time) error
	Delete(ctx context.Context, key OperationKey) error

	Start(ctx context.Context, sandbox SandboxRef, request StartRequest) (ProcessRef, error)
	Process(ctx context.Context, sandbox SandboxRef, key OperationKey) (ProcessObservation, error)
	Output(ctx context.Context, process ProcessRef, cursor string, maxBytes int) (OutputPage, error)
	Stop(ctx context.Context, sandbox SandboxRef, key OperationKey, policy StopPolicy) error

	ReadFile(ctx context.Context, sandbox SandboxRef, path string) (io.ReadCloser, error)
	WriteFile(ctx context.Context, sandbox SandboxRef, path string, contents io.Reader, size int64) error
}

type AdapterIdentity struct {
	ID      string
	Version string
}

type InputAsset struct {
	Path        string
	ArtifactRef string
}

type AgentInput struct {
	TaskID string
	RunID  string
	Prompt string
	Assets []InputAsset
	Config json.RawMessage
}

type InputFile struct {
	Path string
	Data []byte
}

type AgentPlan struct {
	Command           CommandSpec
	Files             []InputFile
	Stop              StopPolicy
	RequiredArtifacts []string
}

type AgentEvent struct {
	SourceOffset string
	Index        uint32 // Deterministic index when one record produces several events.
	Kind         string
	Payload      json.RawMessage
}

type DecodedOutput struct {
	Events    []AgentEvent
	NextState []byte // Bounded parser checkpoint, committed with the output cursor.
}

type ArtifactCandidate struct {
	Path      string
	MediaType string
	Required  bool
}

type AgentResult struct {
	Succeeded bool
	Summary   string
	Artifacts []ArtifactCandidate
}

// Reader is restricted to this Run's workspace; adapters cannot access other
// sandboxes, host files, Temporal, or backend administration APIs.
type WorkspaceReader interface {
	Read(ctx context.Context, path string) (io.ReadCloser, error)
}

// Plan and Decode are deterministic for the same input and adapter version.
// The platform owns execution, cancellation, retries, persistence, and cleanup.
type AgentAdapter interface {
	Identity() AdapterIdentity
	Requirements() []Capability
	Plan(input AgentInput) (AgentPlan, error)
	Decode(page OutputPage, previousState []byte) (DecodedOutput, error)
	Collect(ctx context.Context, workspace WorkspaceReader, exit ProcessExit) (AgentResult, error)
}
