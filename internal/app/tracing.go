// Package app provides module-level functionality for app.
// input: tracing runtime config (enabled/exporter/endpoint/sampling/service name)
// output: OpenTelemetry tracer provider initialization and shutdown hook
// pos: app bootstrapping layer that controls tracing lifecycle for all modules
// note: if this file changes, update this header and module README.md.
package app

import (
	"context"
	"fmt"
	"strings"

	"binlog_server/internal/config"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
)

func initTracing(cfg config.TracingConfig) (trace.TracerProvider, func(context.Context) error, error) {
	if !cfg.Enabled || cfg.Exporter == "disabled" {
		return nil, func(context.Context) error { return nil }, nil
	}

	switch cfg.Exporter {
	case "otlp-http":
	default:
		return nil, nil, fmt.Errorf("tracing exporter %q is not supported in this phase; choose otlp-http or disabled", cfg.Exporter)
	}

	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		return nil, nil, fmt.Errorf("tracing endpoint is required when tracing is enabled")
	}
	serviceName := strings.TrimSpace(cfg.ServiceName)
	if serviceName == "" {
		serviceName = "binlog-server"
	}

	exporter, err := otlptracehttp.New(
		context.Background(),
		otlptracehttp.WithEndpointURL(endpoint),
	)
	if err != nil {
		return nil, nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(serviceName),
		)),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return tp, tp.Shutdown, nil
}
