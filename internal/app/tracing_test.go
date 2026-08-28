// Package app tests tracing initialization behavior.
// input: tracing config and an in-process OTLP HTTP collector
// output: regression coverage for the default OTLP traces request path
// pos: app tracing lifecycle integration tests
// note: if this file changes, update this header and module README.md.
package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"binlog_server/internal/config"

	"go.opentelemetry.io/otel"
)

func TestInitTracingUsesDefaultPathForPathlessEndpoint(t *testing.T) {
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
	})

	paths := make(chan string, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	provider, shutdown, err := initTracing(config.TracingConfig{
		Enabled:     true,
		Exporter:    "otlp-http",
		Endpoint:    collector.URL,
		SampleRatio: 1,
		ServiceName: "test",
	})
	if err != nil {
		t.Fatalf("init tracing: %v", err)
	}

	_, span := provider.Tracer("test").Start(context.Background(), "test")
	span.End()
	if err := shutdown(t.Context()); err != nil {
		t.Fatalf("shutdown tracing: %v", err)
	}

	select {
	case path := <-paths:
		if path != "/v1/traces" {
			t.Fatalf("OTLP request path = %q, want %q", path, "/v1/traces")
		}
	case <-time.After(time.Second):
		t.Fatal("OTLP collector received no request")
	}
}
