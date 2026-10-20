//go:build ignore

// Package main holds one task_leases row with SELECT ... FOR UPDATE.
// input: a MySQL DSN with no session variables, and a task id; --take waits for the row instead of holding it; --plain holds through database/sql with a long wait_timeout
// output: HOLDING on stdout after the holder locks the row through OpenClusterMeta, or through --plain; the process sleeps so it stays alive; --take exits 0 when the lock is acquired within --timeout
// pos: e2e helper for the ADR 0011 PR-2 SIGSTOP cell
// note: if this file changes, update this header and scripts/e2e/README.md.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"binlog_server/internal/meta"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

func main() {
	take := false
	plain := false
	timeout := 8 * time.Second
	args := os.Args[1:]
	usage := "usage: hold_for_update [--take] [--plain] [--timeout 8s] <dsn> <task-id>"
	for len(args) > 0 && len(args[0]) > 1 && args[0][0] == '-' {
		switch args[0] {
		case "--take":
			take = true
			args = args[1:]
		case "--plain":
			plain = true
			args = args[1:]
		case "--timeout":
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr, usage)
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
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
	}
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if plain {
		if err := plainHold(args[0], args[1]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
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

// plainHold locks task_leases on an unarmed connection and stays there.
// The noprocess cell SIGSTOPs this process. A long wait_timeout keeps the
// server from dropping the transaction before the second sweep.
func plainHold(dsn, taskID string) error {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SET SESSION wait_timeout = 600"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "SET SESSION innodb_lock_wait_timeout = 50"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "SET SESSION idle_transaction_timeout = 600"); err != nil {
		var me *mysqlDriver.MySQLError
		if !errors.As(err, &me) || me.Number != 1193 {
			return err
		}
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var owner string
	if err := tx.QueryRowContext(ctx, "SELECT owner_worker_id FROM task_leases WHERE task_id = ? FOR UPDATE", taskID).Scan(&owner); err != nil {
		return err
	}
	fmt.Println("HOLDING")
	_ = os.Stdout.Sync()
	for {
		time.Sleep(time.Hour)
	}
}
