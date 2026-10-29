// Package store persists tasks, runs, events, and dispatch commands in PostgreSQL.
package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/store/dbgen"
	"github.com/makez-dev/makez/internal/telemetry"
)

// Store owns the PostgreSQL connection pool and persistence metrics.
type Store struct {
	pool    *pgxpool.Pool
	Metrics *telemetry.Metrics
}

// Open opens a store with the default connection limit.
func Open(ctx context.Context, url string) (*Store, error) {
	return OpenWithLimit(ctx, url, 16)
}

// OpenWithLimit fixes a per-process connection budget independent of CPU count.
func OpenWithLimit(ctx context.Context, url string, maxConns int32) (*Store, error) {
	if maxConns < 2 || maxConns > 256 {
		return nil, fmt.Errorf("database pool limit must be 2–256")
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	config.MaxConns = maxConns
	if config.MinConns > maxConns || config.MinIdleConns > maxConns {
		return nil, fmt.Errorf("database pool minimum exceeds budget")
	}
	config.ConnConfig.ConnectTimeout = 5 * time.Second
	// Bound application statements and abandoned transactions independently of HTTP
	// timeouts. Explicit migration connections use their own larger deadline.
	if _, ok := config.ConnConfig.RuntimeParams["statement_timeout"]; !ok {
		config.ConnConfig.RuntimeParams["statement_timeout"] = "15000"
	}
	if _, ok := config.ConnConfig.RuntimeParams["lock_timeout"]; !ok {
		config.ConnConfig.RuntimeParams["lock_timeout"] = "5000"
	}
	if _, ok := config.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"]; !ok {
		config.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "30000"
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

// Close releases all database connections.
func (s *Store) Close() { s.pool.Close() }

func newID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(value[:])
}

func canonicalConfig(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("config must be a JSON object: %w", err)
	}
	if value == nil {
		return nil, fmt.Errorf("config must be a JSON object")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config must contain one JSON object")
	}
	return json.Marshal(value)
}

