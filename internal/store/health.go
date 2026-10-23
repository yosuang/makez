package store

import (
	"context"

	"github.com/makez-dev/makez/internal/telemetry"
)

// Ping checks connectivity to the database.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// ReadBacklog reads only indexed operational queues, not task/event history.
// The collector supplies a bounded context and publishes only complete samples.
func (s *Store) ReadBacklog(ctx context.Context) (telemetry.Backlog, error) {
	var snapshot telemetry.Backlog
	err := s.pool.QueryRow(ctx, `SELECT count(*),COALESCE(EXTRACT(EPOCH FROM now()-min(created_at)),0) FROM makez_outbox WHERE delivered_at IS NULL`).Scan(&snapshot.Pending, &snapshot.OldestSeconds)
	if err != nil {
		return telemetry.Backlog{}, err
	}
	err = s.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE cleanup='PENDING'),count(*) FILTER(WHERE cleanup='BLOCKED') FROM makez_runs WHERE cleanup IN ('PENDING','BLOCKED')`).Scan(&snapshot.CleanupPending, &snapshot.CleanupBlocked)
	if err != nil {
		return telemetry.Backlog{}, err
	}
	return snapshot, nil
}
