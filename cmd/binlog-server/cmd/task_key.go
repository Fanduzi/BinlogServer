// Package cmd provides module-level functionality for cmd.
// input: task-key generate --out <path>
// output: a new random 32-byte standalone task key written base64 to a new 0600 file (an existing path is refused); prints the path and key id, never the key
// pos: ADR 0010 (#385) key tooling for the standalone task record
// note: if this file changes, update this header and module README.md.
package cmd

import (
	"errors"
	"fmt"

	"binlog_server/internal/taskrecords"

	"github.com/spf13/cobra"
)

func newTaskKeyCommand() *cobra.Command {
	group := &cobra.Command{
		Use:   "task-key",
		Short: "Manage the standalone task record key (ADR 0010)",
		Args:  cobra.NoArgs,
	}
	var out string
	gen := &cobra.Command{
		Use:           "generate",
		Short:         "Write a new random task key to a new 0600 file",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if out == "" {
				return errors.New("--out is required")
			}
			k, err := taskrecords.GenerateKeyFile(out)
			if err != nil {
				return fmt.Errorf("generate task key: %w", err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s (mode 0600), key id %s\nset standalone.task_key_file to this path; back the file up outside data_dir\n", out, k.ID())
			return err
		},
	}
	gen.Flags().StringVar(&out, "out", "", "path of the new key file (must not exist)")
	group.AddCommand(gen)
	return group
}
