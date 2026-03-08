// Package cmd provides module-level functionality for cmd.
// input: config loader, signal context, logging setup, app runtime dependencies
// output: root cobra command that starts binlog-server in configured mode
// pos: CLI orchestration entry between process bootstrap and app lifecycle
// note: if this file changes, update this header and module README.md.
package cmd

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"binlog_server/internal/app"
	"binlog_server/internal/config"
	"binlog_server/internal/logging"

	"github.com/spf13/cobra"
)

// NewRootCommand 创建 binlog-server CLI 根命令。
func NewRootCommand() *cobra.Command {
	var configPath string
	var encryptionKey string

	root := &cobra.Command{
		Use:   "binlog-server",
		Short: "Centralized MySQL binlog backup service",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.LoadConfigWithEncryption(configPath, encryptionKey)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			_, cleanupLogger, err := logging.Setup(ctx, cfg.Log)
			if err != nil {
				return fmt.Errorf("setup logger: %w", err)
			}
			defer cleanupLogger()

			if err := app.New(cfg).Run(ctx); err != nil {
				return fmt.Errorf("run app: %w", err)
			}
			return nil
		},
	}

	root.Flags().StringVarP(&configPath, "config", "c", "", "YAML config file path (default: ./config.yaml if exists)")
	root.Flags().StringVar(&encryptionKey, "encryption-key", "", "Encryption key for encrypted config values (32 bytes for AES-256)")
	return root
}
