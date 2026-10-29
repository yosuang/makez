package store

import (
	"context"

	"github.com/makez-dev/makez/internal/store/dbgen"
	"github.com/makez-dev/makez/internal/telemetry"
)

// Ping checks connectivity to the database.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// ReadBacklog reads only indexed operational queues, not task/event history.
// The collector supplies a bounded context and publishes only complete samples.
func (s *Store) ReadBacklog(ctx context.Context) (telemetry.Backlog, error) {
	q := dbgen.New(s.pool)
	outbox, err := q.OutboxBacklog(ctx)
	if err != nil {
		return telemetry.Backlog{}, err
	}
	cleanup, err := q.CleanupBacklog(ctx)
	if err != nil {
		return telemetry.Backlog{}, err
	}
	return telemetry.Backlog{Pending: outbox.Pending, OldestSeconds: outbox.OldestSeconds, CleanupPending: cleanup.Pending, CleanupBlocked: cleanup.Blocked}, nil
}
