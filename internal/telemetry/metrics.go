// Package telemetry defines application instruments using the OpenTelemetry API.
// Exporters and SDK lifecycle belong to the executable's startup wiring.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics groups application instruments and backlog observation state.
type Metrics struct {
	HTTP                   metric.Float64Histogram
	Streams                metric.Int64UpDownCounter
	StreamEnds             metric.Int64Counter
	Queue                  metric.Float64Histogram
	Execution              metric.Float64Histogram
	Results                metric.Int64Counter
	CleanupFailures        metric.Int64Counter
	OutboxRetries          metric.Int64Counter
	AuthorizationDecisions metric.Int64Counter
	ArtifactFailures       metric.Int64Counter
	meter                  metric.Meter
	pending                metric.Int64ObservableGauge
	oldest                 metric.Float64ObservableGauge
	cleanup                metric.Int64ObservableGauge
}

// New creates application instruments using the supplied meter.
func New(meter metric.Meter) (*Metrics, error) {
	m := &Metrics{meter: meter}
	var errs [13]error
	m.HTTP, errs[0] = meter.Float64Histogram("makez_http_request_seconds", metric.WithDescription("HTTP request duration including streaming lifetime."), metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10))
	m.Streams, errs[1] = meter.Int64UpDownCounter("makez_sse_active", metric.WithDescription("Active SSE connections."), metric.WithUnit("{connection}"))
	m.StreamEnds, errs[2] = meter.Int64Counter("makez_sse_disconnects_total", metric.WithDescription("SSE closures by bounded reason."), metric.WithUnit("{connection}"))
	m.Queue, errs[3] = meter.Float64Histogram("makez_task_queue_seconds", metric.WithDescription("Time from task submission to run initialization, observed on first initialization."), metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(1, 5, 15, 30, 60, 120, 300, 600))
	m.Execution, errs[4] = meter.Float64Histogram("makez_task_execution_seconds", metric.WithDescription("Run initialization to terminal publication; includes preparation and finalization."), metric.WithUnit("s"), metric.WithExplicitBucketBoundaries(1, 10, 30, 60, 300, 600, 1800, 3600, 5400))
	m.Results, errs[5] = meter.Int64Counter("makez_task_results_total", metric.WithDescription("Committed terminal task transitions observed by this process."), metric.WithUnit("{task}"))
	m.CleanupFailures, errs[6] = meter.Int64Counter("makez_cleanup_failures_total", metric.WithDescription("Failed sandbox deletion attempts, including retries."), metric.WithUnit("{attempt}"))
	m.OutboxRetries, errs[7] = meter.Int64Counter("makez_outbox_retries_total", metric.WithDescription("Failed outbox delivery attempts."), metric.WithUnit("{attempt}"))
	// These are installation-wide snapshots, not additive per-process counters.
	m.pending, errs[8] = meter.Int64ObservableGauge("makez_outbox_pending", metric.WithDescription("Undelivered outbox rows across the installation; do not sum across processes."), metric.WithUnit("{row}"))
	m.oldest, errs[9] = meter.Float64ObservableGauge("makez_outbox_oldest_seconds", metric.WithDescription("Age of the oldest undelivered outbox row; pre-migration rows are measured from migration."), metric.WithUnit("s"))
	m.cleanup, errs[10] = meter.Int64ObservableGauge("makez_cleanup_runs", metric.WithDescription("Runs needing cleanup across the installation; do not sum across processes."), metric.WithUnit("{run}"))
	m.AuthorizationDecisions, errs[11] = meter.Int64Counter("makez_authorization_decisions_total", metric.WithDescription("Current database authorization decisions by bounded action and outcome."), metric.WithUnit("{decision}"))
	m.ArtifactFailures, errs[12] = meter.Int64Counter("makez_artifact_integrity_failures_total", metric.WithDescription("Rejected artifact reads by bounded operation."), metric.WithUnit("{failure}"))
	if err := errors.Join(errs[:]...); err != nil {
		return nil, err
	}
	return m, nil
}

// Authorization records a decision, never identifiers or role cache contents.
func (m *Metrics) Authorization(ctx context.Context, action, outcome string) {
	if m == nil {
		return
	}
	switch action {
	case "read", "write", "members", "worker":
	default:
		action = "other"
	}
	switch outcome {
	case "allowed", "denied", "error":
	default:
		outcome = "error"
	}
	m.AuthorizationDecisions.Add(ctx, 1, metric.WithAttributes(attribute.String("action", action), attribute.String("outcome", outcome)))
}

// ArtifactFailure records an integrity rejection without exposing storage keys.
func (m *Metrics) ArtifactFailure(ctx context.Context, operation string) {
	if m == nil {
		return
	}
	if operation != "download" && operation != "archive" {
		operation = "other"
	}
	m.ArtifactFailures.Add(ctx, 1, metric.WithAttributes(attribute.String("operation", operation)))
}

// Backlog contains an installation-wide snapshot of pending work.
type Backlog struct {
	Pending                        int64
	OldestSeconds                  float64
	CleanupPending, CleanupBlocked int64
}

// RegisterBacklog samples on collection, including the final collection before
// shutdown. The callback lifetime is owned by the supplied MeterProvider.
func (m *Metrics) RegisterBacklog(read func(context.Context) (Backlog, error)) error {
	_, err := m.meter.RegisterCallback(func(ctx context.Context, observer metric.Observer) error {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		snapshot, err := read(ctx)
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			slog.WarnContext(ctx, "backlog metrics collection failed", "error_type", fmt.Sprintf("%T", err))
			// Omit failed samples instead of reusing stale values. Other instruments
			// can still be exported when the database is unavailable.
			return nil
		}
		observer.ObserveInt64(m.pending, snapshot.Pending)
		observer.ObserveFloat64(m.oldest, max(0, snapshot.OldestSeconds))
		observer.ObserveInt64(m.cleanup, snapshot.CleanupPending, metric.WithAttributes(attribute.String("state", "pending")))
		observer.ObserveInt64(m.cleanup, snapshot.CleanupBlocked, metric.WithAttributes(attribute.String("state", "blocked")))
		return nil
	}, m.pending, m.oldest, m.cleanup)
	return err
}

// Result records a committed terminal task transition.
// Metrics follow a successful commit, so a crash between commit and observation
// can miss a sample. They are operational counters, not an accounting ledger.
func (m *Metrics) Result(ctx context.Context, state string, started time.Time) {
	if m == nil {
		return
	}
	attributes := metric.WithAttributes(attribute.String("result", state))
	m.Results.Add(ctx, 1, attributes)
	if !started.IsZero() {
		m.Execution.Record(ctx, max(0, time.Since(started).Seconds()), attributes)
	}
}
