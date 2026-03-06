// Package meta provides module-level functionality for meta.
// input: context from callers and optional tracer provider configured by app
// output: metadata store spans around DB-backed store operations
// pos: observability hook inside metadata persistence layer
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

var metaTracingEnabled atomic.Bool

func ConfigureTracing(enabled bool, provider trace.TracerProvider) {
	metaTracingEnabled.Store(enabled)
	_ = provider
}

func startMetaSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	if !metaTracingEnabled.Load() {
		return ctx, nil
	}

	provider := otel.GetTracerProvider()
	return provider.Tracer("binlog_server/internal/meta").Start(ctx, name)
}

func endMetaSpan(span trace.Span) {
	if span != nil {
		span.End()
	}
}
