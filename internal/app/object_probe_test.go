// Package app provides module-level functionality for app.
// input: a fake object client and the startup probe timeout
// output: proof that the probe deadline stays near 2s, a fast healthy probe is success, a timeout says "timed out after", and a failure is warn-level and names the bucket and endpoint without secrets
// pos: startup object-probe tests for ADR 0009 phase 3
// note: if this file changes, update this header and module README.md.
package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

type deadlineSee struct{ remain time.Duration }

func (d *deadlineSee) ProbeStorage(ctx context.Context) error {
	dl, ok := ctx.Deadline()
	if !ok {
		return errors.New("no deadline")
	}
	d.remain = time.Until(dl)
	return nil
}

func TestObjectProbe_DeadlineStaysNearTwoSeconds(t *testing.T) {
	var seen deadlineSee
	detail := objectProbeDetail(context.Background(), &seen, nil, "binlogs", "127.0.0.1:9000", "access-value", "secret-value")
	if detail != "" {
		t.Fatal(detail)
	}
	if seen.remain > 5*time.Second || seen.remain < time.Second {
		t.Fatalf("probe deadline %s, want about 2s", seen.remain)
	}
}

type fastHealthy struct{}

func (fastHealthy) ProbeStorage(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < time.Millisecond {
		return context.DeadlineExceeded
	}
	return nil
}

func TestObjectProbe_FastHealthyIsOK(t *testing.T) {
	detail := objectProbeDetail(context.Background(), fastHealthy{}, nil, "binlogs", "127.0.0.1:9000", "", "")
	if detail != "" {
		t.Fatalf("fast healthy probe: %s", detail)
	}
}

type alreadyTimedOut struct{}

func (alreadyTimedOut) ProbeStorage(context.Context) error { return context.DeadlineExceeded }

func TestObjectProbe_TimeoutSaysTimedOutAfter(t *testing.T) {
	obs := captureAppZap(t, zapcore.InfoLevel)
	detail := objectProbeDetail(context.Background(), alreadyTimedOut{}, nil, "binlogs", "127.0.0.1:9000", "access-value", "secret-value")
	if !strings.Contains(detail, "timed out after") {
		t.Fatalf("detail %q", detail)
	}
	if strings.Contains(detail, "secret-value") || strings.Contains(detail, "access-value") {
		t.Fatalf("timeout detail includes a credential: %s", detail)
	}
	if !strings.Contains(detail, "bucket binlogs") || !strings.Contains(detail, "endpoint 127.0.0.1:9000") {
		t.Fatalf("timeout detail %q", detail)
	}
	if !sawAppLevel(obs, "verifier: object storage probe", zapcore.WarnLevel) {
		t.Fatal("probe failure was not logged at warn")
	}
}

type leakyProbe struct{}

func (leakyProbe) ProbeStorage(context.Context) error {
	return errors.New("AccessDenied secret-value access-value")
}

func TestObjectProbe_NamesBucketAndScrubsSecret(t *testing.T) {
	obs := captureAppZap(t, zapcore.InfoLevel)
	detail := objectProbeDetail(context.Background(), leakyProbe{}, nil, "binlogs", "127.0.0.1:9000", "access-value", "secret-value")
	if strings.Contains(detail, "secret-value") || strings.Contains(detail, "access-value") {
		t.Fatalf("detail includes a credential: %s", detail)
	}
	if !strings.Contains(detail, "AccessDenied") || !strings.Contains(detail, "bucket binlogs") || !strings.Contains(detail, "endpoint 127.0.0.1:9000") {
		t.Fatalf("detail %q", detail)
	}
	if !sawAppLevel(obs, "verifier: object storage probe", zapcore.WarnLevel) {
		t.Fatal("probe failure was not logged at warn")
	}
}

func TestObjectProbe_HeadlessLogsWarn(t *testing.T) {
	obs := captureAppZap(t, zapcore.InfoLevel)
	detail := objectProbeDetail(context.Background(), headlessUpload{}, nil, "binlogs", "127.0.0.1:9000", "access-value", "secret-value")
	if !strings.Contains(detail, "HEAD") {
		t.Fatalf("detail %q", detail)
	}
	if !sawAppLevel(obs, "verifier: object client cannot HEAD and GET", zapcore.WarnLevel) {
		t.Fatal("missing HEAD was not logged at warn")
	}
}

func captureAppZap(t *testing.T, level zapcore.Level) *observer.ObservedLogs {
	t.Helper()
	core, obs := observer.New(level)
	prev := zap.L()
	zap.ReplaceGlobals(zap.New(core))
	t.Cleanup(func() { zap.ReplaceGlobals(prev) })
	return obs
}

func sawAppLevel(obs *observer.ObservedLogs, msg string, level zapcore.Level) bool {
	for _, e := range obs.All() {
		if strings.Contains(e.Message, msg) && e.Level == level {
			return true
		}
	}
	return false
}
