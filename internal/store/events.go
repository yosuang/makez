package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/makez-dev/makez/internal/core"
)

func (s *Store) ClaimObserver(ctx context.Context, runID string) (int64, error) {
	var epoch int64
	err := s.pool.QueryRow(ctx, `UPDATE makez_runs SET observer_epoch=observer_epoch+1 WHERE id=$1 AND state NOT IN ('SUCCEEDED','FAILED','CANCELED') RETURNING observer_epoch`, runID).Scan(&epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrConflict
	}
	return epoch, err
}
func (s *Store) AppendOutput(ctx context.Context, runID string, epoch int64, cursor string, parser []byte, events []core.AgentEvent) error {
	if len(parser) > 64<<10 || len(cursor) > 4096 || len(events) > 1024 {
		return fmt.Errorf("output batch exceeds limit")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var current int64
	var state core.RunState
	if err = tx.QueryRow(ctx, `SELECT observer_epoch,state FROM makez_runs WHERE id=$1 FOR UPDATE`, runID).Scan(&current, &state); err != nil {
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
		_, err = tx.Exec(ctx, `INSERT INTO makez_events(run_id,source_offset,item_index,type,payload) VALUES($1,$2,$3,$4,$5) ON CONFLICT(run_id,source_offset,item_index) DO NOTHING`, runID, event.SourceOffset, int64(event.Index), event.Kind, event.Payload)
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE makez_runs SET cursor=$2,parser_state=$3,version=version+1 WHERE id=$1`, runID, cursor, nonNil(parser))
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) Events(ctx context.Context, workspace, taskID string, after int64) ([]Event, error) {
	if _, err := s.Get(ctx, workspace, taskID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT e.seq,e.run_id,e.type,e.payload,e.created_at FROM makez_events e JOIN makez_runs r ON r.id=e.run_id JOIN makez_tasks t ON t.id=r.task_id WHERE t.workspace_id=$1 AND t.id=$2 AND e.seq>$3 ORDER BY e.seq LIMIT 500`, workspace, taskID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var e Event
		if err = rows.Scan(&e.Seq, &e.RunID, &e.Type, &e.Payload, &e.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
func (s *Store) Artifacts(ctx context.Context, workspace, taskID string) ([]Artifact, error) {
	if _, err := s.Get(ctx, workspace, taskID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT a.id,a.run_id,a.path,a.storage_key,a.sha256,a.media_type,a.size FROM makez_artifacts a JOIN makez_runs r ON r.id=a.run_id JOIN makez_tasks t ON t.id=r.task_id WHERE t.workspace_id=$1 AND t.id=$2 ORDER BY a.path`, workspace, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	artifacts := []Artifact{}
	for rows.Next() {
		a, e := scanArtifact(rows)
		if e != nil {
			return nil, e
		}
		artifacts = append(artifacts, a)
	}
	return artifacts, rows.Err()
}
func scanArtifact(row rowScanner) (Artifact, error) {
	var a Artifact
	err := row.Scan(&a.ID, &a.RunID, &a.Path, &a.StorageKey, &a.SHA256, &a.MediaType, &a.Size)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return a, err
}
func (s *Store) Artifact(ctx context.Context, workspace, id string) (Artifact, error) {
	return scanArtifact(s.pool.QueryRow(ctx, `SELECT a.id,a.run_id,a.path,a.storage_key,a.sha256,a.media_type,a.size FROM makez_artifacts a JOIN makez_runs r ON r.id=a.run_id JOIN makez_tasks t ON t.id=r.task_id WHERE t.workspace_id=$1 AND a.id=$2`, workspace, id))
}
func (s *Store) Pending(ctx context.Context, limit int) ([]Outbox, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT id,workspace_id,task_id,run_id,command,attempts,last_error FROM makez_outbox WHERE delivered_at IS NULL AND next_attempt_at<=now() ORDER BY next_attempt_at,id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pending := []Outbox{}
	for rows.Next() {
		var o Outbox
		if err = rows.Scan(&o.ID, &o.WorkspaceID, &o.TaskID, &o.RunID, &o.Command, &o.Attempts, &o.LastError); err != nil {
			return nil, err
		}
		pending = append(pending, o)
	}
	return pending, rows.Err()
}
func (s *Store) Delivered(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE makez_outbox SET delivered_at=COALESCE(delivered_at,now()),last_error='' WHERE id=$1`, id)
	return err
}
func (s *Store) RetryOutbox(ctx context.Context, id, reason string) error {
	if len(reason) > 2048 {
		reason = reason[:2048]
	}
	_, err := s.pool.Exec(ctx, `UPDATE makez_outbox SET attempts=attempts+1,last_error=$2,next_attempt_at=now()+make_interval(secs=>LEAST(60,power(2,LEAST(attempts,6)))::integer) WHERE id=$1 AND delivered_at IS NULL`, id, reason)
	return err
}
