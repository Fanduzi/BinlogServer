// input: migration files, META_DSN/env flags, golang-migrate driver dependencies
// output: schema migration commands (up/down/version/force/goto) execution results
// pos: database schema migration CLI for operational change management
// note: if this file changes, update this header and module AGENTS.md.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/mysql"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/spf13/cobra"
)

type migrateOptions struct {
	dsn       string
	path      string
	env       string
	allowDown bool
}

func main() {
	opts := migrateOptions{
		dsn:  strings.TrimSpace(os.Getenv("META_DSN")),
		path: "./migrations",
		env:  effectiveEnv(),
	}
	opts.allowDown = isTruthy(os.Getenv("ALLOW_DESTRUCTIVE_MIGRATE"))

	root := &cobra.Command{
		Use:   "migrate",
		Short: "Run database schema migrations",
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Name() == "help" {
				return nil
			}
			if opts.dsn == "" {
				return errors.New("META_DSN is required (or pass --dsn)")
			}
			if opts.path == "" {
				return errors.New("--path is required")
			}
			return nil
		},
	}
	root.PersistentFlags().StringVar(&opts.dsn, "dsn", opts.dsn, "database dsn, e.g. user:pass@tcp(127.0.0.1:3306)/db?parseTime=true")
	root.PersistentFlags().StringVar(&opts.path, "path", opts.path, "migration files path")
	root.PersistentFlags().StringVar(&opts.env, "env", opts.env, "runtime env (dev/prod)")
	root.PersistentFlags().BoolVar(&opts.allowDown, "allow-destructive", opts.allowDown, "allow down/force in production")

	root.AddCommand(
		newUpCommand(&opts),
		newDownCommand(&opts),
		newVersionCommand(&opts),
		newForceCommand(&opts),
		newGotoCommand(&opts),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newUpCommand(opts *migrateOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Apply all up migrations",
		RunE: func(_ *cobra.Command, _ []string) error {
			m, err := newMigrator(opts.path, opts.dsn)
			if err != nil {
				return err
			}
			defer closeMigrator(m)
			if err := m.Up(); err != nil {
				if errors.Is(err, migrate.ErrNoChange) {
					fmt.Println("no change")
					return nil
				}
				return err
			}
			version, dirty, err := m.Version()
			if err != nil {
				if errors.Is(err, migrate.ErrNilVersion) {
					fmt.Println("no version")
					return nil
				}
				return err
			}
			fmt.Printf("version=%d dirty=%v\n", version, dirty)
			return nil
		},
	}
}

func newDownCommand(opts *migrateOptions) *cobra.Command {
	var steps int
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Rollback migrations",
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := guardDestructive(opts); err != nil {
				return err
			}
			m, err := newMigrator(opts.path, opts.dsn)
			if err != nil {
				return err
			}
			defer closeMigrator(m)
			if steps <= 0 {
				return errors.New("--steps must be > 0")
			}
			if err := m.Steps(-steps); err != nil {
				if errors.Is(err, migrate.ErrNoChange) {
					fmt.Println("no change")
					return nil
				}
				return err
			}
			version, dirty, err := m.Version()
			if err != nil {
				if errors.Is(err, migrate.ErrNilVersion) {
					fmt.Println("no version")
					return nil
				}
				return err
			}
			fmt.Printf("version=%d dirty=%v\n", version, dirty)
			return nil
		},
	}
	cmd.Flags().IntVar(&steps, "steps", 1, "rollback steps")
	return cmd
}

func newVersionCommand(opts *migrateOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print current migration version",
		RunE: func(_ *cobra.Command, _ []string) error {
			m, err := newMigrator(opts.path, opts.dsn)
			if err != nil {
				return err
			}
			defer closeMigrator(m)
			version, dirty, err := m.Version()
			if err != nil {
				if errors.Is(err, migrate.ErrNilVersion) {
					fmt.Println("no version")
					return nil
				}
				return err
			}
			fmt.Printf("version=%d dirty=%v\n", version, dirty)
			return nil
		},
	}
}

func newForceCommand(opts *migrateOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "force <version>",
		Short: "Set migration version without running migrations",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := guardDestructive(opts); err != nil {
				return err
			}
			version, err := strconv.Atoi(args[0])
			if err != nil {
				return fmt.Errorf("invalid version %q: %w", args[0], err)
			}
			m, err := newMigrator(opts.path, opts.dsn)
			if err != nil {
				return err
			}
			defer closeMigrator(m)
			if err := m.Force(version); err != nil {
				return err
			}
			fmt.Printf("forced version=%d\n", version)
			return nil
		},
	}
}

func newGotoCommand(opts *migrateOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "goto <version>",
		Short: "Migrate to specific version",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			version, err := strconv.ParseUint(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid version %q: %w", args[0], err)
			}
			if version > uint64(^uint(0)) {
				return fmt.Errorf("version %d overflows uint on current platform", version)
			}
			m, err := newMigrator(opts.path, opts.dsn)
			if err != nil {
				return err
			}
			defer closeMigrator(m)
			if err := m.Migrate(uint(version)); err != nil {
				if errors.Is(err, migrate.ErrNoChange) {
					fmt.Println("no change")
					return nil
				}
				return err
			}
			fmt.Printf("version=%d dirty=false\n", version)
			return nil
		},
	}
}

func newMigrator(migrationsPath, dsn string) (*migrate.Migrate, error) {
	absPath, err := filepath.Abs(migrationsPath)
	if err != nil {
		return nil, err
	}
	sourceURL := "file://" + filepath.ToSlash(absPath)
	return migrate.New(sourceURL, normalizeDSN(dsn))
}

func closeMigrator(m *migrate.Migrate) {
	if m == nil {
		return
	}
	srcErr, dbErr := m.Close()
	if srcErr != nil || dbErr != nil {
		fmt.Fprintf(os.Stderr, "close migrate resources warning: source=%v database=%v\n", srcErr, dbErr)
	}
}

func normalizeDSN(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if strings.HasPrefix(dsn, "mysql://") {
		return dsn
	}
	return "mysql://" + dsn
}

func guardDestructive(opts *migrateOptions) error {
	env := strings.ToLower(strings.TrimSpace(opts.env))
	if env != "prod" && env != "production" {
		return nil
	}
	if opts.allowDown {
		return nil
	}
	return fmt.Errorf("refusing destructive migrate command in production: env=%s (set --allow-destructive or ALLOW_DESTRUCTIVE_MIGRATE=1 to override)", opts.env)
}

func effectiveEnv() string {
	if env := strings.TrimSpace(os.Getenv("MIGRATE_ENV")); env != "" {
		return env
	}
	if env := strings.TrimSpace(os.Getenv("ENV")); env != "" {
		return env
	}
	return "dev"
}

func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}
