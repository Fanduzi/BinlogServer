// Package api provides module-level functionality for api.
// input: inbound HTTP request context, tracing config, and handler execution result
// output: OpenTelemetry server spans for API entrypoint requests
// pos: API ingress observability middleware between auth and business handlers
// note: if this file changes, update this header and module README.md.
package api

import (
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func tracingMiddleware(cfg TracingConfig) gin.HandlerFunc {
	provider := cfg.TracerProvider
	if provider == nil {
		provider = otel.GetTracerProvider()
	}
	serviceName := strings.TrimSpace(cfg.ServiceName)
	if serviceName == "" {
		serviceName = "binlog-server"
	}
	tracer := provider.Tracer("binlog_server/internal/api")

	return func(c *gin.Context) {
		route := c.FullPath()
		if route == "" {
			route = c.Request.URL.Path
		}
		spanName := c.Request.Method + " " + route
		ctx, span := tracer.Start(c.Request.Context(), spanName, trace.WithSpanKind(trace.SpanKindServer))
		span.SetAttributes(
			attribute.String("service.name", serviceName),
			attribute.String("http.method", c.Request.Method),
			attribute.String("http.route", route),
			attribute.String("http.target", c.Request.URL.RequestURI()),
		)

		c.Request = c.Request.WithContext(ctx)
		c.Next()

		span.SetAttributes(attribute.Int("http.status_code", c.Writer.Status()))
		if len(c.Errors) > 0 {
			err := errors.New(c.Errors.String())
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
}
