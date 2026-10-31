// input: 注入的 newAppMetaStoreForRun 工厂与可缩短的 metaStoreOpenBudget
// output: 启动期元数据库打开的瞬时错误退避重试、永久错误快速失败、预算耗尽仍报错、ctx 取消即停的证据
// pos: soak 中 worker 冷启动撞上元数据闪断窗口会进程自杀的回归；
//
//	打开重试让重启在原位等过瞬时窗口，永久错误不得被吞。
//
// note: 变更本文件需同步 internal/app/README.md。
package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"binlog_server/internal/config"
)

func TestOpenMetaStoreForRun_RetriesTransientThenSucceeds(t *testing.T) {
	store := &fakeAppMetaStore{}
	var calls atomic.Int32
	restoreFactory := newAppMetaStoreForRun
	newAppMetaStoreForRun = func(config.Config) (appMetaStore, error) {
		if calls.Add(1) < 3 {
			return nil, errors.New("meta.tls: invalid connection")
		}
		return store, nil
	}
	defer func() { newAppMetaStoreForRun = restoreFactory }()

	got, err := openMetaStoreForRun(context.Background(), config.Config{})
	if err != nil || got != store {
		t.Fatalf("openMetaStoreForRun = (%v, %v), want the recovered store", got, err)
	}
	if calls.Load() != 3 {
		t.Fatalf("factory calls = %d, want 3 (2 transient failures then success)", calls.Load())
	}
}

func TestOpenMetaStoreForRun_PermanentErrorFailsFast(t *testing.T) {
	var calls atomic.Int32
	restoreFactory := newAppMetaStoreForRun
	newAppMetaStoreForRun = func(config.Config) (appMetaStore, error) {
		calls.Add(1)
		return nil, errors.New("Error 1045 (28000): Access denied for user")
	}
	defer func() { newAppMetaStoreForRun = restoreFactory }()

	got, err := openMetaStoreForRun(context.Background(), config.Config{})
	if err == nil || got != nil {
		t.Fatalf("openMetaStoreForRun = (%v, %v), want the auth error", got, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("factory calls = %d, want 1 — permanent errors must not retry", calls.Load())
	}
}

func TestOpenMetaStoreForRun_TransientPastBudgetStillFails(t *testing.T) {
	var calls atomic.Int32
	restoreFactory := newAppMetaStoreForRun
	newAppMetaStoreForRun = func(config.Config) (appMetaStore, error) {
		calls.Add(1)
		return nil, errors.New("dial tcp 127.0.0.1:3306: connect: connection refused")
	}
	defer func() { newAppMetaStoreForRun = restoreFactory }()

	restoreBudget := metaStoreOpenBudget
	metaStoreOpenBudget = 50 * time.Millisecond
	defer func() { metaStoreOpenBudget = restoreBudget }()

	got, err := openMetaStoreForRun(context.Background(), config.Config{})
	if err == nil || got != nil {
		t.Fatalf("openMetaStoreForRun = (%v, %v), want the last transient error after budget", got, err)
	}
	if calls.Load() < 1 {
		t.Fatalf("factory calls = %d, want >=1", calls.Load())
	}
}

func TestOpenMetaStoreForRun_CancelStopsBackoff(t *testing.T) {
	var calls atomic.Int32
	restoreFactory := newAppMetaStoreForRun
	newAppMetaStoreForRun = func(config.Config) (appMetaStore, error) {
		calls.Add(1)
		return nil, errors.New("server has gone away")
	}
	defer func() { newAppMetaStoreForRun = restoreFactory }()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	got, err := openMetaStoreForRun(ctx, config.Config{})
	if err == nil || got != nil {
		t.Fatalf("openMetaStoreForRun = (%v, %v), want error on cancel", got, err)
	}
	if time.Since(start) > metaStoreOpenBudget {
		t.Fatal("cancelled context did not interrupt the retry backoff")
	}
}
