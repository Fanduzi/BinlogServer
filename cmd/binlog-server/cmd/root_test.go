// Package cmd provides module-level functionality for cmd.
// input: root cobra command construction, test-time output capture, startup hook overrides
// output: CLI regression tests for version command, version flag, and arg validation
// pos: command-layer behavioral safety net for binlog-server user-facing CLI semantics
// note: if this file changes, update this header and module README.md.
package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func setTestVersionInfo(version, commit, date string) func() {
	prevVersion, prevCommit, prevDate := buildVersion, buildCommit, buildDate
	buildVersion, buildCommit, buildDate = version, commit, date
	return func() {
		buildVersion, buildCommit, buildDate = prevVersion, prevCommit, prevDate
	}
}

func setTestRunRootApp(fn func(string, string) error) func() {
	prev := runRootApp
	runRootApp = fn
	return func() {
		runRootApp = prev
	}
}

func TestRootCommandVersionSubcommandPrintsBannerWithoutStartingApp(t *testing.T) {
	restoreVersion := setTestVersionInfo("v9.9.9-test", "deadbeef", "2026-03-12")
	defer restoreVersion()

	var startCalls int
	restoreRun := setTestRunRootApp(func(string, string) error {
		startCalls++
		return nil
	})
	defer restoreRun()

	cmd := NewRootCommand()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{"version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if startCalls != 0 {
		t.Fatalf("expected app startup to be skipped, got %d calls", startCalls)
	}

	output := stdout.String()
	if !strings.Contains(output, "BinlogServer") {
		t.Fatalf("expected banner to contain project name, got %q", output)
	}
	if !strings.Contains(output, "v9.9.9-test") {
		t.Fatalf("expected banner to contain version, got %q", output)
	}
	if !strings.Contains(output, "_") {
		t.Fatalf("expected banner to contain ascii logo, got %q", output)
	}
}

func TestRootCommandVersionFlagPrintsVersionOnlyWithoutStartingApp(t *testing.T) {
	restoreVersion := setTestVersionInfo("v1.2.3-test", "deadbeef", "2026-03-12")
	defer restoreVersion()

	var startCalls int
	restoreRun := setTestRunRootApp(func(string, string) error {
		startCalls++
		return nil
	})
	defer restoreRun()

	cmd := NewRootCommand()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{"--version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if startCalls != 0 {
		t.Fatalf("expected app startup to be skipped, got %d calls", startCalls)
	}

	output := strings.TrimSpace(stdout.String())
	if output != "v1.2.3-test" {
		t.Fatalf("expected version-only output, got %q", output)
	}
}

func TestRootCommandRejectsUnexpectedPositionalArgs(t *testing.T) {
	var startCalls int
	restoreRun := setTestRunRootApp(func(string, string) error {
		startCalls++
		return nil
	})
	defer restoreRun()

	cmd := NewRootCommand()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs([]string{"unexpected"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected positional arg validation error")
	}
	if startCalls != 0 {
		t.Fatalf("expected app startup to be skipped, got %d calls", startCalls)
	}
	if !strings.Contains(err.Error(), "unknown command") && !strings.Contains(err.Error(), "accepts 0 arg(s)") {
		t.Fatalf("expected arg-related error, got %v", err)
	}
}

func TestRootCommandWithoutArgsStillStartsApp(t *testing.T) {
	var startCalls int
	restoreRun := setTestRunRootApp(func(string, string) error {
		startCalls++
		return nil
	})
	defer restoreRun()

	cmd := NewRootCommand()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs(nil)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if startCalls != 1 {
		t.Fatalf("expected app startup to run once, got %d calls", startCalls)
	}
}
