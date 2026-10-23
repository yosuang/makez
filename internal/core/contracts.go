// Package core defines the platform contracts independent of a sandbox backend.
// Types passed here are Activity-local unless explicitly documented otherwise.
package core

import (
	"context"
	"encoding/json"
	"io"
	"time"
)

// TaskState tracks the user-visible lifecycle of a task.
type TaskState string

// Task lifecycle states.
const (
	TaskQueued    TaskState = "QUEUED"
	TaskRunning   TaskState = "RUNNING"
	TaskCanceling TaskState = "CANCELING"
	TaskSucceeded TaskState = "SUCCEEDED"
	TaskFailed    TaskState = "FAILED"
	TaskCanceled  TaskState = "CANCELED"
)

// RunState tracks execution within a task run.
type RunState string

// Execution run lifecycle states.
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

// CleanupState tracks disposal of a run’s sandbox resources.
type CleanupState string

// Sandbox cleanup states.
const (
	CleanupNotRequired CleanupState = "NOT_REQUIRED"
	CleanupPending     CleanupState = "PENDING"
	CleanupDone        CleanupState = "DONE"
	CleanupBlocked     CleanupState = "BLOCKED"
)

// FailureCode classifies provider failures for reconciliation and retry decisions.
type FailureCode string

// Provider failure classifications.
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

// ProviderError classifies a provider operation failure.
// Retry policy is chosen by the caller; a transient transport error may still
// have an unknown external outcome for a mutating operation.
type ProviderError interface {
	error
	Code() FailureCode
}

// OperationKey identifies a stable, retryable operation within one run.
type OperationKey struct {
	WorkspaceID string
	TaskID      string
	RunID       string
	Step        string // Stable logical step; never a Temporal Activity attempt.
}

// Capability identifies a feature offered by a sandbox backend.
type Capability string

// Capabilities advertised by sandbox backends.
const (
	BackgroundProcess Capability = "background_process"
	OperationLookup   Capability = "operation_lookup"
	ReplayableOutput  Capability = "replayable_output"
	ProcessStop       Capability = "process_stop"
	FileAccess        Capability = "file_access"
	FiniteLifetime    Capability = "finite_lifetime"
	NetworkAllowlist  Capability = "network_allowlist"
	SecretInjection   Capability = "secret_injection"
)

// SandboxCapabilities describes supported operations and lifetime limits.
type SandboxCapabilities struct {
	Supported   []Capability
	MaxLifetime time.Duration
	Renewable   bool
}

// ResourceLimits bounds sandbox CPU, memory, and disk allocation.
type ResourceLimits struct {
	MilliCPU    int64
	MemoryBytes int64
	DiskBytes   int64
}

// SandboxRequest specifies the identity, runtime profile, and limits of a sandbox.
type SandboxRequest struct {
	Key                   OperationKey
	RuntimeProfileID      string
	RuntimeProfileVersion string
	ExpiresAt             time.Time
	Limits                ResourceLimits
	NetworkPolicyRef      string
}

// SandboxRef identifies a backend resource and its owning operation.
type SandboxRef struct {
	BackendID string
	ID        string
	Key       OperationKey
}

// SandboxState describes the observed lifecycle of a sandbox resource.
type SandboxState string

// Observed sandbox states.
const (
	SandboxCreating SandboxState = "CREATING"
	SandboxReady    SandboxState = "READY"
	SandboxDeleting SandboxState = "DELETING"
	SandboxAbsent   SandboxState = "ABSENT"
	SandboxUnknown  SandboxState = "UNKNOWN"
)

// SandboxObservation records the owned resources found for an operation key.
type SandboxObservation struct {
	State      SandboxState
	Matches    []SandboxRef // READY requires exactly one owned resource.
	ExpiresAt  time.Time
	ObservedAt time.Time
}

// CommandSpec describes a process invocation without shell interpolation.
type CommandSpec struct {
	Argv    []string // Passed as arguments, never joined into an unescaped shell command.
	Workdir string
	Env     map[string]string // Non-secret values only.
}

// StartRequest binds a command, credentials, and deadline to an operation key.
type StartRequest struct {
	Key          OperationKey
	Command      CommandSpec
	SecretRefs   map[string]string // Versioned references resolved by the Worker, scoped to its workspace.
	SecretValues map[string]string `json:"-"` // Ephemeral transport only; excluded from history, hashes and persistence.
	Deadline     time.Time
}

