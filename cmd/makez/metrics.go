package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// configureMetrics is the only production code that selects an SDK and exporter.
// No endpoint means no SDK, background reader, database sampling, or network I/O.
// OTEL_METRICS_EXPORTER=none or OTEL_SDK_DISABLED=true explicitly disables it.
func configureMetrics(ctx context.Context) (metric.MeterProvider, func(context.Context) error, error) {
	disabled := false
	if raw := os.Getenv("OTEL_SDK_DISABLED"); raw != "" {
		var err error
		disabled, err = strconv.ParseBool(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("OTEL_SDK_DISABLED must be a boolean")
		}
	}
	exporterName := strings.TrimSpace(os.Getenv("OTEL_METRICS_EXPORTER"))
	if disabled || exporterName == "none" {
		return noop.NewMeterProvider(), func(context.Context) error { return nil }, nil
	}
	if exporterName != "" && exporterName != "otlp" {
		return nil, nil, fmt.Errorf("OTEL_METRICS_EXPORTER must be otlp or none")
	}
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT")
	if endpoint == "" {
		endpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if endpoint == "" {
		return noop.NewMeterProvider(), func(context.Context) error { return nil }, nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Fragment != "" {
		return nil, nil, fmt.Errorf("OTLP metrics endpoint must be an absolute HTTP(S) URL without a fragment")
	}
	protocol := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL")
	if protocol == "" {
		protocol = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	if protocol != "" && protocol != "http/protobuf" {
		return nil, nil, fmt.Errorf("OTLP metrics protocol must be http/protobuf")
	}
	// Never send error strings containing exporter URLs or credentials to logs.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		slog.Warn("metrics collection or export failed", "error_type", fmt.Sprintf("%T", err))
	}))
	res, err := resource.New(ctx, resource.WithAttributes(attribute.String("service.name", "makez")), resource.WithFromEnv())
	if err != nil {
		return nil, nil, err
	}
	// Standard OTEL_EXPORTER_OTLP_* / OTEL_EXPORTER_OTLP_METRICS_* settings
	// configure endpoint, headers, TLS and timeout; no backend is hard-coded.
	exporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, nil, err
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)))
	// PeriodicReader.Shutdown stops collection and performs a final export using
	// the caller's deadline. No separate ForceFlush (and duplicate export) is needed.
	return provider, provider.Shutdown, nil
}
