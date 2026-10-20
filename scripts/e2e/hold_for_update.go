//go:build ignore

// Package main holds one task_leases row with SELECT ... FOR UPDATE.
// input: a MySQL DSN with no session variables, and a task id; --take waits for the row instead of holding it
// output: HOLDING on stdout after the holder locks the row through OpenClusterMeta; the process sleeps so it stays alive; --take exits 0 when the lock is acquired within --timeout
// pos: e2e helper for the ADR 0011 PR-2 SIGSTOP cell
// note: if this file changes, update this header and scripts/e2e/README.md.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"binlog_server/internal/meta"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	take := false
	timeout := 8 * time.Second
	args := os.Args[1:]
	for len(args) > 0 && len(args[0]) > 1 && args[0][0] == '-' {
		switch args[0] {
		case "--take":
			take = true
			args = args[1:]
		case "--timeout":
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr, "usage: hold_for_update [--take] [--timeout 8s] <dsn> <task-id>")
				os.Exit(2)
			}
			d, err := time.ParseDuration(args[1])
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			timeout = d
			args = args[2:]
		default:
			fmt.Fprintln(os.Stderr, "usage: hold_for_update [--take] [--timeout 8s] <dsn> <task-id>")
			os.Exit(2)
		}
	}
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: hold_for_update [--take] [--timeout 8s] <dsn> <task-id>")
		os.Exit(2)
	}
	if !take {
		// Lease TTL 4s arms a 2s idle kill through the production pool.
		// The DSN must not carry session timeouts; OpenClusterMeta sets them.
		store, err := meta.OpenClusterMeta(args[0], 15*time.Second, "", 4*time.Second, "off")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer store.Close()
		if err := store.HoldLeaseForUpdate(args[1], func() {
			fmt.Println("HOLDING")
			_ = os.Stdout.Sync()
		}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	db, err := sql.Open("mysql", args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = tx.Rollback() }()
	var owner string
	if err := tx.QueryRowContext(ctx, "SELECT owner_worker_id FROM task_leases WHERE task_id = ? FOR UPDATE", args[1]).Scan(&owner); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
