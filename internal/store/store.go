package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/telemetry"
)

//go:embed schema.sql
var schema string

type Store struct {
	pool    *pgxpool.Pool
	Metrics *telemetry.Metrics
}

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
func (s *Store) Close() { s.pool.Close() }
func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(746302945)"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS makez_schema_migrations (version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	for index, migration := range []string{schema, `ALTER TABLE makez_runs ADD COLUMN IF NOT EXISTS agent_plan jsonb`, `
ALTER TABLE makez_runs ADD COLUMN started_at timestamptz;
-- Historical outbox creation times are unknown; age starts at this migration.
ALTER TABLE makez_outbox ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX makez_tasks_recovery_cursor ON makez_tasks(created_at,id);
CREATE INDEX makez_outbox_pending_created ON makez_outbox(created_at) WHERE delivered_at IS NULL;
CREATE INDEX makez_runs_cleanup_needed ON makez_runs(cleanup) WHERE cleanup IN ('PENDING','BLOCKED');
`} {
		version := index + 1
		var applied bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM makez_schema_migrations WHERE version=$1)`, version).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		if _, err = tx.Exec(ctx, migration); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO makez_schema_migrations(version) VALUES($1)`, version); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

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

const taskColumns = `t.id,t.workspace_id,t.prompt,t.config,t.agent_id,t.agent_version,t.profile_id,t.profile_version,t.state,t.reason,t.deadline,t.cancel_requested,t.created_at,COALESCE((SELECT last_error FROM makez_outbox WHERE task_id=t.id AND command='START_TASK' AND delivered_at IS NULL),'')`

type rowScanner interface{ Scan(...any) error }

func scanTask(row rowScanner) (Task, error) {
	var t Task
	err := row.Scan(&t.ID, &t.WorkspaceID, &t.Prompt, &t.Config, &t.AgentID, &t.AgentVersion, &t.ProfileID, &t.ProfileVersion, &t.State, &t.Reason, &t.Deadline, &t.CancelRequested, &t.CreatedAt, &t.DispatchError)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return t, err
}
func scanRun(row rowScanner) (*Run, error) {
	var r Run
	var sandbox, process, exit, result, plan []byte
	err := row.Scan(&r.ID, &r.TaskID, &r.State, &r.BackendID, &sandbox, &process, &r.Cursor, &r.ParserState, &r.ObserverEpoch, &exit, &r.Cleanup, &r.VerifiedStopped, &result, &plan, &r.StartedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, field := range []struct {
		raw    []byte
		target any
	}{{sandbox, &r.Sandbox}, {process, &r.Process}, {exit, &r.Exit}, {result, &r.Result}, {plan, &r.Plan}} {
		if len(field.raw) > 0 {
			if err = json.Unmarshal(field.raw, field.target); err != nil {
				return nil, err
			}
		}
	}
	return &r, nil
}

const runColumns = `id,task_id,state,backend_id,sandbox,process,cursor,parser_state,observer_epoch,process_exit,cleanup,verified_stopped,prepared_result,agent_plan,started_at`

func loadRun(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, taskID string) (*Run, error) {
	return scanRun(q.QueryRow(ctx, "SELECT "+runColumns+" FROM makez_runs WHERE task_id=$1", taskID))
}
func taskInTx(ctx context.Context, tx pgx.Tx, workspace, id string) (Task, error) {
	t, err := scanTask(tx.QueryRow(ctx, "SELECT "+taskColumns+" FROM makez_tasks t WHERE workspace_id=$1 AND id=$2 FOR UPDATE", workspace, id))
	if err == nil {
		t.Run, err = loadRun(ctx, tx, id)
	}
	return t, err
}
func enqueue(ctx context.Context, tx pgx.Tx, t Task, command string) error {
	runID := ""
	if t.Run != nil {
		runID = t.Run.ID
	}
	_, err := tx.Exec(ctx, `INSERT INTO makez_outbox(id,workspace_id,task_id,run_id,command) VALUES($1,$2,$3,$4,$5) ON CONFLICT(task_id,command) DO NOTHING`, newID(), t.WorkspaceID, t.ID, runID, command)
	return err
}

func (s *Store) Submit(ctx context.Context, workspace, key string, input SubmitInput) (Task, bool, error) {
	if workspace == "" || len(workspace) > 128 || key == "" || len(key) > 256 || input.Prompt == "" || len(input.Prompt) > 1<<20 || input.AgentID == "" || input.AgentVersion == "" || input.ProfileID == "" || input.ProfileVersion == "" {
		return Task{}, false, fmt.Errorf("invalid task input: %w", ErrInvalidInput)
	}
	var err error
	input.Config, err = canonicalConfig(input.Config)
	if err != nil {
		return Task{}, false, fmt.Errorf("%w: %v", ErrInvalidInput, err)
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
	defer tx.Rollback(ctx)
	var previousID, previousHash string
	err = tx.QueryRow(ctx, `SELECT id,request_hash FROM makez_tasks WHERE workspace_id=$1 AND idempotency_key=$2 FOR UPDATE`, workspace, key).Scan(&previousID, &previousHash)
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
	tag, err := tx.Exec(ctx, `INSERT INTO makez_tasks(id,workspace_id,idempotency_key,request_hash,prompt,config,agent_id,agent_version,profile_id,profile_version,state,deadline) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'QUEUED',$11) ON CONFLICT(workspace_id,idempotency_key) DO NOTHING`, id, workspace, key, hash, input.Prompt, input.Config, input.AgentID, input.AgentVersion, input.ProfileID, input.ProfileVersion, input.Deadline)
	if err != nil {
		return Task{}, false, err
	}
	created := tag.RowsAffected() == 1
	var existingHash string
	if err = tx.QueryRow(ctx, `SELECT id,request_hash FROM makez_tasks WHERE workspace_id=$1 AND idempotency_key=$2 FOR UPDATE`, workspace, key).Scan(&id, &existingHash); err != nil {
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
		if err = enqueue(ctx, tx, t, "START_TASK"); err != nil {
			return Task{}, false, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Task{}, false, err
	}
	return t, created, nil
}
func (s *Store) Get(ctx context.Context, workspace, id string) (Task, error) {
	t, err := scanTask(s.pool.QueryRow(ctx, "SELECT "+taskColumns+" FROM makez_tasks t WHERE workspace_id=$1 AND id=$2", workspace, id))
	if err == nil {
		t.Run, err = loadRun(ctx, s.pool, id)
	}
	return t, err
}
func (s *Store) List(ctx context.Context, workspace string) ([]Task, error) {
	return s.list(ctx, "WHERE t.workspace_id=$1 ORDER BY t.created_at DESC LIMIT 100", workspace)
}

// RecoveryCursor uses an immutable order, so old active tasks cannot starve later ones.
type RecoveryCursor struct {
	CreatedAt time.Time
	ID        string
	// Through bounds one sweep so continuous arrivals cannot prevent a restart.
	Through time.Time
}

const RecoveryPageSize = 100

func (s *Store) Recoverable(ctx context.Context, after RecoveryCursor) ([]Task, error) {
	through := after.Through
	if through.IsZero() {
		through = time.Now()
	}
	return s.list(ctx, `WHERE (t.state NOT IN ('SUCCEEDED','FAILED','CANCELED') OR EXISTS(SELECT 1 FROM makez_runs r WHERE r.task_id=t.id AND r.cleanup='PENDING')) AND ($2='' OR (t.created_at,t.id)>($1,$2)) AND t.created_at <= $3 ORDER BY t.created_at,t.id LIMIT 100`, after.CreatedAt, after.ID, through)
}
func (s *Store) list(ctx context.Context, where string, args ...any) ([]Task, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+taskColumns+" FROM makez_tasks t "+where, args...)
	if err != nil {
		return nil, err
	}
	tasks := []Task{}
	for rows.Next() {
		t, e := scanTask(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		tasks = append(tasks, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		tasks[i].Run, err = loadRun(ctx, s.pool, tasks[i].ID)
		if err != nil {
			return nil, err
		}
	}
	return tasks, nil
}
func (s *Store) Cancel(ctx context.Context, workspace, id string) (Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback(ctx)
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
		_, err = tx.Exec(ctx, `UPDATE makez_tasks SET state=$2,reason=$3,cancel_requested=true,version=version+1 WHERE id=$1`, id, t.State, t.Reason)
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
func (s *Store) InitializeRun(ctx context.Context, workspace, id, backend string) (Task, error) {
	if backend == "" {
		return Task{}, fmt.Errorf("backend is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback(ctx)
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
			_, err = tx.Exec(ctx, `UPDATE makez_tasks SET state=$2,reason=$3,version=version+1 WHERE id=$1`, id, t.State, t.Reason)
		} else {
			initialized = true
			t.Run = &Run{StartedAt: new(time.Now()), ID: newID(), TaskID: id, State: core.RunPreparing, BackendID: backend, Cleanup: core.CleanupNotRequired}
			_, err = tx.Exec(ctx, `INSERT INTO makez_runs(id,task_id,state,backend_id,started_at) VALUES($1,$2,$3,$4,$5)`, t.Run.ID, id, t.Run.State, backend, t.Run.StartedAt)
			if err == nil {
				_, err = tx.Exec(ctx, `UPDATE makez_tasks SET state='RUNNING',version=version+1 WHERE id=$1`, id)
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
