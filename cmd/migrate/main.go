// Package main provides module-level functionality for main.
// input: migration files, META_DSN/env flags, an optional --config whose meta.tls matches the server, golang-migrate driver dependencies
// output: schema migration commands (up/down/version/force/goto) execution results; a DSN rewritten with meta.tls when --config is set; the pool is opened in this process so that registered tls config is the one migrate uses; a dial error is prefixed meta.tls the same way server startup prefixes it
// pos: database schema migration CLI for operational change management
// note: if this file changes, update this header and module README.md.
package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"binlog_server/internal/config"
	"binlog_server/internal/mysqltls"

	"github.com/golang-migrate/migrate/v4"
	mysqldriver "github.com/golang-migrate/migrate/v4/database/mysql"
	"github.com/golang-migrate/migrate/v4/source/file"
	"github.com/spf13/cobra"

	_ "github.com/go-sql-driver/mysql"
)

type migrateOptions struct {
	dsn       string
	config    string
	path      string
	env       string
	allowDown bool
}

func main() {
	if err := execute(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func execute(args []string) error {
	opts := migrateOptions{
		dsn:  strings.TrimSpace(os.Getenv("META_DSN")),
		path: "./migrations",
		env:  effectiveEnv(),
	}
	opts.allowDown = isTruthy(os.Getenv("ALLOW_DESTRUCTIVE_MIGRATE"))
	root := newRoot(&opts)
	root.SetArgs(args)
	root.SilenceErrors = true
	root.SilenceUsage = true
	return root.Execute()
}

func newRoot(opts *migrateOptions) *cobra.Command {
	root := &cobra.Command{
		Use:   "migrate",
		Short: "Run database schema migrations",
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Name() == "help" {
				return nil
			}
			dsn, err := resolveMigrateDSN(opts.config, opts.dsn)
			if err != nil {
				return err
			}
			opts.dsn = dsn
			if opts.path == "" {
				return errors.New("--path is required")
			}
			return nil
		},
	}
	root.PersistentFlags().StringVar(&opts.dsn, "dsn", opts.dsn, "database dsn, e.g. user:pass@tcp(127.0.0.1:3306)/db?parseTime=true")
	root.PersistentFlags().StringVar(&opts.config, "config", "", "process config; meta.tls from this file is applied to the DSN (same block as the server). --dsn and META_DSN override meta_dsn")
	root.PersistentFlags().StringVar(&opts.path, "path", opts.path, "migration files path")
	root.PersistentFlags().StringVar(&opts.env, "env", opts.env, "runtime env (dev/prod)")
	root.PersistentFlags().BoolVar(&opts.allowDown, "allow-destructive", opts.allowDown, "allow down/force in production")
	root.AddCommand(
		newUpCommand(opts),
		newDownCommand(opts),
		newVersionCommand(opts),
		newForceCommand(opts),
		newGotoCommand(opts),
	)
	return root
}

// metaDialErr prefixes a database dial the same way server startup does.
func metaDialErr(err error) error {
	return mysqltls.PrefixMeta(err)
}

func newUpCommand(opts *migrateOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Apply all up migrations",
		RunE: func(_ *cobra.Command, _ []string) error {
			m, err := newMigrator(opts.path, opts.dsn)
			if err != nil {
				return metaDialErr(err)
			}
			defer closeMigrator(m)
			if err := m.Up(); err != nil {
				if errors.Is(err, migrate.ErrNoChange) {
					fmt.Println("no change")
					return nil
				}
				return metaDialErr(err)
			}
			version, dirty, err := m.Version()
			if err != nil {
				if errors.Is(err, migrate.ErrNilVersion) {
					fmt.Println("no version")
					return nil
				}
				return metaDialErr(err)
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
				return metaDialErr(err)
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
				return metaDialErr(err)
			}
			version, dirty, err := m.Version()
			if err != nil {
				if errors.Is(err, migrate.ErrNilVersion) {
					fmt.Println("no version")
					return nil
				}
				return metaDialErr(err)
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
				return metaDialErr(err)
			}
			defer closeMigrator(m)
			version, dirty, err := m.Version()
			if err != nil {
				if errors.Is(err, migrate.ErrNilVersion) {
					fmt.Println("no version")
					return nil
				}
				return metaDialErr(err)
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
				return metaDialErr(err)
			}
			defer closeMigrator(m)
			if err := m.Force(version); err != nil {
				return metaDialErr(err)
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
				return metaDialErr(err)
			}
			defer closeMigrator(m)
			if err := m.Migrate(uint(version)); err != nil {
				if errors.Is(err, migrate.ErrNoChange) {
					fmt.Println("no change")
					return nil
				}
				return metaDialErr(err)
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
	src, err := (&file.File{}).Open(sourceURL)
	if err != nil {
		return nil, err
	}
	// Open the pool here. migrate's URL opener re-registers a custom tls=
	// name and then reads x-tls-ca, which drops the meta.tls config ApplyDSN
	// already registered.
	db, err := sql.Open("mysql", migrateSQLDSN(dsn))
	if err != nil {
		return nil, err
	}
	driver, err := mysqldriver.WithInstance(db, &mysqldriver.Config{})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return migrate.NewWithInstance("file", src, "mysql", driver)
}

func migrateSQLDSN(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	dsn = strings.TrimPrefix(dsn, "mysql://")
	if strings.Contains(dsn, "multiStatements=true") {
		return dsn
	}
	if strings.Contains(dsn, "?") {
		return dsn + "&multiStatements=true"
	}
	return dsn + "?multiStatements=true"
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

// resolveMigrateDSN applies meta.tls from --config. Without --config the DSN
// is unchanged, including a tls= the operator already set. --dsn and META_DSN
// win over the file's meta_dsn. The tls block always comes from the file.
func resolveMigrateDSN(configPath, dsn string) (string, error) {
	configPath = strings.TrimSpace(configPath)
	dsn = strings.TrimSpace(dsn)
	if configPath == "" {
		if dsn == "" {
			return "", errors.New("META_DSN is required (or pass --dsn or --config)")
		}
		return dsn, nil
	}
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return "", err
	}
	if dsn == "" {
		dsn = strings.TrimSpace(cfg.MetaDSN)
	}
	if dsn == "" {
		return "", errors.New("META_DSN is required (or pass --dsn or --config)")
	}
	return mysqltls.ApplyDSN(dsn, cfg.Meta.TLS, mysqltls.MetaTLSName)
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
