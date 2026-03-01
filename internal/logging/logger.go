// input: log configuration options, output streams, rotation backends
// output: initialized global logger and stdlib log redirection behavior
// pos: logging infrastructure setup shared by application startup and runtime paths
// note: if this file changes, update this header and module AGENTS.md.
package logging

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"binlog_server/internal/config"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Setup 初始化全局日志：使用 zap 输出，并把标准库 log 重定向到 zap。
// 返回的 cleanup 需要在进程退出前调用以停止轮转协程并 flush 缓冲。
func Setup(ctx context.Context, cfg config.LogConfig) (*zap.Logger, func(), error) {
	level := zap.NewAtomicLevel()
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		return nil, nil, fmt.Errorf("invalid log level %q: %w", cfg.Level, err)
	}

	if cfg.File == "" {
		return nil, nil, fmt.Errorf("log file is empty")
	}
	if cfg.Encoding == "" {
		return nil, nil, fmt.Errorf("log encoding is empty")
	}
	if cfg.MaxSizeMB <= 0 {
		return nil, nil, fmt.Errorf("log max_size_mb must be > 0")
	}
	if cfg.RotateInterval == "" {
		return nil, nil, fmt.Errorf("log rotate_interval is empty")
	}

	interval, err := time.ParseDuration(cfg.RotateInterval)
	if err != nil {
		return nil, nil, fmt.Errorf("parse log rotate_interval %q: %w", cfg.RotateInterval, err)
	}
	if interval <= 0 {
		return nil, nil, fmt.Errorf("log rotate_interval must be > 0")
	}

	if err := os.MkdirAll(filepath.Dir(cfg.File), 0o755); err != nil {
		return nil, nil, fmt.Errorf("create log dir: %w", err)
	}

	rotatingFile := &lumberjack.Logger{
		Filename:   cfg.File,
		MaxSize:    cfg.MaxSizeMB,
		MaxBackups: cfg.MaxBackups,
		MaxAge:     cfg.MaxAgeDays,
		Compress:   cfg.Compress,
		LocalTime:  true,
	}

	encoder, err := buildEncoder(cfg.Encoding)
	if err != nil {
		return nil, nil, err
	}

	writeSyncer := zapcore.NewMultiWriteSyncer(
		zapcore.AddSync(os.Stdout),
		zapcore.AddSync(rotatingFile),
	)

	core := zapcore.NewCore(
		encoder,
		writeSyncer,
		level,
	)
	logger := zap.New(core, zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel))
	undoStdLog := zap.RedirectStdLog(logger)
	zap.ReplaceGlobals(logger)

	stopCh := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = rotatingFile.Rotate()
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			}
		}
	}()

	cleanup := func() {
		close(stopCh)
		undoStdLog()
		_ = logger.Sync()
	}
	return logger, cleanup, nil
}

func buildEncoder(encoding string) (zapcore.Encoder, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "json":
		encoderCfg := zap.NewProductionEncoderConfig()
		encoderCfg.EncodeTime = zapcore.ISO8601TimeEncoder
		encoderCfg.TimeKey = "ts"
		encoderCfg.LevelKey = "level"
		encoderCfg.MessageKey = "msg"
		encoderCfg.CallerKey = "caller"
		encoderCfg.EncodeLevel = zapcore.LowercaseLevelEncoder
		return zapcore.NewJSONEncoder(encoderCfg), nil
	case "console":
		encoderCfg := zap.NewDevelopmentEncoderConfig()
		encoderCfg.EncodeTime = zapcore.ISO8601TimeEncoder
		encoderCfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
		return zapcore.NewConsoleEncoder(encoderCfg), nil
	default:
		return nil, fmt.Errorf("invalid log encoding %q, allowed: json|console", encoding)
	}
}
