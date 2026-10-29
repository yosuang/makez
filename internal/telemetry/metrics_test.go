package telemetry

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func testMetrics(t *testing.T) (*Metrics, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	m, err := New(provider.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	return m, reader
}

func collect(ctx context.Context, t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var data metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &data); err != nil {
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

func TestInstrumentTypesUnitsValuesAndBuckets(t *testing.T) {
	m, reader := testMetrics(t)
	ctx := t.Context()
	m.HTTP.Record(ctx, .05, metric.WithAttributes(attribute.String("method", "GET"), attribute.String("route", "/v1/tasks/:id"), attribute.String("status", "200")))
	m.Queue.Record(ctx, 5)
	m.Streams.Add(ctx, 2)
	m.Streams.Add(ctx, -1)
	m.StreamEnds.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "terminal")))
	m.CleanupFailures.Add(ctx, 1)
	m.OutboxRetries.Add(ctx, 2)
	m.Authorization(ctx, "read", "allowed")
	m.ArtifactFailure(ctx, "download")
	m.Result(ctx, "SUCCEEDED", time.Now().Add(-time.Second))
	if err := m.RegisterBacklog(func(context.Context) (Backlog, error) { return Backlog{3, 120, 2, 1}, nil }); err != nil {
		t.Fatal(err)
	}
	data := collect(ctx, t, reader)
	if len(data) != 13 {
		t.Fatalf("expected all 13 instruments, got %d", len(data))
	}
	for _, tc := range []struct {
		name   string
		bounds []float64
		sum    float64
	}{
		{"makez_http_request_seconds", []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}, .05},
		{"makez_task_queue_seconds", []float64{1, 5, 15, 30, 60, 120, 300, 600}, 5},
		{"makez_task_execution_seconds", []float64{1, 10, 30, 60, 300, 600, 1800, 3600, 5400}, -1},
	} {
		m := data[tc.name]
		h, ok := m.Data.(metricdata.Histogram[float64])
		if !ok || m.Unit != "s" || len(h.DataPoints) != 1 {
			t.Fatalf("%s: %+v", tc.name, m)
		}
		p := h.DataPoints[0]
		if p.Count != 1 || !slices.Equal(p.Bounds, tc.bounds) || len(p.BucketCounts) != len(tc.bounds)+1 {
			t.Fatalf("%s buckets/count changed: %+v", tc.name, p)
		}
		if tc.sum >= 0 && p.Sum != tc.sum {
			t.Fatalf("%s sum=%v", tc.name, p.Sum)
		}
		if tc.sum < 0 && p.Sum < 1 {
			t.Fatal("execution duration lost")
		}
	}
	for _, tc := range []struct {
		name, unit string
		value      int64
		monotonic  bool
	}{
		{"makez_authorization_decisions_total", "{decision}", 1, true}, {"makez_artifact_integrity_failures_total", "{failure}", 1, true},
		{"makez_sse_active", "{connection}", 1, false}, {"makez_sse_disconnects_total", "{connection}", 1, true},
		{"makez_task_results_total", "{task}", 1, true}, {"makez_cleanup_failures_total", "{attempt}", 1, true}, {"makez_outbox_retries_total", "{attempt}", 2, true},
	} {
		m := data[tc.name]
		sum, ok := m.Data.(metricdata.Sum[int64])
		if !ok || m.Unit != tc.unit || sum.IsMonotonic != tc.monotonic || len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != tc.value {
			t.Fatalf("%s: %+v", tc.name, m)
		}
	}
	pending := data["makez_outbox_pending"].Data.(metricdata.Gauge[int64])
	age := data["makez_outbox_oldest_seconds"].Data.(metricdata.Gauge[float64])
	cleanup := data["makez_cleanup_runs"].Data.(metricdata.Gauge[int64])
	if pending.DataPoints[0].Value != 3 || age.DataPoints[0].Value != 120 || data["makez_outbox_oldest_seconds"].Unit != "s" || data["makez_outbox_pending"].Unit != "{row}" || data["makez_cleanup_runs"].Unit != "{run}" {
		t.Fatal("backlog values or units changed")
	}
	states := map[string]int64{}
	for _, p := range cleanup.DataPoints {
		v, _ := p.Attributes.Value("state")
		states[v.AsString()] = p.Value
	}
	if len(states) != 2 || states["pending"] != 2 || states["blocked"] != 1 {
		t.Fatal(states)
	}
	allowed := map[attribute.Key]bool{"method": true, "route": true, "status": true, "reason": true, "result": true, "state": true, "action": true, "outcome": true, "operation": true}
	check := func(attrs attribute.Set) {
		for _, kv := range attrs.ToSlice() {
			if !allowed[kv.Key] {
				t.Fatalf("unbounded metric attribute: %s", kv.Key)
			}
		}
	}
	for _, m := range data {
		switch d := m.Data.(type) {
		case metricdata.Histogram[float64]:
			for _, p := range d.DataPoints {
				check(p.Attributes)
			}
		case metricdata.Sum[int64]:
			for _, p := range d.DataPoints {
				check(p.Attributes)
			}
		case metricdata.Gauge[int64]:
			for _, p := range d.DataPoints {
				check(p.Attributes)
			}
		case metricdata.Gauge[float64]:
			for _, p := range d.DataPoints {
				check(p.Attributes)
			}
		}
	}
	// Closing a stream must be observable even after the request was canceled.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	m.Streams.Add(canceled, -1)
	if got := collect(ctx, t, reader)["makez_sse_active"].Data.(metricdata.Sum[int64]).DataPoints[0].Value; got != 0 {
		t.Fatalf("active streams=%d", got)
	}
}

