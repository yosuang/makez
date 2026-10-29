package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/store/dbgen"
)

// ClaimObserver advances the observer epoch to fence out stale output writers.
func (s *Store) ClaimObserver(ctx context.Context, runID string) (int64, error) {
	epoch, err := dbgen.New(s.pool).ClaimOutputObserver(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrConflict
	}
	return epoch, err
}

// AppendOutput commits decoded events and the parser cursor for the current observer.
func (s *Store) AppendOutput(ctx context.Context, runID string, epoch int64, cursor string, parser []byte, events []core.AgentEvent) error {
	if len(parser) > 64<<10 || len(cursor) > 4096 || len(events) > 1024 {
		return fmt.Errorf("output batch exceeds limit")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit closes the transaction; pgx discards the connection on rollback failure.
	locked, err := dbgen.New(tx).LockRun(ctx, runID)
	current, state := locked.ObserverEpoch, core.RunState(locked.State)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return err
	}
	if current != epoch {
		return ErrStale
	}
	if IsTerminal(core.TaskState(state)) {
		return ErrConflict
	}
	for _, event := range events {
		if event.SourceOffset == "" || len(event.SourceOffset) > 4096 || len(event.Kind) > 128 || event.Kind == "" || len(event.Payload) > 64<<10 || !json.Valid(event.Payload) {
			return fmt.Errorf("invalid event")
		}
		_, err = dbgen.New(tx).InsertOutput(ctx, dbgen.InsertOutputParams{RunID: runID, SourceOffset: event.SourceOffset, ItemIndex: int64(event.Index), Type: event.Kind, Payload: event.Payload})
		if err != nil {
			return err
		}
	}
	_, err = dbgen.New(tx).SaveOutputCursor(ctx, dbgen.SaveOutputCursorParams{ID: runID, Cursor: cursor, ParserState: nonNil(parser)})
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// EventPageSize bounds the number of events returned in one replay page.
const EventPageSize = 500

// Events returns task events after a replay sequence number.
func (s *Store) Events(ctx context.Context, workspace, taskID string, after int64) ([]Event, error) {
	raw, err := dbgen.New(s.pool).GetEvents(ctx, dbgen.GetEventsParams{WorkspaceID: workspace, ID: taskID, Seq: after})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var events []Event
	err = json.Unmarshal(raw, &events)
	return events, err
}

// Artifacts lists a task’s published artifacts within its workspace.
func (s *Store) Artifacts(ctx context.Context, workspace, taskID string) ([]Artifact, error) {
	if _, err := s.Get(ctx, workspace, taskID); err != nil {
		return nil, err
	}
	rows, err := dbgen.New(s.pool).ListArtifacts(ctx, dbgen.ListArtifactsParams{WorkspaceID: workspace, TaskID: taskID})
	if err != nil {
		return nil, err
	}
	result := make([]Artifact, 0, len(rows))
	for _, r := range rows {
		result = append(result, artifactFromRow(r))
	}
	return result, nil
}
func artifactFromRow(r dbgen.MakezArtifact) Artifact {
	return Artifact{ID: r.ID, RunID: r.RunID, Path: r.Path, StorageKey: r.StorageKey, SHA256: r.Sha256, MediaType: r.MediaType, Size: r.Size}
}

// Artifact loads one published artifact within its workspace.
func (s *Store) Artifact(ctx context.Context, workspace, id string) (Artifact, error) {
	r, err := dbgen.New(s.pool).GetArtifact(ctx, dbgen.GetArtifactParams{WorkspaceID: workspace, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return Artifact{}, ErrNotFound
	}
	return artifactFromRow(r), err
}

// Pending claims due commands; callers must retain the returned fencing tokens.
func (s *Store) Pending(ctx context.Context, limit int) ([]Outbox, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := dbgen.New(s.pool).ClaimOutbox(ctx, dbgen.ClaimOutboxParams{Limit: int32(limit), LeaseOwner: new(newID())})
	if err != nil {
		return nil, err
	}
	pending := make([]Outbox, 0, len(rows))
	for _, row := range rows {
		pending = append(pending, Outbox{ID: row.ID, WorkspaceID: row.WorkspaceID, TaskID: row.TaskID, RunID: row.RunID, Command: row.Command, Attempts: int(row.Attempts), LastError: row.LastError, LeaseOwner: *row.LeaseOwner, LeaseToken: row.LeaseToken})
	}
	return pending, nil
}

// Delivered acknowledges only the current unexpired lease.
func (s *Store) Delivered(ctx context.Context, row Outbox) error {
	count, err := dbgen.New(s.pool).AckOutbox(ctx, dbgen.AckOutboxParams{ID: row.ID, LeaseOwner: new(row.LeaseOwner), LeaseToken: row.LeaseToken})
	if err == nil && count != 1 {
		return ErrStale
	}
	return err
}

// RetryOutbox releases a current lease with bounded jitter and a dead-letter limit.
func (s *Store) RetryOutbox(ctx context.Context, row Outbox, reason string) error {
	if len(reason) > 2048 {
		reason = reason[:2048]
	}
	count, err := dbgen.New(s.pool).RetryClaimedOutbox(ctx, dbgen.RetryClaimedOutboxParams{ID: row.ID, LeaseOwner: new(row.LeaseOwner), LeaseToken: row.LeaseToken, LastError: reason})
	if err == nil && count != 1 {
		return ErrStale
	}
	return err
}

// ReplayOutbox explicitly requeues a dead command without changing its identity.
func (s *Store) ReplayOutbox(ctx context.Context, workspace, id string) error {
	count, err := dbgen.New(s.pool).ReplayDeadOutbox(ctx, dbgen.ReplayDeadOutboxParams{ID: id, WorkspaceID: workspace})
	if err == nil && count != 1 {
		return ErrNotFound
	}
	return err
}