func taskFromRow(r dbgen.GetTaskRow) Task {
	return Task{ID: r.ID, WorkspaceID: r.WorkspaceID, Prompt: r.Prompt, Config: r.Config, AgentID: r.AgentID, AgentVersion: r.AgentVersion, ProfileID: r.ProfileID, ProfileVersion: r.ProfileVersion, State: core.TaskState(r.State), Reason: r.Reason, Deadline: r.Deadline.Time, CancelRequested: r.CancelRequested, CreatedAt: r.CreatedAt.Time, DispatchError: r.DispatchError}
}
func runFromRow(row dbgen.MakezRun, err error) (*Run, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r := &Run{ID: row.ID, TaskID: row.TaskID, State: core.RunState(row.State), BackendID: row.BackendID, Cursor: row.Cursor, ParserState: row.ParserState, ObserverEpoch: row.ObserverEpoch, Cleanup: core.CleanupState(row.Cleanup), VerifiedStopped: row.VerifiedStopped}
	if row.StartedAt.Valid {
		r.StartedAt = new(row.StartedAt.Time)
	}
	for _, field := range []struct {
		raw    []byte
		target any
	}{{row.Sandbox, &r.Sandbox}, {row.Process, &r.Process}, {row.ProcessExit, &r.Exit}, {row.PreparedResult, &r.Result}, {row.AgentPlan, &r.Plan}} {
		if len(field.raw) > 0 {
			if err = json.Unmarshal(field.raw, field.target); err != nil {
				return nil, err
			}
		}
	}
	return r, nil
}
func loadRun(ctx context.Context, q *dbgen.Queries, taskID string) (*Run, error) {
	return runFromRow(q.GetRunByTask(ctx, taskID))
}
func taskInTx(ctx context.Context, tx pgx.Tx, workspace, id string) (Task, error) {
	q := dbgen.New(tx)
	row, err := q.LockTask(ctx, dbgen.LockTaskParams{WorkspaceID: workspace, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, err
	}
	t := taskFromRow(dbgen.GetTaskRow(row))
	t.Run, err = loadRun(ctx, q, id)
	return t, err
}
func enqueue(ctx context.Context, tx pgx.Tx, t Task, command string) error {
	runID := ""
	if t.Run != nil {
		runID = t.Run.ID
	}
	_, err := dbgen.New(tx).EnqueueCommand(ctx, dbgen.EnqueueCommandParams{ID: newID(), WorkspaceID: t.WorkspaceID, TaskID: t.ID, RunID: runID, Command: command})
	return err
}

// Submit creates a task and dispatch command, or returns its idempotent replay.
func (s *Store) Submit(ctx context.Context, workspace, key string, input SubmitInput) (Task, bool, error) {
	if workspace == "" || len(workspace) > 128 || key == "" || len(key) > 256 || input.Prompt == "" || len(input.Prompt) > 1<<20 || input.AgentID == "" || input.AgentVersion == "" || input.ProfileID == "" || input.ProfileVersion == "" {
		return Task{}, false, fmt.Errorf("invalid task input: %w", ErrInvalidInput)
	}
	var err error
	input.Config, err = canonicalConfig(input.Config)
	if err != nil {
		// Only the input classification is part of the error contract.
		return Task{}, false, fmt.Errorf("%w: %s", ErrInvalidInput, err.Error())
	}
	if len(input.Config) > 64<<10 {
		return Task{}, false, fmt.Errorf("%w: config exceeds 64 KiB", ErrInvalidInput)
	}
	explicitDeadline := !input.Deadline.IsZero()
	input.Deadline = input.Deadline.UTC()
	if input.Deadline.IsZero() {
		if input.TimeoutSeconds == 0 {
			input.TimeoutSeconds = 3600
		}
		if input.TimeoutSeconds < 1 || input.TimeoutSeconds > 86400 {
			return Task{}, false, fmt.Errorf("%w: timeout_seconds must be between 1 and 86400", ErrInvalidInput)
		}
	} else if input.TimeoutSeconds != 0 {
		return Task{}, false, fmt.Errorf("%w: choose deadline or timeout_seconds", ErrInvalidInput)
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return Task{}, false, err
	}
	digest := sha256.Sum256(encoded)
	hash := hex.EncodeToString(digest[:])
	if input.Deadline.IsZero() {
		input.Deadline = time.Now().UTC().Add(time.Duration(input.TimeoutSeconds) * time.Second)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit closes the transaction; pgx discards the connection on rollback failure.
	q := dbgen.New(s.pool).WithTx(tx)
	principal := CurrentPrincipal(ctx)
	if err = authorizeTx(ctx, q, principal, workspace, "write", false); err != nil {
		return Task{}, false, err
	}
	previous, err := q.FindIdempotency(ctx, dbgen.FindIdempotencyParams{WorkspaceID: workspace, IdempotencyKey: key})
	previousID, previousHash := previous.ID, previous.RequestHash
	if err == nil {
		if previousHash != hash {
			return Task{}, false, ErrConflict
		}
		previous, e := taskInTx(ctx, tx, workspace, previousID)
		if e != nil {
			return Task{}, false, e
		}
		if e = tx.Commit(ctx); e != nil {
			return Task{}, false, e
		}
		return previous, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Task{}, false, err
	}
	if explicitDeadline && (input.Deadline.Before(time.Now().Add(5*time.Second)) || input.Deadline.After(time.Now().Add(90*time.Minute))) {
		return Task{}, false, fmt.Errorf("%w: deadline must be within 5 seconds to 90 minutes", ErrInvalidInput)
	}
	id := newID()
	tag, err := dbgen.New(tx).InsertTask(ctx, dbgen.InsertTaskParams{ID: id, WorkspaceID: workspace, IdempotencyKey: key, RequestHash: hash, Prompt: input.Prompt, Config: input.Config, AgentID: input.AgentID, AgentVersion: input.AgentVersion, ProfileID: input.ProfileID, ProfileVersion: input.ProfileVersion, Deadline: timestamp(input.Deadline)})
	if err != nil {
		return Task{}, false, err
	}
	created := tag == 1
	existing, err := q.FindIdempotency(ctx, dbgen.FindIdempotencyParams{WorkspaceID: workspace, IdempotencyKey: key})
	id, existingHash := existing.ID, existing.RequestHash
	if err != nil {
		return Task{}, false, err
	}
	if hash != existingHash {
		return Task{}, false, ErrConflict
	}
	t, err := taskInTx(ctx, tx, workspace, id)
	if err != nil {
		return Task{}, false, err
	}
	if created {
		actor := dbgen.SaveTaskActorParams{WorkspaceID: workspace, ID: id}
		if principal.Kind == "user" {
			actor.SubmittedByUserID = new(principal.ID)
		} else {
			actor.SubmittedByMachineID = new(principal.ID)
		}
		if err = q.SaveTaskActor(ctx, actor); err != nil {
			return Task{}, false, err
		}
		if err = enqueue(ctx, tx, t, "START_TASK"); err != nil {
			return Task{}, false, err
		}
		if err = audit(ctx, q, workspace, principal, "submit_task", "allowed", id); err != nil {
			return Task{}, false, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Task{}, false, err
	}
	return t, created, nil
}

// Get loads a task within its workspace.
func (s *Store) Get(ctx context.Context, workspace, id string) (Task, error) {
	q := dbgen.New(s.pool)
	row, err := q.GetTask(ctx, dbgen.GetTaskParams{WorkspaceID: workspace, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, err
	}
	t := taskFromRow(row)
	t.Run, err = loadRun(ctx, q, id)
	return t, err
}

// List returns recent tasks within a workspace.
func (s *Store) List(ctx context.Context, workspace string) ([]Task, error) {
	rows, err := dbgen.New(s.pool).ListTasks(ctx, workspace)
	if err != nil {
		return nil, err
	}
	tasks := make([]Task, 0, len(rows))
	for _, r := range rows {
		tasks = append(tasks, taskFromRow(dbgen.GetTaskRow(r)))
	}
	return s.attachRuns(ctx, tasks)
}

// RecoveryCursor uses an immutable order, so old active tasks cannot starve later ones.
type RecoveryCursor struct {
	CreatedAt time.Time
	ID        string
	// Through bounds one sweep so continuous arrivals cannot prevent a restart.
	Through time.Time
}

// RecoveryPageSize bounds each scan for tasks requiring workflow recovery.
const RecoveryPageSize = 100

// Recoverable lists tasks requiring execution or cleanup recovery after the cursor.
func (s *Store) Recoverable(ctx context.Context, after RecoveryCursor) ([]Task, error) {
	if after.Through.IsZero() {
		after.Through = time.Now()
	}
	rows, err := dbgen.New(s.pool).RecoveryTasks(ctx, dbgen.RecoveryTasksParams{Column1: timestamp(after.CreatedAt), Column2: after.ID, Column3: timestamp(after.Through)})
	if err != nil {
		return nil, err
	}
	tasks := make([]Task, 0, len(rows))
	for _, r := range rows {
		tasks = append(tasks, taskFromRow(dbgen.GetTaskRow(r)))
	}
	return s.attachRuns(ctx, tasks)
}
func (s *Store) attachRuns(ctx context.Context, tasks []Task) ([]Task, error) {
	q := dbgen.New(s.pool)
	for i := range tasks {
		run, err := loadRun(ctx, q, tasks[i].ID)
		if err != nil {
			return nil, err
		}
		tasks[i].Run = run
	}
	return tasks, nil
}

// Cancel records cancellation and queues the corresponding workflow command.
func (s *Store) Cancel(ctx context.Context, workspace, id string) (Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit closes the transaction; pgx discards the connection on rollback failure.
	q := dbgen.New(s.pool).WithTx(tx)
	if err = authorizeTx(ctx, q, CurrentPrincipal(ctx), workspace, "write", false); err != nil {
		return Task{}, err
	}
	t, err := taskInTx(ctx, tx, workspace, id)
	if err != nil {
		return Task{}, err
	}
	wasTerminal := IsTerminal(t.State)
	if !wasTerminal {
		t.CancelRequested = true
		if t.Run == nil {
			t.State = core.TaskCanceled
			t.Reason = "CANCEL_REQUESTED"
		} else {
			t.State = core.TaskCanceling
		}
		_, err = dbgen.New(tx).CancelTaskState(ctx, dbgen.CancelTaskStateParams{ID: id, State: string(t.State), Reason: t.Reason})
		if err != nil {
			return Task{}, err
		}
		if t.Run != nil {
			if err = enqueue(ctx, tx, t, "CANCEL_TASK"); err != nil {
				return Task{}, err
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Task{}, err
	}
	if !wasTerminal && IsTerminal(t.State) {
		s.Metrics.Result(ctx, string(t.State), time.Time{})
	}
	return t, nil
}

// InitializeRun creates the execution run once and updates the task state.
func (s *Store) InitializeRun(ctx context.Context, workspace, id, backend string) (Task, error) {
	if backend == "" {
		return Task{}, fmt.Errorf("backend is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit closes the transaction; pgx discards the connection on rollback failure.
	t, err := taskInTx(ctx, tx, workspace, id)
	if err != nil {
		return Task{}, err
	}
	initialized, expired := false, false
	if !IsTerminal(t.State) && t.Run == nil {
		if !t.Deadline.After(time.Now()) || time.Since(t.CreatedAt) > 10*time.Minute {
			expired = true
			t.State = core.TaskFailed
			t.Reason = "DEADLINE_EXCEEDED"
			_, err = dbgen.New(tx).ExpireTaskState(ctx, dbgen.ExpireTaskStateParams{ID: id, State: string(t.State), Reason: t.Reason})
		} else {
			initialized = true
			t.Run = &Run{StartedAt: new(time.Now()), ID: newID(), TaskID: id, State: core.RunPreparing, BackendID: backend, Cleanup: core.CleanupNotRequired}
			_, err = dbgen.New(tx).InitializeBusinessRun(ctx, dbgen.InitializeBusinessRunParams{ID: t.Run.ID, TaskID: id, State: string(t.Run.State), BackendID: backend, StartedAt: timestamp(*t.Run.StartedAt)})
			if err == nil {
				_, err = dbgen.New(tx).MarkTaskRunning(ctx, id)
				t.State = core.TaskRunning
			}
		}
	}
	if err != nil {
		return Task{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Task{}, err
	}
	if initialized && s.Metrics != nil {
		s.Metrics.Queue.Record(ctx, max(0, time.Since(t.CreatedAt).Seconds()))
	}
	if expired {
		s.Metrics.Result(ctx, string(t.State), time.Time{})
	}
	return t, nil
}
