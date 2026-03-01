// Package logging provides module-level functionality for logging.
// input: log configuration options, output streams, rotation backends
// output: initialized global logger and stdlib log redirection behavior
// pos: logging infrastructure setup shared by application startup and runtime paths
// note: if this file changes, update this header and module README.md.
package logging

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"binlog_server/internal/config"
)

// TestSetup_InvalidRotateInterval 验证非法轮转间隔会返回错误。
func TestSetup_InvalidRotateInterval(t *testing.T) {
	_, cleanup, err := Setup(context.Background(), config.LogConfig{
		Level:          "info",
		Encoding:       "json",
		File:           filepath.Join(t.TempDir(), "app.log"),
		MaxSizeMB:      10,
		MaxBackups:     3,
		MaxAgeDays:     7,
		RotateInterval: "bad-duration",
	})
	if cleanup != nil {
		cleanup()
	}
	if err == nil {
		t.Fatal("expected error for invalid rotate interval")
	}
}

// TestSetup_InvalidEncoding 验证非法日志编码会返回错误。
func TestSetup_InvalidEncoding(t *testing.T) {
	_, cleanup, err := Setup(context.Background(), config.LogConfig{
		Level:          "info",
		Encoding:       "bad",
		File:           filepath.Join(t.TempDir(), "app.log"),
		MaxSizeMB:      10,
		MaxBackups:     3,
		MaxAgeDays:     7,
		RotateInterval: "1h",
	})
	if cleanup != nil {
		cleanup()
	}
	if err == nil {
		t.Fatal("expected error for invalid encoding")
	}
}

// TestSetup_RedirectStdLog 验证标准库日志会被重定向并写入轮转文件。
func TestSetup_RedirectStdLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "app.log")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, cleanup, err := Setup(ctx, config.LogConfig{
		Level:          "info",
		Encoding:       "console",
		File:           logPath,
		MaxSizeMB:      10,
		MaxBackups:     3,
		MaxAgeDays:     7,
		RotateInterval: "1h",
	})
	if err != nil {
		t.Fatalf("setup logger: %v", err)
	}
	defer cleanup()

	log.Print("hello from stdlib logger")
	time.Sleep(50 * time.Millisecond)

	info, statErr := os.Stat(logPath)
	if statErr != nil {
		t.Fatalf("stat log file: %v", statErr)
	}
	if info.Size() == 0 {
		t.Fatal("expected log file to contain data")
	}
}
