// Package cmd provides module-level functionality for cmd.
// input: config loader, optional --encryption-key, signal context, logging setup, app runtime dependencies
// output: root cobra command that starts binlog-server without dumping Usage on bind errors
// pos: CLI orchestration entry between process bootstrap and app lifecycle
// note: if this file changes, update this header and module README.md.
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var runRootApp = defaultRunRootApp

// NewRootCommand 创建 binlog-server CLI 根命令。
func NewRootCommand() *cobra.Command {
	var configPath string
	var encryptionKey string
	var versionOnly bool

	root := &cobra.Command{
		Use:           "binlog-server",
		Short:         "Centralized MySQL binlog backup service",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if versionOnly {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), effectiveVersionInfo().Version)
				return err
			}
			return runRootApp(configPath, encryptionKey)
		},
	}

	root.Flags().StringVarP(&configPath, "config", "c", "", "YAML config file path (default: ./config.yaml if exists)")
	root.Flags().StringVar(&encryptionKey, "encryption-key", "", "AES-256 key (32 bytes) for enc:aes256: config values and source passwords in meta")
	root.Flags().BoolVar(&versionOnly, "version", false, "Print version and exit")
	root.AddCommand(newVersionCommand())
	return root
}
