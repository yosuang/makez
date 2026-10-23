package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/makez-dev/makez/internal/core"
)

func allowedRunTransition(from, to core.RunState) bool {
	if from == to {
		return true
	}
	switch from {
	case core.RunPreparing:
		return to == core.RunStarting || to == core.RunStopping
	case core.RunStarting:
		return to == core.RunRunning || to == core.RunStopping
	case core.RunRunning:
		return to == core.RunStopping || to == core.RunFinalizing
	case core.RunStopping:
		return to == core.RunFinalizing
	}
	return false
}

// UpdateRun validates and commits a run transition under a row lock.
func (s *Store) UpdateRun(ctx context.Context, id string, u RunUpdate) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit closes the transaction; pgx discards the connection on rollback failure.
	r, err := scanRun(tx.QueryRow(ctx, "SELECT "+runColumns+" FROM makez_runs WHERE id=$1 FOR UPDATE", id))
	if err != nil {
		return err
	}
	if r == nil {
		return ErrNotFound
	}
	if IsTerminal(core.TaskState(r.State)) {
		return ErrConflict
	}
	if u.State != nil {
		if !allowedRunTransition(r.State, *u.State) {
			return fmt.Errorf("run %s to %s: %w", r.State, *u.State, ErrConflict)
		}
		r.State = *u.State
	}
	if u.Sandbox != nil {
		if u.Sandbox.Key.RunID != r.ID || u.Sandbox.Key.TaskID != r.TaskID || u.Sandbox.BackendID != r.BackendID {
			return ErrConflict
		}
		if r.Sandbox != nil && r.Sandbox.ID != u.Sandbox.ID {
			return ErrConflict
		}
		r.Sandbox = u.Sandbox
	}
	if u.Process != nil {
		if u.Process.Key.RunID != r.ID || u.Process.Key.TaskID != r.TaskID || r.Sandbox == nil || u.Process.Sandbox.ID != r.Sandbox.ID {
			return ErrConflict
		}
		if r.Process != nil && r.Process.ID != u.Process.ID {
			return ErrConflict
		}
		r.Process = u.Process
	}
	if u.Cursor != nil {
		r.Cursor = *u.Cursor
	}
	if u.ParserState != nil {
		r.ParserState = *u.ParserState
	}
	if len(r.ParserState) > 64<<10 {
		return fmt.Errorf("parser checkpoint exceeds 64 KiB")
	}
	if u.Exit != nil {
		r.Exit = u.Exit
	}
	if u.Cleanup != nil {
		r.Cleanup = *u.Cleanup
	}
	if u.VerifiedStopped != nil {
		r.VerifiedStopped = *u.VerifiedStopped
	}
	if u.Result != nil {
		if r.State != core.RunFinalizing || !IsTerminal(u.Result.State) {
			return ErrConflict
		}
		if r.Result != nil {
			old, _ := json.Marshal(r.Result)
			next, _ := json.Marshal(u.Result)
			if !bytes.Equal(old, next) {
				return ErrConflict
			}
		}
		r.Result = u.Result
	}
	// Cursor commits during observation use AppendOutput, which also fences stale observers.
	_, err = tx.Exec(ctx, `UPDATE makez_runs SET state=$2,sandbox=$3,process=$4,cursor=$5,parser_state=$6,process_exit=$7,cleanup=$8,verified_stopped=$9,prepared_result=$10,version=version+1 WHERE id=$1`, id, r.State, jsonValue(r.Sandbox), jsonValue(r.Process), r.Cursor, nonNil(r.ParserState), jsonValue(r.Exit), r.Cleanup, r.VerifiedStopped, jsonValue(r.Result))
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func jsonValue(value any) any {
	encoded, _ := json.Marshal(value)
	if string(encoded) == "null" {
		return nil
	}
	return encoded
}
func nonNil(value []byte) []byte {
	if value == nil {
		return []byte{}
	}
	return value
}

