// Package meta provides module-level functionality for meta.
// input: sqlmock-backed metadata store calls and otel tracer provider
// output: assertions that metadata store operations emit spans when enabled
// pos: regression tests for metadata tracing instrumentation
// note: if this file changes, update this header and module README.md.
package meta

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestMySQLTaskStore_ListTasks_TracingEnabled 验证启用 tracing 后会产生元数据 span。
func TestMySQLTaskStore_ListTasks_TracingEnabled(t *testing.T) {
	prevProvider := otel.GetTracerProvider()
	defer otel.SetTracerProvider(prevProvider)

	spanRecorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider()
	tracerProvider.RegisterSpanProcessor(spanRecorder)
	otel.SetTracerProvider(tracerProvider)
	ConfigureTracing(true, tracerProvider)
	defer ConfigureTracing(false, nil)

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	defer db.Close()

	store := newMySQLTaskStoreFromDB(db, 5*time.Second)
	mock.ExpectQuery(listTaskSQL).WillReturnRows(sqlmock.NewRows([]string{
		"id", "name", "cluster_key", "state", "last_error", "owner_worker_id", "epoch", "run_id", "source_json", "start_json", "storage_json", "updated_at",
	}))

	tasks, err := store.ListTasks(context.Background())
	if err != nil {
		t.Fatalf("ListTasks returned error: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("expected empty task list, got %d", len(tasks))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}

	ended := spanRecorder.Ended()
	if len(ended) == 0 {
		t.Fatal("expected metadata span when tracing is enabled")
	}
	if ended[0].Name() != "meta.mysql_store.list_tasks" {
		t.Fatalf("unexpected span name: %s", ended[0].Name())
	}
}
