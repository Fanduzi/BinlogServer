//go:build ignore

// Package main holds one task_leases row with SELECT ... FOR UPDATE.
// input: a MySQL DSN and a task id; --take waits for the row instead of holding it
// output: HOLDING on stdout after the lock, then the process sleeps so it stays alive; --take exits 0 when the lock is acquired within --timeout
// pos: e2e helper for the ADR 0011 PR-2 SIGSTOP cell
// note: if this file changes, update this header and scripts/e2e/README.md.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

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
	db, err := sql.Open("mysql", args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	if take {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
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
	if take {
		os.Exit(0)
	}
	fmt.Println("HOLDING")
	_ = os.Stdout.Sync()
	// An empty select is a deadlock: the process exits and the server
	// drops the session because the client went away. Sleep keeps the
	// process, and the lock, alive until the server timeout or SIGKILL.
	for {
		time.Sleep(time.Hour)
	}
}