func TestBacklogFailureOmitsStaleSamplesAndKeepsOtherMetrics(t *testing.T) {
	m, reader := testMetrics(t)
	fail := false
	if err := m.RegisterBacklog(func(ctx context.Context) (Backlog, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 2*time.Second {
			t.Error("collection lacks a shared two-second budget")
		}
		if fail {
			return Backlog{}, errors.New("database unavailable")
		}
		return Backlog{Pending: 7}, nil
	}); err != nil {
		t.Fatal(err)
	}
	m.OutboxRetries.Add(t.Context(), 1)
	if _, ok := collect(t.Context(), t, reader)["makez_outbox_pending"]; !ok {
		t.Fatal("missing successful sample")
	}
	fail = true
	data := collect(t.Context(), t, reader)
	for _, name := range []string{"makez_outbox_pending", "makez_outbox_oldest_seconds", "makez_cleanup_runs"} {
		if _, ok := data[name]; ok {
			t.Fatalf("stale sample was reported as current: %s", name)
		}
	}
	if _, ok := data["makez_outbox_retries_total"]; !ok {
		t.Fatal("sampling failure suppressed other metrics")
	}
}

func TestBacklogCollectionHonorsCancellation(t *testing.T) {
	m, reader := testMetrics(t)
	if err := m.RegisterBacklog(func(ctx context.Context) (Backlog, error) { <-ctx.Done(); return Backlog{Pending: 99}, ctx.Err() }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	var data metricdata.ResourceMetrics
	start := time.Now()
	_ = reader.Collect(ctx, &data)
	if time.Since(start) > time.Second {
		t.Fatal("callback ignored collection deadline")
	}
	for _, scope := range data.ScopeMetrics {
		if len(scope.Metrics) != 0 {
			t.Fatal("failed collection emitted a sample")
		}
	}
}

func TestNoopMeterDoesNotSampleOrFail(t *testing.T) {
	m, err := New(noop.NewMeterProvider().Meter("disabled"))
	if err != nil {
		t.Fatal(err)
	}
	called := false
	if err := m.RegisterBacklog(func(context.Context) (Backlog, error) { called = true; return Backlog{}, nil }); err != nil {
		t.Fatal(err)
	}
	m.Result(t.Context(), "SUCCEEDED", time.Now())
	m.Streams.Add(t.Context(), 1)
	m.Streams.Add(t.Context(), -1)
	if called {
		t.Fatal("disabled metrics performed database sampling")
	}
}
