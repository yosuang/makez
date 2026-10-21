package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/telemetry"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestRecoveryCursorVisitsBeyond500WithEqualTimestamps(t *testing.T) {
	s, workspace := testStore(t)
	_, err := s.pool.Exec(t.Context(), `INSERT INTO makez_tasks(id,workspace_id,idempotency_key,request_hash,prompt,agent_id,agent_version,profile_id,profile_version,state,deadline,created_at)
 SELECT 'page-'||lpad(i::text,4,'0'),$1,'key-'||i,'hash','test','demo','1','default','1','QUEUED',now()+interval '1 hour',now() FROM generate_series(1,503) i`, workspace)
	if err != nil {
		t.Fatal(err)
	}
	cursor := RecoveryCursor{}
	seen := map[string]bool{}
	for range 7 {
		tasks, err := s.Recoverable(t.Context(), cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, task := range tasks {
			if seen[task.ID] {
				t.Fatal("cursor repeated task", task.ID)
			}
			seen[task.ID] = true
			cursor = RecoveryCursor{CreatedAt: task.CreatedAt, ID: task.ID}
		}
		if len(tasks) < RecoveryPageSize {
			break
		}
		// A completed task disappearing before the next page must not shift offsets.
		if _, err := s.pool.Exec(t.Context(), `UPDATE makez_tasks SET state='CANCELED' WHERE id=$1`, tasks[0].ID); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 503 || !seen["page-0503"] {
		t.Fatalf("recovery starved tasks: visited=%d", len(seen))
	}
}

func TestCommittedTaskMetricsDoNotCountReplays(t *testing.T) {
	s, workspace := testStore(t)
	reader := attachTestMetrics(t, s)
	task := prepareRun(t, s, workspace)
	if _, err := s.InitializeRun(t.Context(), workspace, task.ID, "test-backend"); err != nil {
		t.Fatal(err)
	}
	for _, state := range []core.RunState{core.RunStopping, core.RunFinalizing} {
		if err := s.UpdateRun(t.Context(), task.Run.ID, RunUpdate{State: new(state)}); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := s.Publish(t.Context(), workspace, task.ID, core.TaskSucceeded, "", true, core.CleanupDone, nil); err != nil {
			t.Fatal(err)
		}
	}
	metrics := collectStoreMetrics(t, reader)
	for _, name := range []string{"makez_task_queue_seconds", "makez_task_execution_seconds"} {
		histogram, ok := metrics[name].Data.(metricdata.Histogram[float64])
		if !ok || len(histogram.DataPoints) != 1 || histogram.DataPoints[0].Count != 1 {
			t.Fatalf("%s missing or counted a replay: %+v", name, histogram)
		}
	}
	results, ok := metrics["makez_task_results_total"].Data.(metricdata.Sum[int64])
	if !ok || len(results.DataPoints) != 1 || results.DataPoints[0].Value != 1 {
		t.Fatalf("result missing or counted a replay: %+v", results)
	}

}

func TestOutboxAgeSurvivesRetryAndPoolIsBounded(t *testing.T) {
	s, workspace := testStore(t)
	reader := attachTestMetrics(t, s)
	task := prepareRun(t, s, workspace)
	_, err := s.pool.Exec(t.Context(), `UPDATE makez_outbox SET created_at=now()-interval '1 hour' WHERE task_id=$1`, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.Pending(t.Context(), 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%d err=%v", len(pending), err)
	}
	if err := s.RetryOutbox(t.Context(), pending[0].ID, "test"); err != nil {
		t.Fatal(err)
	}
	metrics := collectStoreMetrics(t, reader)
	age, ok := metrics["makez_outbox_oldest_seconds"].Data.(metricdata.Gauge[float64])
	if !ok || len(age.DataPoints) != 1 || age.DataPoints[0].Value < 3590 {
		t.Fatalf("missing age or retry reset it: %+v", age)
	}

	if got := s.pool.Config().MaxConns; got != 16 {
		t.Fatalf("default pool budget=%d", got)
	}
	run, err := s.Get(t.Context(), workspace, task.ID)
	if err != nil || run.Run.StartedAt == nil || time.Since(*run.Run.StartedAt) > time.Minute {
		t.Fatal("run initialization time not persisted", err)
	}
}

func TestPoolBudgetValidation(t *testing.T) {
	for _, limit := range []int32{-1, 0, 1, 257} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			if _, err := OpenWithLimit(t.Context(), "", limit); err == nil {
				t.Fatal("invalid pool budget accepted")
			}
		})
	}
}

func TestMigrationThreePreservesExistingRuns(t *testing.T) {
	s, workspace := testStore(t)
	task := prepareRun(t, s, workspace)
	// Roll back only this test's private schema to the previous shape.
	_, err := s.pool.Exec(t.Context(), `ALTER TABLE makez_runs DROP COLUMN started_at;
 ALTER TABLE makez_outbox DROP COLUMN created_at;
 DROP INDEX makez_tasks_recovery_cursor;
 DROP INDEX makez_runs_cleanup_needed;
 DELETE FROM makez_schema_migrations WHERE version=3`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Get(t.Context(), workspace, task.ID)
	if err != nil || got.Run == nil || got.Run.ID != task.Run.ID || got.Run.StartedAt != nil {
		t.Fatalf("migration must preserve old runs without inventing a start time: %v", err)
	}
	pending, err := s.Pending(t.Context(), 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("migration lost outbox: %v", err)
	}
}

func TestRecoverySweepExcludesNewArrivalsUntilRestart(t *testing.T) {
	s, workspace := testStore(t)
	task := prepareRun(t, s, workspace)
	var through time.Time
	if err := s.pool.QueryRow(t.Context(), `SELECT now()`).Scan(&through); err != nil {
		t.Fatal(err)
	}
	cursor := RecoveryCursor{Through: through}
	first, err := s.Recoverable(t.Context(), cursor)
	if err != nil || len(first) != 1 {
		t.Fatalf("initial sweep: %d %v", len(first), err)
	}
	cursor.CreatedAt, cursor.ID = first[0].CreatedAt, first[0].ID
	_, err = s.pool.Exec(t.Context(), `INSERT INTO makez_tasks(id,workspace_id,idempotency_key,request_hash,prompt,agent_id,agent_version,profile_id,profile_version,state,deadline,created_at)
 VALUES('later-arrival',$1,'later','hash','test','demo','1','default','1','QUEUED',now()+interval '1 hour',$2)`, workspace, through.Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	rest, err := s.Recoverable(t.Context(), cursor)
	if err != nil || len(rest) != 0 {
		t.Fatalf("new arrivals extended current sweep: %d %v", len(rest), err)
	}
	next, err := s.Recoverable(t.Context(), RecoveryCursor{Through: through.Add(time.Second)})
	if err != nil || len(next) != 2 {
		t.Fatalf("restart missed existing/new tasks: %d %v", len(next), err)
	}
	if next[0].ID != task.ID || next[1].ID != "later-arrival" {
		t.Fatal("sweep order changed")
	}
}

func attachTestMetrics(t *testing.T, s *Store) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	var err error
	s.Metrics, err = telemetry.New(provider.Meter("store-test"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Metrics.RegisterBacklog(s.ReadBacklog); err != nil {
		t.Fatal(err)
	}
	return reader
}

func collectStoreMetrics(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var data metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &data); err != nil {
		t.Fatal(err)
	}
	result := map[string]metricdata.Metrics{}
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			result[m.Name] = m
		}
	}
	return result
}
