package store

import "context"

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// RefreshMetrics reads only indexed operational queues, not task/event history.
func (s *Store) RefreshMetrics(ctx context.Context) error {
	if s.Metrics == nil {
		return nil
	}
	var pending, oldest float64
	err := s.pool.QueryRow(ctx, `SELECT count(*),COALESCE(EXTRACT(EPOCH FROM now()-min(created_at)),0) FROM makez_outbox WHERE delivered_at IS NULL`).Scan(&pending, &oldest)
	if err != nil {
		return err
	}
	var waiting, blocked float64
	err = s.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE cleanup='PENDING'),count(*) FILTER(WHERE cleanup='BLOCKED') FROM makez_runs WHERE cleanup IN ('PENDING','BLOCKED')`).Scan(&waiting, &blocked)
	if err != nil {
		return err
	}
	s.Metrics.Pending.Set(pending)
	s.Metrics.Oldest.Set(max(0, oldest))
	s.Metrics.Cleanup.WithLabelValues("pending").Set(waiting)
	s.Metrics.Cleanup.WithLabelValues("blocked").Set(blocked)
	return nil
}
