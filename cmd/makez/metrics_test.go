package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/makez-dev/makez/internal/telemetry"
	"go.opentelemetry.io/otel/metric/noop"
	collector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

func clearMetricsEnvironment(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "OTEL_") {
			t.Setenv(name, "")
		}
	}
}

func TestMetricsDisabledByDefaultAndExplicitOptOut(t *testing.T) {
	clearMetricsEnvironment(t)
	var requests atomic.Int64
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer receiver.Close()
	for _, mode := range []string{"unconfigured", "none", "sdk-disabled", "otlp-without-endpoint"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("OTEL_METRICS_EXPORTER", "")
			t.Setenv("OTEL_SDK_DISABLED", "")
			switch mode {
			case "none":
				t.Setenv("OTEL_METRICS_EXPORTER", "none")
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", receiver.URL)
			case "sdk-disabled":
				t.Setenv("OTEL_SDK_DISABLED", "true")
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", receiver.URL)
			case "otlp-without-endpoint":
				t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
			}
			provider, shutdown, err := configureMetrics(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := provider.(noop.MeterProvider); !ok {
				t.Fatalf("disabled metrics created an SDK: %T", provider)
			}
			m, err := telemetry.New(provider.Meter("disabled-test"))
			if err != nil {
				t.Fatal(err)
			}
			m.Result(t.Context(), "SUCCEEDED", time.Now())
			if err := shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatal("disabled metrics sent a network request")
	}
}

func TestMetricsConfigurationRejectsInvalidEndpointAndProtocol(t *testing.T) {
	clearMetricsEnvironment(t)
	for _, tc := range []struct{ name, value string }{
		{"OTEL_EXPORTER_OTLP_ENDPOINT", "not-a-url"},
		{"OTEL_EXPORTER_OTLP_ENDPOINT", "ftp://localhost"},
		{"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "grpc"},
		{"OTEL_METRICS_EXPORTER", "prometheus"},
		{"OTEL_SDK_DISABLED", "invalid"},
	} {
		t.Run(tc.name+tc.value, func(t *testing.T) {
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
			t.Setenv(tc.name, tc.value)
			if _, _, err := configureMetrics(t.Context()); err == nil {
				t.Fatal("invalid configuration silently accepted")
			}
		})
	}
}

func TestOTLPHTTPFinalExportUsesStandardConfiguration(t *testing.T) {
	clearMetricsEnvironment(t)
	type received struct {
		path, header, contentType string
		data                      *collector.ExportMetricsServiceRequest
		err                       error
	}
	samples := make(chan received, 2)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		data := new(collector.ExportMetricsServiceRequest)
		if err == nil {
			err = proto.Unmarshal(raw, data)
		}
		samples <- received{r.URL.Path, r.Header.Get("X-Test-Token"), r.Header.Get("Content-Type"), data, err}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer receiver.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1/unused")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", receiver.URL+"/custom-metrics")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "X-Test-Token=general")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_HEADERS", "X-Test-Token=metrics-test-only")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "3600000")
	t.Setenv("OTEL_SERVICE_NAME", "makez-test")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment.name=test")
	runCtx, stop := context.WithCancel(t.Context())
	provider, shutdown, err := configureMetrics(runCtx)
	if err != nil {
		t.Fatal(err)
	}
	m, err := telemetry.New(provider.Meter("otlp-test"))
	if err != nil {
		t.Fatal(err)
	}
	m.Result(runCtx, "SUCCEEDED", time.Now().Add(-time.Second))
	if err := m.RegisterBacklog(func(ctx context.Context) (telemetry.Backlog, error) {
		if ctx.Err() != nil {
			t.Error("shutdown reused canceled application context")
		}
		return telemetry.Backlog{Pending: 3, OldestSeconds: 20}, nil
	}); err != nil {
		t.Fatal(err)
	}
	stop()
	// A fresh shutdown context flushes the final sample after producers stop.
	flushCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := shutdown(flushCtx); err != nil {
		t.Fatal(err)
	}
	var sample received
	select {
	case sample = <-samples:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not export final metrics")
	}
	if sample.err != nil || sample.path != "/custom-metrics" || sample.header != "metrics-test-only" || sample.contentType != "application/x-protobuf" {
		t.Fatalf("wrong OTLP request: %+v", sample)
	}
	if len(sample.data.ResourceMetrics) != 1 {
		t.Fatal("missing resource metrics")
	}
	rm := sample.data.ResourceMetrics[0]
	attrs := map[string]string{}
	for _, kv := range rm.Resource.Attributes {
		attrs[kv.Key] = kv.Value.GetStringValue()
	}
	if attrs["service.name"] != "makez-test" || attrs["deployment.environment.name"] != "test" {
		t.Fatal(attrs)
	}
	found := map[string]bool{}
	for _, scope := range rm.ScopeMetrics {
		for _, metric := range scope.Metrics {
			switch metric.Name {
			case "makez_task_results_total":
				if metric.GetSum().DataPoints[0].GetAsInt() != 1 {
					t.Fatal("wrong committed result count")
				}
			case "makez_outbox_pending":
				if metric.GetGauge().DataPoints[0].GetAsInt() != 3 {
					t.Fatal("missing final backlog sample")
				}
			case "makez_task_execution_seconds":
				if metric.Unit != "s" || metric.GetHistogram().DataPoints[0].Count != 1 {
					t.Fatal("wrong duration instrument")
				}
			default:
				continue
			}
			found[metric.Name] = true
		}
	}
	if len(found) != 3 {
		t.Fatal("missing OTLP measurements", found)
	}
}

func TestPeriodicExportDoesNotBlockRecordingAndShutdownIsBounded(t *testing.T) {
	clearMetricsEnvironment(t)
	entered := make(chan string, 1)
	release := make(chan struct{})
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- r.URL.Path:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer receiver.Close()
	defer close(release)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", receiver.URL)
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "20")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TIMEOUT", "100")
	provider, shutdown, err := configureMetrics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	m, err := telemetry.New(provider.Meter("periodic-test"))
	if err != nil {
		t.Fatal(err)
	}
	m.Results.Add(t.Context(), 1)
	select {
	case path := <-entered:
		if path != "/v1/metrics" {
			t.Fatal(path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("periodic reader did not export")
	}
	recorded := make(chan struct{})
	go func() { m.Results.Add(t.Context(), 1); close(recorded) }()
	select {
	case <-recorded:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("business recording waited for network export")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := shutdown(ctx); err == nil {
		t.Fatal("blocked export unexpectedly succeeded")
	}
	if time.Since(start) > time.Second {
		t.Fatal("shutdown ignored independent deadline")
	}
}