// Publish atomically publishes the final task state and archived artifacts.
func (s *Store) Publish(ctx context.Context, workspace, id string, state core.TaskState, reason string, verified bool, cleanup core.CleanupState, artifacts []Artifact) error {
	if !IsTerminal(state) {
		return ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit closes the transaction; pgx discards the connection on rollback failure.
	t, err := taskInTx(ctx, tx, workspace, id)
	if err != nil {
		return err
	}
	if IsTerminal(t.State) {
		return nil
	}
	if t.Run == nil {
		if state != core.TaskFailed {
			return ErrConflict
		}
	} else {
		r, err := scanRun(tx.QueryRow(ctx, "SELECT "+runColumns+" FROM makez_runs WHERE id=$1 FOR UPDATE", t.Run.ID))
		if err != nil {
			return err
		}
		if r.State != core.RunFinalizing {
			return fmt.Errorf("publish requires FINALIZING: %w", ErrConflict)
		}
		if !verified {
			state = core.TaskFailed
			reason = "STOP_UNCONFIRMED"
		} else if t.CancelRequested {
			state = core.TaskCanceled
			reason = "CANCEL_REQUESTED"
		}
		finishedAt := time.Now()
		if r.Exit != nil && !r.Exit.FinishedAt.IsZero() {
			finishedAt = r.Exit.FinishedAt
		}
		if state == core.TaskSucceeded && !t.Deadline.After(finishedAt) {
			state = core.TaskFailed
			reason = "DEADLINE_EXCEEDED"
		}
		for _, artifact := range artifacts {
			if artifact.RunID != "" && artifact.RunID != r.ID {
				return ErrConflict
			}
			if artifact.ID == "" {
				digest := sha256.Sum256([]byte(r.ID + "\x00" + artifact.Path))
				artifact.ID = hex.EncodeToString(digest[:16])
			}
			if artifact.Path == "" || path.IsAbs(artifact.Path) || path.Clean(artifact.Path) != artifact.Path || strings.HasPrefix(artifact.Path, "../") || artifact.Path == ".." || artifact.Path == "." || strings.ContainsAny(artifact.Path, "\\\x00") || artifact.StorageKey == "" || artifact.Size < 0 || len(artifact.SHA256) != 64 {
				return fmt.Errorf("invalid artifact metadata")
			}
			_, err = tx.Exec(ctx, `INSERT INTO makez_artifacts(id,run_id,path,storage_key,sha256,media_type,size) VALUES($1,$2,$3,$4,$5,$6,$7)`, artifact.ID, r.ID, artifact.Path, artifact.StorageKey, artifact.SHA256, artifact.MediaType, artifact.Size)
			if err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE makez_runs SET state=$2,verified_stopped=$3,cleanup=$4,version=version+1 WHERE id=$1`, r.ID, core.RunState(state), verified, cleanup)
		if err != nil {
			return err
		}
		if cleanup == core.CleanupPending || cleanup == core.CleanupBlocked {
			if err = enqueue(ctx, tx, t, "START_CLEANUP"); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(ctx, `UPDATE makez_tasks SET state=$2,reason=$3,version=version+1 WHERE id=$1`, id, state, reason)
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	started := time.Time{}
	if t.Run != nil && t.Run.StartedAt != nil {
		started = *t.Run.StartedAt
	}
	s.Metrics.Result(ctx, string(state), started)
	return nil
}

// SetCleanup updates the cleanup state of a run.
func (s *Store) SetCleanup(ctx context.Context, id string, state core.CleanupState) error {
	if state != core.CleanupDone && state != core.CleanupPending && state != core.CleanupBlocked && state != core.CleanupNotRequired {
		return ErrConflict
	}
	tag, err := s.pool.Exec(ctx, `UPDATE makez_runs SET cleanup=$2,version=version+1 WHERE id=$1 AND (cleanup!='DONE' OR $2='DONE')`, id, state)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}

// BeginOperation records the intent and request hash of a keyed external operation.
func (s *Store) BeginOperation(ctx context.Context, runID, step, hash string) (string, bool, error) {
	if hash == "" || (step != "sandbox" && step != "agent") {
		return "", false, ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit closes the transaction; pgx discards the connection on rollback failure.
	var taskState core.TaskState
	var deadline time.Time
	if err = tx.QueryRow(ctx, `SELECT t.state,t.deadline FROM makez_tasks t JOIN makez_runs r ON r.task_id=t.id WHERE r.id=$1 FOR UPDATE OF t`, runID).Scan(&taskState, &deadline); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return "", false, err
	}
	var oldHash, phase string
	err = tx.QueryRow(ctx, `SELECT request_hash,phase FROM makez_operations WHERE run_id=$1 AND step=$2`, runID, step).Scan(&oldHash, &phase)
	if err == nil {
		if oldHash != hash {
			return phase, false, ErrConflict
		}
		return phase, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	if taskState != core.TaskRunning || !deadline.After(time.Now()) {
		return "", false, ErrConflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO makez_operations(run_id,step,request_hash,phase) VALUES($1,$2,$3,'DISPATCHING')`, runID, step, hash)
	if err != nil {
		return "", false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return "DISPATCHING", true, nil
}

// ConfirmOperation records the reconciled outcome of a keyed external operation.
func (s *Store) ConfirmOperation(ctx context.Context, runID, step string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE makez_operations SET phase='CONFIRMED',updated_at=now() WHERE run_id=$1 AND step=$2`, runID, step)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// OperationPhase is read-only: it must never claim a new external mutation.
func (s *Store) OperationPhase(ctx context.Context, runID, step string) (string, error) {
	var phase string
	err := s.pool.QueryRow(ctx, `SELECT phase FROM makez_operations WHERE run_id=$1 AND step=$2`, runID, step).Scan(&phase)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return phase, err
}

// FreezePlan atomically keeps the first non-secret plan across retries and
// configuration changes. It returns the winning plan, never a caller's loser.
func (s *Store) FreezePlan(ctx context.Context, runID string, plan core.AgentPlan) (core.AgentPlan, error) {
	if len(plan.Command.Argv) == 0 {
		return core.AgentPlan{}, ErrInvalidInput
	}
	for name := range plan.SecretRefs {
		if _, exists := plan.Command.Env[name]; exists {
			return core.AgentPlan{}, ErrInvalidInput
		}
	}
	encoded, err := json.Marshal(plan)
	if err != nil || len(encoded) > 1<<20 {
		return core.AgentPlan{}, ErrInvalidInput
	}
	var stored []byte
	err = s.pool.QueryRow(ctx, `UPDATE makez_runs SET agent_plan=COALESCE(agent_plan,$2::jsonb),version=version+CASE WHEN agent_plan IS NULL THEN 1 ELSE 0 END WHERE id=$1 AND (agent_plan IS NOT NULL OR state IN ('PREPARING','STARTING')) RETURNING agent_plan`, runID, encoded).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.AgentPlan{}, ErrConflict
	}
	if err != nil {
		return core.AgentPlan{}, err
	}
	var frozen core.AgentPlan
	if err = json.Unmarshal(stored, &frozen); err != nil {
		return core.AgentPlan{}, err
	}
	return frozen, nil
}