// ProcessRef identifies an execution receipt within a sandbox.
type ProcessRef struct {
	Sandbox SandboxRef
	ID      string // Stable execution receipt, not an unqualified reusable OS PID.
	Key     OperationKey
}

// ProcessState describes the observed state of a keyed process execution.
type ProcessState string

// Observed process execution states.
const (
	ProcessNotStarted ProcessState = "NOT_STARTED" // Authoritative receipt guarantees no launch.
	ProcessRunning    ProcessState = "RUNNING"
	ProcessExited     ProcessState = "EXITED"
	ProcessStopped    ProcessState = "STOPPED" // Launch key is closed; no process can start later.
	ProcessUnknown    ProcessState = "UNKNOWN"
)

// ProcessExit records how and when a process terminated.
type ProcessExit struct {
	Code       int
	Signal     string
	FinishedAt time.Time
}

// ProcessObservation captures the current receipt and termination state of a process.
type ProcessObservation struct {
	State      ProcessState
	Ref        *ProcessRef
	Exit       *ProcessExit
	ObservedAt time.Time
}

// OutputRecord contains a chunk of process output at a stable source position.
type OutputRecord struct {
	Offset string // Stable source position across reconnects.
	Stream string // stdout or stderr.
	Data   []byte
}

// OutputPage contains replayable output and the cursor for the next read.
type OutputPage struct {
	Records    []OutputRecord
	NextCursor string
	EOF        bool // True only when the process exited and output has been drained.
}

// StopPolicy specifies the signal and grace period for process termination.
type StopPolicy struct {
	Signal      string
	GracePeriod time.Duration
}

// SandboxProvider defines the backend operations used by execution activities.
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

// AdapterIdentity identifies an agent implementation and its version.
type AdapterIdentity struct {
	ID      string
	Version string
}

// InputAsset maps a stored artifact to an agent workspace path.
type InputAsset struct {
	Path        string
	ArtifactRef string
}

// AgentInput contains the task data needed to construct an execution plan.
type AgentInput struct {
	TaskID string
	RunID  string
	Prompt string
	Assets []InputAsset
	Config json.RawMessage
}

// InputFile contains a workspace path and the bytes to write before execution.
type InputFile struct {
	Path string
	Data []byte
}

// AgentPlan specifies the command, inputs, credentials, and expected outputs for a run.
type AgentPlan struct {
	SecretRefs        map[string]string // Destination env name -> versioned secret reference; never a secret value.
	Command           CommandSpec
	Files             []InputFile
	Stop              StopPolicy
	RequiredArtifacts []string
}

// AgentEvent represents an adapter event decoded from process output.
type AgentEvent struct {
	SourceOffset string
	Index        uint32 // Deterministic index when one record produces several events.
	Kind         string
	Payload      json.RawMessage
}

// DecodedOutput contains decoded events and the next bounded parser checkpoint.
type DecodedOutput struct {
	Events    []AgentEvent
	NextState []byte // Bounded parser checkpoint, committed with the output cursor.
}

// ArtifactCandidate identifies a workspace file proposed for archival.
type ArtifactCandidate struct {
	Path      string
	MediaType string
	Required  bool
}

// AgentResult contains the adapter’s outcome and candidate artifacts.
type AgentResult struct {
	Succeeded bool
	Summary   string
	Artifacts []ArtifactCandidate
}

// WorkspaceReader provides read access to the current run’s files.
// Reader is restricted to this Run's workspace; adapters cannot access other
// sandboxes, host files, Temporal, or backend administration APIs.
type WorkspaceReader interface {
	Read(ctx context.Context, path string) (io.ReadCloser, error)
}

// AgentAdapter translates task input and process output into platform contracts.
// Plan and Decode are deterministic for the same input and adapter version.
// The platform owns execution, cancellation, retries, persistence, and cleanup.
type AgentAdapter interface {
	Identity() AdapterIdentity
	Requirements() []Capability
	Plan(input AgentInput) (AgentPlan, error)
	Decode(page OutputPage, previousState []byte) (DecodedOutput, error)
	Collect(ctx context.Context, workspace WorkspaceReader, exit ProcessExit) (AgentResult, error)
}

// SecretResolver authorizes versioned references by workspace. Values are
// resolved only in the execution Activity, never in a Workflow or Adapter.
type SecretResolver interface {
	Check(workspace string, refs map[string]string) error
	Resolve(ctx context.Context, workspace string, refs map[string]string) (map[string]string, error)
}
