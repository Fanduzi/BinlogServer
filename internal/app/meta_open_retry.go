// input: run context, runtime config, injected newAppMetaStoreForRun factory, metaStoreOpenBudget, an opened appMetaStore
// output: openMetaStoreForRun — startup meta-pool open that retries transient MySQL errors inside a bounded total budget so a restart during a metadata blip waits in place instead of exiting into a supervisor crash loop; permanent DSN/auth/schema errors still fail fast; ctx cancel interrupts the backoff — plus metaStoreRunOptions, the scheduler/runner option wiring for that store and its cluster lease runtime
// pos: application composition helpers used by App.Run when wiring the metadata store
// note: if this file changes, update this header and module README.md.
package app

import (
	"context"
	"log"
	"time"

	"binlog_server/internal/config"
	"binlog_server/internal/meta"
	"binlog_server/internal/replication"
	"binlog_server/internal/tasks"
)

// metaStoreOpenBudget 是启动期打开元数据库的总预算：重启撞上元数据闪断时
// 进程应原地等过这个窗口而不是直接退出（退出只会让 supervisor 拉起后再撞死）。
var metaStoreOpenBudget = 90 * time.Second

// openMetaStoreForRun 在元数据瞬时错误上退避重试，直到预算耗尽或出现永久错误。
// DSN/认证/schema 类错误立即返回；预算耗尽仍返回最后一个瞬时错误，让进程照旧退出。
func openMetaStoreForRun(ctx context.Context, cfg config.Config) (appMetaStore, error) {
	deadline := time.Now().Add(metaStoreOpenBudget)
	attempt := 0
	for {
		store, err := newAppMetaStoreForRun(cfg)
		if err == nil || !meta.IsTransientMySQLError(err) {
			return store, err
		}
		attempt++
		if !time.Now().Before(deadline) {
			return nil, err
		}
		wait := time.Duration(attempt) * time.Second
		if wait > 5*time.Second {
			wait = 5 * time.Second
		}
		log.Printf("meta store open failed transiently; retrying attempt=%d err=%v", attempt, err)
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(wait):
		}
	}
}

// metaStoreRunOptions 把打开的 meta 池同时注入调度器（任务/检查点/事件/文件/finding
// 存储）与 runner（checkpoint/file metadata），集群模式下再挂租约运行时。
func metaStoreRunOptions(mysqlStore appMetaStore, cluster bool, opts []tasks.Option, runnerOpts []replication.RunnerOption) ([]tasks.Option, []replication.RunnerOption, tasks.LeaseManager) {
	opts = append(opts, tasks.WithStore(mysqlStore))
	opts = append(opts, tasks.WithCheckpointReader(mysqlStore))
	opts = append(opts, tasks.WithEventStore(mysqlStore))
	opts = append(opts, tasks.WithFileStore(mysqlStore))
	if findingStore, ok := mysqlStore.(tasks.FindingStore); ok {
		opts = append(opts, tasks.WithFindingStore(findingStore))
	}
	runnerOpts = append(runnerOpts, replication.WithCheckpointStore(mysqlStore))
	runnerOpts = append(runnerOpts, replication.WithFileMetaStore(mysqlStore))
	var leaseManager tasks.LeaseManager
	if cluster {
		leaseManager = newClusterLeaseRuntimeForRun(mysqlStore)
		if column, ok := mysqlStore.(interface{ PendingDumpColumn() bool }); ok && !column.PendingDumpColumn() {
			log.Printf("%s", tasks.ClusterDumpFenceSchemaMessage)
		}
	}
	return opts, runnerOpts, leaseManager
}
