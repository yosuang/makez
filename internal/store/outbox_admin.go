package store

import (
	"context"
	"errors"
	"strings"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/makez-dev/makez/internal/store/dbgen"
)

// ReplayDeadLetter requeues one dead event and records the operator command in
// the same transaction. Database credentials authorize this maintenance action;
// the supplied reason is explanatory text, not an authenticated operator identity.
// Replay preserves event identity and does not bypass downstream reconciliation.
func (s *Store) ReplayDeadLetter(ctx context.Context, workspace, id, queue, reason string) error {
	reason = strings.TrimSpace(reason)
	if (queue != "task" && queue != "authorization") || reason == "" || len(reason) > 512 {
		return ErrInvalidInput
	}
	if queue == "authorization" {
		if _, err := uuid.Parse(id); err != nil {
			return ErrInvalidInput
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit closes the transaction.
	q := dbgen.New(s.pool).WithTx(tx)
	if _, err = q.WorkspaceShareLock(ctx, workspace); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	var count int64
	if queue == "task" {
		count, err = q.ReplayDeadOutbox(ctx, dbgen.ReplayDeadOutboxParams{ID: id, WorkspaceID: workspace})
	} else {
		count, err = q.ReplayDeadAuthorization(ctx, dbgen.ReplayDeadAuthorizationParams{ID: id, WorkspaceID: workspace})
	}
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrNotFound
	}
	if err = q.AuditOutboxReplay(ctx, dbgen.AuditOutboxReplayParams{
		ID: uuid.New().String(), WorkspaceID: new(workspace), ResourceID: id, Queue: queue, Reason: reason,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// OutboxQueueStatus counts pending and dead events separately in one workspace.
type OutboxQueueStatus struct {
	Queue   string `json:"queue"`
	Pending int64  `json:"pending"`
	Dead    int64  `json:"dead"`
}

// DeadLetter identifies a stopped delivery, not a proven absence of side effects.
type DeadLetter struct {
	Queue    string    `json:"queue"`
	ID       string    `json:"id"`
	Attempts int64     `json:"attempts"`
	DeadAt   time.Time `json:"dead_at"`
	Reason   string    `json:"reason"`
}

// OutboxReport is a bounded, consistent operator view of both delivery queues.
// Only the oldest 100 dead letters are returned; counts always cover all rows.
type OutboxReport struct {
	Queues      []OutboxQueueStatus `json:"queues"`
	DeadLetters []DeadLetter        `json:"dead_letters"`
	Truncated   bool                `json:"truncated"`
}

// OutboxStatus reads delivery status without claiming, recovering, or replaying events.
func (s *Store) OutboxStatus(ctx context.Context, workspace string) (OutboxReport, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return OutboxReport{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit closes the read-only transaction.
	q := dbgen.New(s.pool).WithTx(tx)
	counts, err := q.OutboxQueueCounts(ctx, workspace)
	if err != nil {
		return OutboxReport{}, err
	}
	rows, err := q.ListDeadLetters(ctx, workspace)
	if err != nil {
		return OutboxReport{}, err
	}
	report := OutboxReport{Queues: make([]OutboxQueueStatus, 0, len(counts)), DeadLetters: make([]DeadLetter, 0, min(len(rows), 100)), Truncated: len(rows) > 100}
	for _, row := range counts {
		report.Queues = append(report.Queues, OutboxQueueStatus{Queue: row.Queue, Pending: row.Pending, Dead: row.Dead})
	}
	for _, row := range rows[:min(len(rows), 100)] {
		report.DeadLetters = append(report.DeadLetters, DeadLetter{Queue: row.Queue, ID: row.ID, Attempts: row.Attempts, DeadAt: row.DeadAt.Time, Reason: row.Reason})
	}
	return report, tx.Commit(ctx)
}
