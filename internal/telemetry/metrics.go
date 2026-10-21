// Package telemetry holds process-local metrics; durable state remains in PostgreSQL.
package telemetry

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type Metrics struct {
	Registry        *prometheus.Registry
	HTTP            *prometheus.HistogramVec
	Streams         prometheus.Gauge
	StreamEnds      *prometheus.CounterVec
	Queue           prometheus.Histogram
	Execution       *prometheus.HistogramVec
	Results         *prometheus.CounterVec
	CleanupFailures prometheus.Counter
	OutboxRetries   prometheus.Counter
	Pending         prometheus.Gauge
	Oldest          prometheus.Gauge
	Cleanup         *prometheus.GaugeVec
}

func New() *Metrics {
	m := &Metrics{
		Registry:        prometheus.NewRegistry(),
		HTTP:            prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "makez_http_request_seconds", Help: "HTTP request duration including streaming lifetime.", Buckets: prometheus.DefBuckets}, []string{"method", "route", "status"}),
		Streams:         prometheus.NewGauge(prometheus.GaugeOpts{Name: "makez_sse_active", Help: "Active SSE connections."}),
		StreamEnds:      prometheus.NewCounterVec(prometheus.CounterOpts{Name: "makez_sse_disconnects_total", Help: "SSE closures by bounded reason."}, []string{"reason"}),
		Queue:           prometheus.NewHistogram(prometheus.HistogramOpts{Name: "makez_task_queue_seconds", Help: "Time from task submission to run initialization, observed on first initialization.", Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600}}),
		Execution:       prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "makez_task_execution_seconds", Help: "Run initialization to terminal publication; includes preparation and finalization.", Buckets: []float64{1, 10, 30, 60, 300, 600, 1800, 3600, 5400}}, []string{"result"}),
		Results:         prometheus.NewCounterVec(prometheus.CounterOpts{Name: "makez_task_results_total", Help: "Committed terminal task transitions observed by this process."}, []string{"result"}),
		CleanupFailures: prometheus.NewCounter(prometheus.CounterOpts{Name: "makez_cleanup_failures_total", Help: "Failed sandbox deletion attempts, including retries."}),
		OutboxRetries:   prometheus.NewCounter(prometheus.CounterOpts{Name: "makez_outbox_retries_total", Help: "Failed outbox delivery attempts."}),
		Pending:         prometheus.NewGauge(prometheus.GaugeOpts{Name: "makez_outbox_pending", Help: "Undelivered outbox rows across the installation."}),
		Oldest:          prometheus.NewGauge(prometheus.GaugeOpts{Name: "makez_outbox_oldest_seconds", Help: "Age of the oldest undelivered outbox row; pre-migration rows are measured from migration."}),
		Cleanup:         prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "makez_cleanup_runs", Help: "Runs needing cleanup across the installation."}, []string{"state"}),
	}
	m.Registry.MustRegister(m.HTTP, m.Streams, m.StreamEnds, m.Queue, m.Execution, m.Results, m.CleanupFailures, m.OutboxRetries, m.Pending, m.Oldest, m.Cleanup)
	return m
}

// Metrics follow a successful commit, so a crash between commit and observation
// can miss a sample. They are operational counters, not an accounting ledger.
func (m *Metrics) Result(state string, started time.Time) {
	if m == nil {
		return
	}
	m.Results.WithLabelValues(state).Inc()
	if !started.IsZero() {
		m.Execution.WithLabelValues(state).Observe(max(0, time.Since(started).Seconds()))
	}
}
