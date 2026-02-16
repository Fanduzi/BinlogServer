package cmd

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"binlog_server/internal/app"
	"binlog_server/internal/config"

	"github.com/spf13/cobra"
)

func NewRootCommand() *cobra.Command {
	var configPath string

	root := &cobra.Command{
		Use:   "binlog-server",
		Short: "Centralized MySQL binlog backup service",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.LoadConfig(configPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			if err := app.New(cfg).Run(ctx); err != nil {
				return fmt.Errorf("run app: %w", err)
			}
			return nil
		},
	}

	root.Flags().StringVarP(&configPath, "config", "c", "", "YAML config file path (default: ./config.yaml if exists)")
	return root
}
