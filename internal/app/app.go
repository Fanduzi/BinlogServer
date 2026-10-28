// Package app provides module-level functionality for app.
// input: runtime config, PRODUCTION environment flag, control-plane listen_addr, persisted task state, resolved cluster worker id, scheduler/runner/meta store dependencies, process context
// output: role-aware application lifecycle control with production and non-loopback control-plane auth checks, a PRODUCTION refuse when EncryptionKey is empty, metadata/source isolation, data_dir wired for on-disk file listing, a desired_run reconcile before Restore, ClaimRunnableTasks on start and claim ticks, worker startup removal of a crashed .takeover-* temp in each task directory before tasks are claimed, standalone MemoryLease, LeaseManager as seal verifier, the upload client wired as the retention object deleter and as the reader for a sealed UPLOADED segment, background retry of sealed UPLOAD_FAILED rows on the worker when upload is configured, background KILL of a Binlog Dump left open because Stop could not reach the source, the post-seal upload bounded by meta.timeout.upload_sec, a bounded ordered shutdown: cancel runCtx (every task runner derives from it), join runners (WaitRuns), join background goroutines including the bounded HTTP shutdown (idle keep-alives and connections that have not sent a request are closed first, including one that reaches StateNew or StateIdle after that close has started (the shutting flag is stored before the idle snapshot), so they do not consume the budget), retry an owed record save, then the finding sink's last flush (WaitFindingSink); nothing this run started writes data_dir after Run returns, and shutdown Cluster mode requires the dump fence and logs when migration 000004 is missing.; the always-on ADR 0009 P2-3 health evaluator and rehasher; the phase 3 verifier on the background group after Restore (standalone always, cluster skipped until ADR 0011); a startup HEAD of the object store (2s) that logs at warn level, names the bucket and endpoint without secrets, and records /api/health detail without refusing startup; a TLS transport failure in that probe degrades health, other probe failures leave status ok (storage can be down; local capture still runs; a blackholed endpoint delays listen by up to that 2s), and one cluster evaluation after Restore so a task-record clear is stored before the API listens; in cluster mode one tasks.LeaseClock is shared by the scheduler (WithLeaseClock) and the runner (WithWriteFence) for the data-plane write fence (ADR 0009 P2-6b, #417); the scheduler is also the runner's WithChainGate so rotate and the health evaluator share HoldChain (#441); startVerifier passes upload.bucket into the verifier so an incomplete object pass names that bucket; cluster meta uses OpenClusterMeta with the session-kill bound; the meta DSN is rewritten with meta.tls before the pool opens and a bare dial error is prefixed meta.tls (ADR 0012); upload.tls.ca_file is the object-storage trust store when set; WithClusterMode is set when mode is cluster so coverage and restore-plan return 501
// note P2-6.1 (§28): health.stale_after_sec (0 = 5×sweep, then the ADR 0011 floor max(2×sweep+lock_wait, min(S, RTO))) is EvaluatorConfig.StaleAfter
// note P2-6.2 (§30): health.node_gone_after_sec (0 = max(3×sweep, lease_ttl+2×sweep)) is EvaluatorConfig.NodeGoneAfter
// note ADR 0011: the scheduler gets cluster.storage, the refusal cap, and the meta cluster gate when the store implements it
// note F2: a non-empty notify.webhook_url is the evaluator's HealthNotifier; an empty URL leaves it nil
// pos: application composition layer that wires modules into runnable service modes
// note: if this file changes, update this header and module README.md.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"binlog_server/internal/api"
	"binlog_server/internal/config"
	"binlog_server/internal/healthstore"
	"binlog_server/internal/meta"
	"binlog_server/internal/mysqltls"
	"binlog_server/internal/notify"
	"binlog_server/internal/ownership"
	"binlog_server/internal/replication"
	"binlog_server/internal/taskrecords"
	"binlog_server/internal/tasks"
	"binlog_server/internal/upload"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"go.uber.org/zap"
)

// S3Uploader is the process object client. Renaming HeadObject or OpenObject
// fails this compile, instead of the Run type assertion silently dropping it.
var _ tasks.ObjectHeadOpener = (*upload.S3Uploader)(nil)

const (
	workerIDFileName                  = ".worker-id"
	maxWorkerIDLength                 = 128
	defaultWorkerRegistrationTTL      = 15 * time.Second
	defaultWorkerRegistrationInterval = 5 * time.Second
)

var workerIDInvalidCharPattern = regexp.MustCompile(`[^a-zA-Z0-9._-]`)
var errWorkerRegistrationOwnershipLost = errors.New("worker registration ownership lost")

type appMetaStore interface {
	tasks.TaskStore
	tasks.CheckpointReader
	tasks.EventStore
	tasks.FileStore
	replication.CheckpointStore
	replication.FileMetaStore
	workerHeartbeatSink
	workerRegistrationStore
	Close() error
}

var newAppMetaStoreForRun = func(cfg config.Config) (appMetaStore, error) {
	timeout := time.Duration(cfg.Meta.Timeout.WriteSec) * time.Second
	dsn, err := mysqltls.ApplyDSN(cfg.MetaDSN, cfg.Meta.TLS, mysqltls.MetaTLSName)
	if err != nil {
		return nil, err
	}
	var store appMetaStore
	if isClusterMode(cfg) && strings.TrimSpace(dsn) != "" {
		store, err = meta.OpenClusterMeta(dsn, timeout, cfg.EncryptionKey, time.Duration(cfg.Cluster.LeaseTTLSec)*time.Second, cfg.Cluster.SideKill)
	} else {
		store, err = meta.NewMySQLTaskStoreWithSchemaTimeout(dsn, timeout, cfg.EncryptionKey)
	}
	if err != nil {
		return nil, prefixMetaDial(err)
	}
	return store, nil
}

// prefixMetaDial names the meta pool when the driver reports a bare x509 error.
func prefixMetaDial(err error) error {
	return mysqltls.PrefixMeta(err)
}

var newClusterLeaseRuntimeForRun = func(store appMetaStore) tasks.LeaseManager {
	mysqlStore, ok := store.(*meta.MySQLTaskStore)
	if !ok {
		return nil
	}
	return meta.NewLeaseStoreFromTaskStore(mysqlStore)
}

var newRunnerForRun = func(cfg config.Config, opts ...replication.RunnerOption) tasks.Runner {
	return replication.NewMySQLRunner(cfg.DataDir, opts...)
}

// uploadClient is the object client the process starts with. S3Uploader uploads,
// deletes, and reads. Takeover reads a sealed object through this same client.
type uploadClient interface {
	tasks.FileUploader
	DeleteObject(context.Context, string) error
	OpenObject(context.Context, string) (io.ReadCloser, int64, error)
}

var newUploadClientForRun = func(cfg upload.S3Config) (uploadClient, error) {
	return upload.NewS3Uploader(cfg)
}

// s3ConfigFrom is the object-storage client the process starts with.
func s3ConfigFrom(cfg config.Config) upload.S3Config {
	return upload.S3Config{
		Endpoint:  cfg.UploadEndpoint,
		Bucket:    cfg.UploadBucket,
		AccessKey: cfg.UploadAccessKey,
		SecretKey: cfg.UploadSecretKey,
		Region:    cfg.UploadRegion,
		UseSSL:    cfg.UploadUseSSL,
		CAFile:    cfg.UploadCAFile,
	}
}

type App struct {
	cfg       config.Config
	readyCh   chan struct{}
	readyOnce sync.Once

	mu               sync.Mutex
	addr             string
	workerHealthAddr string
}

// New 创建应用实例并初始化就绪通知通道。
func New(cfg config.Config) *App {
	return &App{
		cfg:     cfg,
		readyCh: make(chan struct{}),
	}
}

// Run 启动应用运行时：按 mode/role 组装 scheduler 与 runner，
// 在 cluster 场景维护 worker 注册、心跳与任务认领，并在 control-plane 模式对外提供 HTTP API。
func (a *App) Run(ctx context.Context) error {
	// 先解析角色，worker-only 不暴露 control-plane API，不能套用 API 鉴权启动约束。
	controlPlaneEnabled, workerEnabled := resolveRoleMode(a.cfg)
	production, err := productionMode(os.Getenv("PRODUCTION"))
	if err != nil {
		return err
	}
	if err := requireProductionEncryptionKey(a.cfg.EncryptionKey, production); err != nil {
		return err
	}
	if controlPlaneEnabled {
		if err := validateControlPlaneAuth(a.cfg.ListenAddr, a.cfg.API.Auth, production); err != nil {
			return err
		}
	}
	if controlPlaneEnabled && !a.cfg.API.Auth.Enabled {
		log.Printf("\x1b[33m\x1b[1m⚠️  SECURITY WARNING: API authentication is DISABLED\x1b[0m")
		log.Printf("\x1b[33m   For production, set api.auth.enabled=true and configure your auth method.\x1b[0m")
		log.Printf("\x1b[33m   See docs/security.md for details.\x1b[0m")
	}

	// runCtx 统一管理本次运行生命周期：
	// 上游取消或内部致命事件（如注册所有权丢失）都会触发全链路退出。
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	// bg counts the goroutines this run starts that may write data_dir or
	// the metadata store; shutdown joins them (bounded).
	var bg sync.WaitGroup
	spawn := func(f func()) {
		bg.Add(1)
		go func() {
			defer bg.Done()
			f()
		}()
	}

	tracerProvider, shutdownTracing, err := initTracing(a.cfg.Tracing)
	if err != nil {
		return err
	}
	defer func() {
		_ = shutdownTracing(context.Background())
	}()
	meta.ConfigureTracing(a.cfg.Tracing.Enabled && a.cfg.Tracing.Exporter != "disabled", tracerProvider)
	defer meta.ConfigureTracing(false, nil)

	// opts 供 scheduler 使用；runnerOpts 供数据面 runner 使用。
	opts := []tasks.Option{}
	if isClusterMode(a.cfg) {
		// Coverage and restore-plan need a single owner for the chain. Until
		// ADR 0011 PR-4, a cluster process answers 501 instead of a partial directory.
		opts = append(opts, tasks.WithClusterMode())
	}
	var runnerOpts []replication.RunnerOption
	// 用于把“注册续约失败导致的退出”上报给调用方。
	var registrationOwnershipLost atomic.Bool

	var resolvedWorkerID string
	if workerEnabled && isClusterMode(a.cfg) {
		// cluster worker 需要稳定 worker_id；若未显式配置则从本地持久化恢复/生成。
		var err error
		resolvedWorkerID, err = resolveClusterWorkerID(a.cfg)
		if err != nil {
			return err
		}
	}

	var mysqlStore appMetaStore
	var leaseManager tasks.LeaseManager
	if a.cfg.MetaDSN != "" {
		metaHost, metaPort, comparable := metadataSourceEndpoint(a.cfg.MetaDSN)
		if comparable {
			opts = append(opts, tasks.WithMetadataSourceEndpoint(metaHost, metaPort))
		}
		// 如果配置了 metadata DB，把它同时注入 scheduler（control plane）
		// 和 runner（data plane 的 checkpoint/file metadata）。
		mysqlStore, err = newAppMetaStoreForRun(a.cfg)
		if err != nil {
			return err
		}
		defer mysqlStore.Close()
		opts = append(opts, tasks.WithStore(mysqlStore))
		opts = append(opts, tasks.WithCheckpointReader(mysqlStore))
		opts = append(opts, tasks.WithEventStore(mysqlStore))
		opts = append(opts, tasks.WithFileStore(mysqlStore))
		if findingStore, ok := mysqlStore.(tasks.FindingStore); ok {
			opts = append(opts, tasks.WithFindingStore(findingStore))
		}
		runnerOpts = append(runnerOpts, replication.WithCheckpointStore(mysqlStore))
		runnerOpts = append(runnerOpts, replication.WithFileMetaStore(mysqlStore))
		if isClusterMode(a.cfg) {
			leaseManager = newClusterLeaseRuntimeForRun(mysqlStore)
			if column, ok := mysqlStore.(interface{ PendingDumpColumn() bool }); ok && !column.PendingDumpColumn() {
				log.Printf("%s", tasks.ClusterDumpFenceSchemaMessage)
			}
		}
	}
	if workerEnabled && !isClusterMode(a.cfg) {
		leaseManager = tasks.NewMemoryLease()
		if resolvedWorkerID == "" {
			resolvedWorkerID = "standalone"
		}
	}

	opts = append(opts, tasks.WithInternalCallTimeouts(tasks.InternalCallTimeouts{
		Read:   time.Duration(a.cfg.Meta.Timeout.ReadSec) * time.Second,
		Write:  time.Duration(a.cfg.Meta.Timeout.WriteSec) * time.Second,
		Lease:  time.Duration(a.cfg.Meta.Timeout.LeaseSec) * time.Second,
		Upload: time.Duration(a.cfg.Meta.Timeout.UploadSec) * time.Second,
	}))
	opts = append(opts, clusterClaimOptsForRun(a.cfg, mysqlStore)...)

	if workerEnabled && isClusterMode(a.cfg) && mysqlStore != nil {
		// 注册语义：同一 worker_id 在同一时刻只能被一个活跃会话占用。
		// sessionID 用于标识“本次进程启动”的身份（同 worker_id 的重启会拿到新 sessionID）。
		// 后续续约/释放都依赖该 sessionID 做持有者校验，避免其他实例误续约或误释放。
		sessionID, err := generateWorkerSessionID()
		if err != nil {
			return err
		}
		// registrationTTL 是注册租期：
		// - 正常运行时由后台循环持续续约；
		// - 进程异常退出或失联时不再续约，超过 TTL 后注册自动过期，其他实例可接管。
		registrationTTL := effectiveWorkerRegistrationTTL(a.cfg)
		// 先尝试获取 worker_id 注册所有权：
		// - true: 当前进程成为该 worker_id 的唯一活跃拥有者；
		// - false: 说明已有其他活跃实例占用，当前进程必须拒绝启动 worker 执行面。
		ok, err := mysqlStore.AcquireWorkerRegistration(ctx, resolvedWorkerID, sessionID, registrationTTL)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("worker_id is already in use: %s", resolvedWorkerID)
		}

		// 后台续约注册；一旦丢失所有权，触发 runCancel 让整个进程收敛退出。
		registrationCtx, registrationCancel := context.WithCancel(context.Background())
		defer registrationCancel()
		go startWorkerRegistrationRenewLoop(
			registrationCtx,
			mysqlStore,
			resolvedWorkerID,
			sessionID,
			effectiveWorkerRegistrationRenewInterval(a.cfg, registrationTTL),
			registrationTTL,
			func() {
				// 续约返回 !ok 表示失租：当前 session 已不再拥有该 worker_id。
				// 通过 runCancel 触发 Run 全链路收敛退出，避免失租后继续执行任务。
				registrationOwnershipLost.Store(true)
				runCancel()
			},
		)
		defer func() {
			// 退出时尽力释放注册，减少 stale registration 的持续时间。
			releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := mysqlStore.ReleaseWorkerRegistration(releaseCtx, resolvedWorkerID, sessionID); err != nil {
				log.Printf("worker registration release failed worker=%s session=%s err=%v", resolvedWorkerID, sessionID, err)
			}
		}()
		go func() {
			// runCtx 结束后主动停止续约协程，避免 goroutine 泄露。
			<-runCtx.Done()
			registrationCancel()
		}()
	}

	// cluster 相关注入集中在一个函数，保持主流程可读性。
	opts, runnerOpts = applyClusterRuntimeOptions(
		a.cfg,
		resolvedWorkerID,
		leaseManager,
		workerEnabled,
		opts,
		runnerOpts,
	)

	uploadConfigured := a.cfg.UploadEndpoint != "" && a.cfg.UploadBucket != "" && a.cfg.UploadAccessKey != "" && a.cfg.UploadSecretKey != ""
	var verifyObjects tasks.ObjectHeadOpener
	var probeDetail string
	if uploadConfigured {
		// upload 是可选能力；启用后 runner 会上传 sealed binlog file。
		uploader, err := newUploadClientForRun(s3ConfigFrom(a.cfg))
		if err != nil {
			return err
		}
		if opener, ok := uploader.(tasks.ObjectHeadOpener); ok {
			verifyObjects = opener
			probeDetail = objectProbeDetail(runCtx, uploader, opener, a.cfg.UploadBucket, a.cfg.UploadEndpoint, a.cfg.UploadAccessKey, a.cfg.UploadSecretKey)
		} else {
			probeDetail = objectProbeDetail(runCtx, uploader, nil, a.cfg.UploadBucket, a.cfg.UploadEndpoint, a.cfg.UploadAccessKey, a.cfg.UploadSecretKey)
		}
		opts = append(opts, tasks.WithFileUploader(uploader))
		// WithUploader is the one post-seal upload caller (ADR 0002). It writes
		// through the runner's file metadata store and reports the upload
		// outcome to the finding sink (ADR 0009 S8/S9).
		runnerOpts = append(runnerOpts, replication.WithUploader(uploader, a.cfg.UploadPrefix), replication.WithObjectDeleter(uploader), replication.WithUploadTimeout(time.Duration(a.cfg.Meta.Timeout.UploadSec)*time.Second))
	}

	// 先组装 scheduler，再根据 worker 开关决定是否挂载 runner。
	// data_dir 与 runner 落盘目录相同，files API 在 binlog_files 为空时扫描这里。
	opts = append(opts, tasks.WithDataDir(a.cfg.DataDir))
	if a.cfg.MetaDSN == "" && strings.TrimSpace(a.cfg.DataDir) != "" {
		// ADR 0010 §Single writer: one standalone process per data_dir, before
		// anything in it is read or written.
		unlock, err := lockDataDir(a.cfg.DataDir)
		if err != nil {
			log.Printf("startup refused: %v", err)
			return err
		}
		defer unlock()
		// Standalone: findings live next to the backups and survive a restart.
		opts = append(opts, tasks.WithFindingStore(tasks.NewFileFindingStore(filepath.Join(a.cfg.DataDir, tasks.FindingFileName))))
		if a.cfg.Standalone.TaskRecords != "off" {
			opts = append(opts, standaloneTaskRecords(a.cfg))
		} else {
			log.Printf("task records: standalone.task_records=off; tasks are kept in memory only and leftover directories need adopt after a restart")
		}
	} else if a.cfg.MetaDSN != "" && strings.TrimSpace(a.cfg.DataDir) != "" {
		if _, err := os.Stat(filepath.Join(a.cfg.DataDir, taskrecords.FileName)); err == nil {
			log.Printf("task records: %s exists but meta_dsn is set; metadata MySQL is the task authority and the file is not used", filepath.Join(a.cfg.DataDir, taskrecords.FileName))
		}
	}
	scheduler := tasks.NewScheduler(opts...)
	if probeDetail != "" {
		scheduler.SetVerifyStorageDetail(probeDetail)
	}
	// Task runners belong to this run: runCtx ending cancels them, and the
	// shutdown below joins them before Run returns.
	scheduler.SetRunContext(runCtx)
	// Seed the open findings in both modes: a restarted process shows them in
	// /metrics, and schema 6 (no task_findings) turns the store off up front.
	if err := scheduler.LoadOpenFindings(runCtx); err != nil {
		if a.cfg.MetaDSN == "" {
			return fmt.Errorf("load findings: %w", err)
		}
		log.Printf("load open findings from metadata store: %v", err)
	}
	// ADR 0009: one writer for findings; retries failed writes and task rows.
	// The sink outlives runCtx: runners report their last findings while
	// they shut down, and the sink's final flush must come after that.
	sinkCtx, sinkCancel := context.WithCancel(context.Background())
	scheduler.StartFindingSink(sinkCtx, 5*time.Second)
	defer func() {
		// Bounded, ordered shutdown. Nothing this run started may touch
		// data_dir after Run returns (the data_dir lock and the metadata
		// store are released by earlier defers, which run after this one).
		runCancel()
		if !scheduler.WaitRuns(shutdownRunnersTimeout) {
			log.Printf("shutdown: task runners did not stop in %s", shutdownRunnersTimeout)
		}
		if !waitGroupTimeout(&bg, shutdownBackgroundTimeout) {
			log.Printf("shutdown: background workers did not stop in %s", shutdownBackgroundTimeout)
		}
		// A record save that failed is owed; try once more from memory.
		scheduler.RetryRecordSave()
		sinkCancel()
		if !scheduler.WaitFindingSink(shutdownFindingsTimeout) {
			log.Printf("shutdown: findings writer did not finish its last flush in %s", shutdownFindingsTimeout)
		}
	}()
	if err := startHealthEvaluator(runCtx, a.cfg, scheduler, mysqlStore, resolvedWorkerID, spawn); err != nil {
		return err
	}
	if workerEnabled {
		// 将 runner 进度回传给 scheduler，供 API/状态机读取。
		runnerOpts = append(runnerOpts, replication.WithProgressReporter(scheduler))
		runnerOpts = append(runnerOpts, replication.WithChainGate(scheduler))
		runner := newRunnerForRun(a.cfg, runnerOpts...)
		if cleaner, ok := runner.(interface{ RemoveStaleTakeoverTemps() error }); ok {
			if err := cleaner.RemoveStaleTakeoverTemps(); err != nil {
				return fmt.Errorf("remove stale takeover temps: %w", err)
			}
		}
		scheduler.SetRunner(runner)
	}
	// Restore 必须在对外服务前执行，保证 API 看到的是恢复后的稳定状态。
	// 先把 v0.5.44/v0.5.45 留下的 desired_run=STOP 按状态改回来，再把行载入内存。
	restoreCtx, cancelRestore := context.WithTimeout(runCtx, time.Duration(a.cfg.Meta.Timeout.ReadSec)*time.Second)
	defer cancelRestore()
	if _, err := scheduler.ReconcileLegacyDesiredRun(restoreCtx); err != nil {
		return fmt.Errorf("reconcile desired_run: %w", err)
	}
	if err := scheduler.Restore(restoreCtx); err != nil {
		return err
	}
	// SignalSuccess during Restore only marks the evaluator dirty. Store the
	// close before listen: the sweep debounce is 1s, and a restart otherwise
	// keeps serving the previous process's TASK_RECORDS_FAILED.
	if err := scheduler.EvaluateClusterNow(runCtx); err != nil {
		log.Printf("health evaluate %s after restore: %v", tasks.ClusterFindingTask, err)
	}
	startVerifier(runCtx, a.cfg, scheduler, verifyObjects, spawn)
	if workerEnabled {
		claimed, err := scheduler.ClaimRunnableTasks()
		if err != nil {
			log.Printf("task claim on start failed err=%v claimed=%d", err, claimed)
		} else if claimed > 0 {
			log.Printf("task claim on start claimed=%d", claimed)
		}
	}
	if workerEnabled && a.cfg.MetaDSN == "" {
		// ADR 0010: Start every task the standalone record says should run.
		if n := scheduler.ResumeRecordedTasks(); n > 0 {
			log.Printf("task records: resumed %d task(s) after restart", n)
		}
	}
	if a.cfg.MetaDSN == "" && a.cfg.Standalone.TaskRecords != "off" {
		// A save that failed (TASK_RECORDS_FAILED/write) is retried from memory.
		spawn(func() { runRecordSaveRetry(runCtx, scheduler, 30*time.Second) })
	}
	if workerEnabled {
		spawn(func() { scheduler.RunDumpCleanupRetry(runCtx) })
	}
	if workerEnabled && uploadConfigured {
		// 封存文件在本进程磁盘上。后台补传复用 RetryFailedUploads，不新增上传实现。
		// control-plane-only 不跑：它没有这些本地文件。手动补传 API 仍在控制面。
		spawn(func() { scheduler.RunBackgroundUploadRetry(runCtx, 0) })
	}
	if workerEnabled && isClusterMode(a.cfg) {
		if mysqlStore != nil {
			// worker 在线状态通过心跳写入元数据表供 control-plane 观测。
			sink := workerHeartbeatSink(mysqlStore)
			spawn(func() {
				startWorkerHeartbeatLoop(
					runCtx,
					sink,
					resolvedWorkerID,
					effectiveHostname(resolvedWorkerID),
					effectiveBinaryVersion(),
					5*time.Second,
				)
			})
			// 定期认领 STARTING 与租约已过期任务，驱动分发后的任务进入真实执行。
			spawn(func() { startWorkerClaimLoop(runCtx, scheduler, 2*time.Second) })
		}
	}

	if !controlPlaneEnabled {
		// worker-only 模式不启动主 API，仅按需暴露健康检查端口。
		if workerEnabled && strings.TrimSpace(a.cfg.Cluster.WorkerHealthListenAddr) != "" {
			addr, err := startWorkerHealthServer(
				runCtx,
				strings.TrimSpace(a.cfg.Cluster.WorkerHealthListenAddr),
				a.cfg.HTTP.WorkerHealth,
			)
			if err != nil {
				return err
			}
			a.setWorkerHealthAddr(addr)
		} else {
			a.setWorkerHealthAddr("")
		}
		a.setAddr("")
		a.readyOnce.Do(func() { close(a.readyCh) })
		<-runCtx.Done()
		// 明确返回“所有权丢失”错误，便于上层区分退出原因。
		if registrationOwnershipLost.Load() {
			return errWorkerRegistrationOwnershipLost
		}
		return nil
	}

	apiOptions := []api.ServerOption{api.WithAuth(api.AuthConfig{
		Enabled:        a.cfg.API.Auth.Enabled,
		Mode:           api.AuthMode(strings.ToLower(strings.TrimSpace(a.cfg.API.Auth.Mode))),
		BearerToken:    a.cfg.API.Auth.BearerToken,
		APIKey:         a.cfg.API.Auth.APIKey,
		APIKeyHeader:   a.cfg.API.Auth.APIKeyHeader,
		ProtectAPI:     a.cfg.API.Auth.ProtectAPI,
		ProtectMetrics: a.cfg.API.Auth.ProtectMetrics,
	})}
	apiOptions = append(apiOptions, api.WithRateLimit(api.RateLimiterConfig{
		Enabled:           a.cfg.API.RateLimit.Enabled,
		RequestsPerSecond: a.cfg.API.RateLimit.RequestsPerSecond,
		Burst:             a.cfg.API.RateLimit.Burst,
	}))
	if a.cfg.Tracing.Enabled && a.cfg.Tracing.Exporter != "disabled" {
		apiOptions = append(apiOptions, api.WithTracing(api.TracingConfig{
			Enabled:        true,
			ServiceName:    a.cfg.Tracing.ServiceName,
			TracerProvider: tracerProvider,
		}))
	}
	handler := api.NewServer(scheduler, apiOptions...)
	server, closeIdle := buildHTTPServer(handler, a.cfg.HTTP.ControlPlane)

	ln, err := net.Listen("tcp", a.cfg.ListenAddr)
	if err != nil {
		return err
	}
	// 暴露实际绑定地址（ListenAddr 使用 :0 时尤其有用）。
	a.setAddr(ln.Addr().String())
	// readyCh 只在 listener 绑定成功后关闭。
	a.readyOnce.Do(func() { close(a.readyCh) })

	spawn(func() {
		<-runCtx.Done()
		// In-flight requests still finish inside the budget. Idle keep-alives
		// do not: closeIdle drops them before Shutdown waits.
		shutdownHTTP(server, closeIdle)
	})

	// Serve 返回 http.ErrServerClosed 表示预期关闭，不应视为错误。
	err = server.Serve(ln)
	if registrationOwnershipLost.Load() {
		return errWorkerRegistrationOwnershipLost
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func productionMode(raw string) (bool, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return false, nil
	}
	production, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("invalid PRODUCTION value %q: %w", value, err)
	}
	return production, nil
}

func validateControlPlaneAuth(listenAddr string, auth config.APIAuthConfig, production bool) error {
	if err := validateProductionAuth(auth, production); err != nil {
		return err
	}
	if isLoopbackListenAddr(listenAddr) {
		return nil
	}
	return validateNonLoopbackAuth(auth)
}

// standaloneTaskRecords opens {data_dir}/.tasks.enc with the key from
// BINLOG_SERVER_TASK_KEY or standalone.task_key_file (ADR 0010). A missing
// or unusable key is handed to the scheduler, which raises
// TASK_RECORDS_FAILED; it does not stop the process.
func standaloneTaskRecords(cfg config.Config) tasks.Option {
	key, keyErr := taskrecords.LoadKey(os.Getenv(taskrecords.KeyEnv), cfg.Standalone.TaskKeyFile)
	if keyErr != nil {
		key = nil
		log.Printf("task records: %v", keyErr)
	}
	var prev *taskrecords.Key
	if os.Getenv(taskrecords.PreviousKeyEnv) != "" || cfg.Standalone.TaskKeyPreviousFile != "" {
		p, err := taskrecords.LoadKey(os.Getenv(taskrecords.PreviousKeyEnv), cfg.Standalone.TaskKeyPreviousFile)
		if err != nil {
			log.Printf("task records: previous key not usable, rotation from it is off: %v", err)
		} else {
			prev = p
		}
	}
	// The key file is the recommended source; an env key is not left in the
	// process environment once read (#393 QA P3).
	_ = os.Unsetenv(taskrecords.KeyEnv)
	_ = os.Unsetenv(taskrecords.PreviousKeyEnv)
	if key != nil {
		log.Printf("task records: on; key id %s, previous key id %q", key.ID(), prev.ID())
	}
	return tasks.WithTaskRecords(taskrecords.Open(cfg.DataDir, key, prev), keyErr)
}

// clusterClaimOptsForRun maps the cluster claim configuration to scheduler
// options; a nil or incapable store leaves the gate unset.
func clusterClaimOptsForRun(cfg config.Config, mysqlStore appMetaStore) []tasks.Option {
	opts := []tasks.Option{tasks.WithStorageMode(cfg.Cluster.Storage)}
	if cfg.Health.RefusalRetryCapSec > 0 {
		opts = append(opts, tasks.WithRefusalCap(time.Duration(cfg.Health.RefusalRetryCapSec)*time.Second))
	}
	if gate, ok := mysqlStore.(tasks.ClusterGate); ok {
		opts = append(opts, tasks.WithClusterGate(gate))
	}
	return opts
}

// Shutdown bounds. Their sum stays under shutdownHardLimit in cmd, so a
// SIGTERM ends the process cleanly well before an orchestrator's kill.
const (
	shutdownHTTPTimeout       = 5 * time.Second
	shutdownRunnersTimeout    = 10 * time.Second
	shutdownBackgroundTimeout = 5 * time.Second
	shutdownFindingsTimeout   = 5 * time.Second
)

// waitGroupTimeout waits for wg up to d and reports whether it finished.
func waitGroupTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

func runRecordSaveRetry(ctx context.Context, scheduler *tasks.Scheduler, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			scheduler.RetryRecordSave()
		}
	}
}

func requireProductionEncryptionKey(key string, production bool) error {
	if !production || strings.TrimSpace(key) != "" {
		return nil
	}
	return errors.New("--encryption-key is required in PRODUCTION mode")
}

func validateProductionAuth(auth config.APIAuthConfig, production bool) error {
	if !production {
		return nil
	}
	if !auth.Enabled {
		return errors.New("api.auth.enabled must be true in PRODUCTION mode (set PRODUCTION=false for development)")
	}
	if !auth.ProtectAPI || !auth.ProtectMetrics {
		return errors.New("api.auth.protect_api and api.auth.protect_metrics must be true in PRODUCTION mode")
	}
	return config.ValidateAPIAuthConfig(auth)
}

func validateNonLoopbackAuth(auth config.APIAuthConfig) error {
	if !auth.Enabled {
		return errors.New("api.auth.enabled must be true when listen_addr is not loopback")
	}
	if !auth.ProtectAPI || !auth.ProtectMetrics {
		return errors.New("api.auth.protect_api and api.auth.protect_metrics must be true when listen_addr is not loopback")
	}
	return config.ValidateAPIAuthConfig(auth)
}

func isLoopbackListenAddr(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	return tasks.IsLoopbackHost(host)
}

func metadataSourceEndpoint(dsn string) (string, uint16, bool) {
	// Store construction remains the source of truth for DSN errors; this helper
	// only extracts TCP endpoints that can be compared with task sources.
	cfg, err := mysqlDriver.ParseDSN(dsn)
	if err != nil {
		return "", 0, false
	}
	if !strings.HasPrefix(cfg.Net, "tcp") {
		return "", 0, false
	}
	host, portText, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return "", 0, false
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return "", 0, false
	}
	return host, uint16(port), true
}

// Ready 返回应用就绪信号通道（listener 绑定成功后关闭）。
func (a *App) Ready() <-chan struct{} {
	return a.readyCh
}

// Addr 返回 control-plane HTTP 实际监听地址。
func (a *App) Addr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.addr
}

// setAddr 更新 control-plane HTTP 实际监听地址。
func (a *App) setAddr(addr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.addr = addr
}

// WorkerHealthAddr 返回 worker 健康检查监听地址（worker-only 模式可用）。
func (a *App) WorkerHealthAddr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.workerHealthAddr
}

// setWorkerHealthAddr 更新 worker 健康检查监听地址。
func (a *App) setWorkerHealthAddr(addr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.workerHealthAddr = addr
}

// resolveRoleMode 根据 mode/cluster.role 决定是否启用 control-plane 与 worker。
func resolveRoleMode(cfg config.Config) (controlPlaneEnabled bool, workerEnabled bool) {
	if strings.ToLower(strings.TrimSpace(cfg.Mode)) != "cluster" {
		return true, true
	}

	switch strings.ToLower(strings.TrimSpace(cfg.Cluster.Role)) {
	case "control-plane":
		return true, false
	case "worker":
		return false, true
	case "all-in-one", "":
		return true, true
	default:
		return true, true
	}
}

// isClusterMode 返回当前是否处于 cluster 模式。
func isClusterMode(cfg config.Config) bool {
	return strings.ToLower(strings.TrimSpace(cfg.Mode)) == "cluster"
}

// resolveClusterWorkerID 解析 worker_id：
// 优先使用配置值，否则读取/生成并持久化到 data_dir/.worker-id。
func resolveClusterWorkerID(cfg config.Config) (string, error) {
	configured := strings.TrimSpace(cfg.Cluster.WorkerID)
	if configured != "" {
		// 显式配置优先，但仍要守住长度约束（与存储层字段保持一致）。
		if len(configured) > maxWorkerIDLength {
			return "", fmt.Errorf("cluster.worker_id exceeds max length %d", maxWorkerIDLength)
		}
		return configured, nil
	}

	dataDir := strings.TrimSpace(cfg.DataDir)
	if dataDir == "" {
		dataDir = "."
	}
	idFile := filepath.Join(dataDir, workerIDFileName)
	if existing, err := readPersistedWorkerID(idFile); err != nil {
		return "", err
	} else if existing != "" {
		// 已存在持久化值时直接复用，保证重启后身份不变。
		return existing, nil
	}

	hostname, _ := os.Hostname()
	generated, err := generateAutoWorkerID(hostname, firstNonLoopbackIPv4())
	if err != nil {
		return "", err
	}
	if err := writePersistedWorkerIDAtomically(idFile, generated); err != nil {
		return "", err
	}
	// 写入后回读，避免并发写入时读取到不同内容。
	persisted, err := readPersistedWorkerID(idFile)
	if err != nil {
		return "", err
	}
	if persisted == "" {
		return "", errors.New("worker_id persist failed")
	}
	return persisted, nil
}

// readPersistedWorkerID 读取已持久化的 worker_id（不存在/空内容返回未命中）。
func readPersistedWorkerID(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 首次启动时文件不存在，按“未命中”处理。
			return "", nil
		}
		return "", err
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", nil
	}
	if len(value) > maxWorkerIDLength {
		return "", fmt.Errorf("persisted worker_id exceeds max length %d", maxWorkerIDLength)
	}
	return value, nil
}

// writePersistedWorkerIDAtomically 以原子替换方式写入 worker_id 文件。
func writePersistedWorkerIDAtomically(path, workerID string) error {
	// 通过 tmp + rename 原子替换，避免部分写入造成损坏文件。
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmpFile, err := os.CreateTemp(filepath.Dir(path), ".worker-id.tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmpFile.WriteString(workerID + "\n"); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// generateAutoWorkerID 基于主机信息生成默认 worker_id，并保证长度受限。
func generateAutoWorkerID(hostname, ip string) (string, error) {
	// worker_id 结构：wk-<host>-<ip>-<random>，兼顾可读性与冲突概率。
	suffix, err := randomHex(4)
	if err != nil {
		return "", err
	}
	hostPart := sanitizeWorkerIDComponent(hostname)
	if hostPart == "" {
		hostPart = "nohost"
	}
	ipPart := sanitizeWorkerIPComponent(ip)
	if ipPart == "" {
		ipPart = "noip"
	}
	id := fmt.Sprintf("wk-%s-%s-%s", hostPart, ipPart, suffix)
	if len(id) <= maxWorkerIDLength {
		return id, nil
	}
	// 超长时优先裁剪 host 片段，尽量保留 ip 与随机后缀的信息。
	fixedLen := len("wk--") + len(ipPart) + len(suffix)
	maxHostLen := maxWorkerIDLength - fixedLen
	if maxHostLen < 1 {
		maxHostLen = 1
	}
	if len(hostPart) > maxHostLen {
		hostPart = hostPart[:maxHostLen]
	}
	id = fmt.Sprintf("wk-%s-%s-%s", hostPart, ipPart, suffix)
	if len(id) > maxWorkerIDLength {
		id = id[:maxWorkerIDLength]
	}
	return id, nil
}

// sanitizeWorkerIDComponent 清理 worker_id 组件中的非法字符。
func sanitizeWorkerIDComponent(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	return workerIDInvalidCharPattern.ReplaceAllString(value, "-")
}

// sanitizeWorkerIPComponent 将 IP 组件转换为 worker_id 安全格式。
func sanitizeWorkerIPComponent(ip string) string {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return "noip"
	}
	return strings.ReplaceAll(ip, ".", "-")
}

// firstNonLoopbackIPv4 返回首个可用的非回环 IPv4 地址。
func firstNonLoopbackIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			default:
				continue
			}
			if ip == nil {
				continue
			}
			ipv4 := ip.To4()
			if ipv4 == nil || ipv4.IsLoopback() {
				continue
			}
			return ipv4.String()
		}
	}
	return ""
}

// randomHex 生成指定字节长度的随机十六进制字符串。
func randomHex(size int) (string, error) {
	if size <= 0 {
		size = 4
	}
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// generateWorkerSessionID 生成 worker 注册会话 ID，用于区分同一 worker_id 的不同实例。
func generateWorkerSessionID() (string, error) {
	return randomHex(8)
}

// effectiveHostname 返回用于 worker 心跳上报的主机名，失败时回退到 fallback。
func effectiveHostname(fallback string) string {
	host, err := os.Hostname()
	if err != nil {
		return fallback
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return fallback
	}
	return host
}

// effectiveBinaryVersion 返回当前二进制版本，无法读取时回退为 "devel"。
func effectiveBinaryVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "devel"
	}
	if info.Main.Version == "" {
		return "devel"
	}
	return info.Main.Version
}

type workerHeartbeatSink interface {
	UpsertWorkerHeartbeat(ctx context.Context, hb tasks.WorkerHeartbeat) error
}

type workerRegistrationStore interface {
	// AcquireWorkerRegistration 尝试获取 worker_id 注册所有权；成功返回 true。
	AcquireWorkerRegistration(ctx context.Context, workerID, sessionID string, ttl time.Duration) (bool, error)
	// RenewWorkerRegistration 仅在当前 session 仍持有所有权时续约；失去所有权返回 false。
	RenewWorkerRegistration(ctx context.Context, workerID, sessionID string, ttl time.Duration) (bool, error)
	// ReleaseWorkerRegistration 释放当前 session 的注册记录（幂等）。
	ReleaseWorkerRegistration(ctx context.Context, workerID, sessionID string) error
}

type workerTaskClaimer interface {
	ClaimRunnableTasks() (int, error)
}

// startWorkerHeartbeatLoop 周期上报 worker ONLINE/OFFLINE 心跳。
func startWorkerHeartbeatLoop(ctx context.Context, sink workerHeartbeatSink, workerID, host, version string, interval time.Duration) {
	if sink == nil || workerID == "" {
		// 缺少上报目标或 worker 身份时无法工作，直接退出。
		return
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}

	report := func(status string) {
		timeoutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		err := sink.UpsertWorkerHeartbeat(timeoutCtx, tasks.WorkerHeartbeat{
			WorkerID:   workerID,
			Host:       host,
			Version:    version,
			LastSeenAt: time.Now(),
			Status:     status,
		})
		if err != nil {
			// 心跳失败仅记录，不中断循环；避免短时抖动触发不必要停机。
			log.Printf("worker heartbeat upsert failed worker=%s status=%s err=%v", workerID, status, err)
		}
	}

	report("ONLINE")
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// 退出前补一条 OFFLINE，缩短控制面对离线的感知延迟。
			report("OFFLINE")
			return
		case <-ticker.C:
			report("ONLINE")
		}
	}
}

// startWorkerClaimLoop 周期触发 STARTING 与过期租约任务认领，驱动 worker 拉起待执行任务。
func startWorkerClaimLoop(ctx context.Context, claimer workerTaskClaimer, interval time.Duration) {
	if claimer == nil {
		return
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			claimed, err := claimer.ClaimRunnableTasks()
			if err != nil {
				log.Printf("worker claim runnable tasks failed err=%v", err)
			} else if claimed > 0 {
				log.Printf("worker claimed runnable tasks count=%d", claimed)
			}
		}
	}
}

// startWorkerRegistrationRenewLoop 周期续约 worker 注册，续约失效时触发 onOwnershipLost。
// 时序：启动抢占成功后进入本循环 -> 定时 Renew -> 明确 !ok 视为失租并上报给上层收敛退出。
func startWorkerRegistrationRenewLoop(
	ctx context.Context,
	store workerRegistrationStore,
	workerID, sessionID string,
	interval, ttl time.Duration,
	onOwnershipLost func(),
) {
	if store == nil || workerID == "" || sessionID == "" {
		// 参数不完整意味着无法续约，直接返回避免无意义循环。
		return
	}
	if interval <= 0 {
		interval = defaultWorkerRegistrationInterval
	}
	if ttl <= 0 {
		ttl = defaultWorkerRegistrationTTL
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// 单次续约调用设置短超时，避免某次 DB 卡顿把整个续约循环长期阻塞。
			// 超时/错误只会进入重试路径，不会直接判定失租。
			timeoutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			ok, err := store.RenewWorkerRegistration(timeoutCtx, workerID, sessionID, ttl)
			cancel()
			if err != nil {
				// 短暂错误先容忍并重试（避免把瞬时 DB 抖动误判为失租）；
				// 只有明确 !ok 才视为“当前 session 已不再持有 worker_id”。
				log.Printf("worker registration renew failed worker=%s session=%s err=%v", workerID, sessionID, err)
				continue
			}
			if !ok {
				log.Printf("worker registration renew lost ownership worker=%s session=%s", workerID, sessionID)
				if onOwnershipLost != nil {
					// 由上层回调统一决定如何收敛（当前实现为 cancel runCtx）。
					onOwnershipLost()
				}
				return
			}
		}
	}
}

// startWorkerHealthServer 启动 worker 专用健康检查端点（/healthz 与 /readyz）。
func startWorkerHealthServer(ctx context.Context, listenAddr string, timeoutCfg config.HTTPServerTimeoutConfig) (string, error) {
	// 该服务只做最小探活语义，不承载业务读写。
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	server, closeIdle := buildHTTPServer(mux, timeoutCfg)
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return "", err
	}

	go func() {
		<-ctx.Done()
		shutdownHTTP(server, closeIdle)
	}()

	go func() {
		err := server.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("worker health server stopped err=%v", err)
		}
	}()

	return ln.Addr().String(), nil
}

// buildHTTPServer 根据配置构建带超时的 HTTP server，避免慢连接拖垮控制面。
// closeIdle closes keep-alive connections that are idle or have not sent a
// request. Shutdown closes StateIdle, but it leaves a StateNew connection
// alone for 5s, which is this process's whole HTTP shutdown budget.
func buildHTTPServer(handler http.Handler, timeoutCfg config.HTTPServerTimeoutConfig) (*http.Server, func()) {
	track := &httpConnTrack{conns: map[net.Conn]http.ConnState{}}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: time.Duration(timeoutCfg.ReadHeaderTimeoutSec) * time.Second,
		ReadTimeout:       time.Duration(timeoutCfg.ReadTimeoutSec) * time.Second,
		WriteTimeout:      time.Duration(timeoutCfg.WriteTimeoutSec) * time.Second,
		IdleTimeout:       time.Duration(timeoutCfg.IdleTimeoutSec) * time.Second,
		ConnState:         track.observe,
	}
	return srv, track.closeIdle
}

// httpConnTrack is the set of connections a server has accepted.
// shutting is set for the process shutdown. A connection accepted after the
// idle snapshot, whose ConnState hook has not run yet, is still StateNew
// and would otherwise keep Shutdown busy for the whole HTTP budget.
type httpConnTrack struct {
	mu       sync.Mutex
	conns    map[net.Conn]http.ConnState
	shutting bool
}

func (t *httpConnTrack) observe(c net.Conn, state http.ConnState) {
	t.mu.Lock()
	if state == http.StateClosed || state == http.StateHijacked {
		delete(t.conns, c)
		t.mu.Unlock()
		return
	}
	t.conns[c] = state
	// StateActive stays open: an in-flight request has the shutdown budget.
	drop := t.shutting && (state == http.StateNew || state == http.StateIdle)
	t.mu.Unlock()
	if drop {
		_ = c.Close()
	}
}

// connTrackSnap, when a test sets it, runs after shutting is stored and
// before the idle snapshot. The lock is not held: the test can dial, and
// ConnState then sees shutting already true. Moving the assignment to
// after the snapshot makes the hook observe shutting still false.
var connTrackSnap func(shutting bool)

// closeIdle closes connections that are not serving a request, and arms the
// hook so a StateNew or StateIdle connection that arrives afterwards is
// closed too. shutting is stored before the snapshot. The caller must not
// hold t.mu: Close runs the ConnState hook, which takes t.mu.
func (t *httpConnTrack) closeIdle() {
	t.mu.Lock()
	t.shutting = true
	armed := t.shutting
	t.mu.Unlock()
	if connTrackSnap != nil {
		connTrackSnap(armed)
	}
	t.mu.Lock()
	var drop []net.Conn
	for c, state := range t.conns {
		if state == http.StateIdle || state == http.StateNew {
			drop = append(drop, c)
		}
	}
	t.mu.Unlock()
	for _, c := range drop {
		_ = c.Close()
	}
}

// shutdownHTTP turns keep-alives off, drops idle connections, then waits up
// to shutdownHTTPTimeout for requests that are still being served.
func shutdownHTTP(server *http.Server, closeIdle func()) {
	server.SetKeepAlivesEnabled(false)
	if closeIdle != nil {
		closeIdle()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownHTTPTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: http server: %v", err)
		_ = server.Close()
	}
}

// applyClusterRuntimeOptions 注入任务所有权：集群用 MySQL 租约，单机 worker 用 MemoryLease。
func applyClusterRuntimeOptions(
	cfg config.Config,
	workerID string,
	leaseManager tasks.LeaseManager,
	workerEnabled bool,
	opts []tasks.Option,
	runnerOpts []replication.RunnerOption,
) ([]tasks.Option, []replication.RunnerOption) {
	if !isNonNilInterface(leaseManager) {
		return opts, runnerOpts
	}

	opts = append(opts,
		tasks.WithClusterLeaseManager(leaseManager),
		tasks.WithClusterWorkerID(workerID),
	)
	if isClusterMode(cfg) {
		opts = append(opts, tasks.WithDumpFenceRequired())
		opts = append(opts, tasks.WithClusterLease(
			time.Duration(cfg.Cluster.LeaseTTLSec)*time.Second,
			time.Duration(cfg.Cluster.LeaseRenewIntervalSec)*time.Second,
			time.Duration(cfg.Cluster.LeaseGraceSec)*time.Second,
		))
	}
	if workerEnabled {
		runnerOpts = append(runnerOpts, replication.WithLeaseVerifier(leaseManager))
		if isClusterMode(cfg) {
			// ADR 0009 P2-6 (#417): one lease clock, written by the
			// scheduler's acquire/renew, read at every runner write boundary.
			clock := tasks.NewLeaseClock()
			opts = append(opts, tasks.WithLeaseClock(clock))
			runnerOpts = append(runnerOpts, replication.WithWriteFence(clock))
		}
	}
	return opts, runnerOpts
}

// effectiveWorkerRegistrationTTL 计算 worker 注册 TTL，配置非法时回退默认值。
// 这里复用 cluster lease_ttl_sec 作为 worker 注册租期，保持“任务租约”与“worker 注册”失效窗口一致。
func effectiveWorkerRegistrationTTL(cfg config.Config) time.Duration {
	ttl := time.Duration(cfg.Cluster.LeaseTTLSec) * time.Second
	if ttl <= 0 {
		return defaultWorkerRegistrationTTL
	}
	return ttl
}

// effectiveWorkerRegistrationRenewInterval 计算 worker 注册续约间隔，并保证小于 TTL。
// 当配置间隔 >= TTL 时自动收敛到 TTL/2，确保每个租期内至少有一次续约机会。
func effectiveWorkerRegistrationRenewInterval(cfg config.Config, ttl time.Duration) time.Duration {
	interval := time.Duration(cfg.Cluster.LeaseRenewIntervalSec) * time.Second
	if interval <= 0 {
		interval = defaultWorkerRegistrationInterval
	}
	if ttl > 0 && interval >= ttl {
		interval = ttl / 2
		if interval <= 0 {
			interval = time.Second
		}
	}
	return interval
}

// isNonNilInterface 判断 interface 包裹值是否为非 nil（处理 typed-nil 场景）。
func isNonNilInterface(v any) bool {
	if v == nil {
		return false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !rv.IsNil()
	default:
		return true
	}
}

// objectProbeTimeout bounds the startup HEAD so a dead endpoint does not hold listen.
const objectProbeTimeout = 2 * time.Second

// objectProbeDetail HEADs the bucket when the client can, otherwise a sentinel
// key. Empty means storage answered. A failure is logged at warn level and
// returned for /api/health detail. The detail names the bucket and endpoint
// and does not contain the access key or secret. Startup continues either
// way: local capture must run while storage is down. A TLS failure is the
// x509 text, not the probe deadline. A later successful upload clears that
// line; SetVerifyStorageDetail replaces it. Any other probe line stays until
// replaced. A bucket HEAD cannot see a policy that allows ListBucket but not
// GetObject; that shows up on a later object pass.
func objectProbeDetail(ctx context.Context, uploader any, opener tasks.ObjectHeadOpener, bucket, endpoint, accessKey, secret string) string {
	pctx, cancel := context.WithTimeout(ctx, objectProbeTimeout)
	defer cancel()
	var err error
	if p, ok := uploader.(interface{ ProbeStorage(context.Context) error }); ok {
		err = p.ProbeStorage(pctx)
	} else if opener == nil {
		zap.L().Warn("verifier: object client cannot HEAD and GET; object checks stay off")
		return "object client cannot HEAD and GET"
	} else {
		err = tasks.ProbeObjectHead(pctx, opener)
	}
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) && !upload.TLSTransportFailure(err.Error()) {
		err = fmt.Errorf("timed out after %s", objectProbeTimeout)
	}
	msg := upload.WithTLSHint(placeProbe(scrubSecrets(err.Error(), accessKey, secret), bucket, endpoint))
	zap.L().Warn("verifier: object storage probe", zap.String("detail", msg))
	return "object storage probe: " + msg
}

func scrubSecrets(msg, accessKey, secret string) string {
	if secret != "" {
		msg = strings.ReplaceAll(msg, secret, "[redacted]")
	}
	if accessKey != "" {
		msg = strings.ReplaceAll(msg, accessKey, "[redacted]")
	}
	return msg
}

func placeProbe(msg, bucket, endpoint string) string {
	if bucket != "" && !strings.Contains(msg, "bucket "+bucket) {
		msg += " bucket " + bucket
	}
	if endpoint != "" && !strings.Contains(msg, "endpoint "+endpoint) {
		msg += " endpoint " + endpoint
	}
	return msg
}

// startVerifier runs the phase 3 verifier on the background group. Standalone
// (no cluster lease) always verifies. Cluster skips every task until ADR 0011
// defines Allows; this process does not call the lease loop. The drill DSN
// is not logged and is not dialed.
func startVerifier(ctx context.Context, cfg config.Config, scheduler *tasks.Scheduler, objects tasks.ObjectHeadOpener, spawn func(func())) {
	vc := tasks.VerifierConfig{
		DataDir:            cfg.DataDir,
		Every:              time.Duration(cfg.Verify.EverySec) * time.Second,
		BudgetBytesPerSec:  cfg.Verify.BudgetBytesPerSec,
		RotationPeriods:    cfg.Verify.RotationPeriods,
		RecentSharePercent: cfg.Verify.RecentSharePercent,
		PassDeadline:       time.Duration(cfg.Verify.PassDeadlineSec) * time.Second,
		PassMaxBytes:       cfg.Verify.PassMaxBytes,
		ObjectRecent:       time.Duration(cfg.Verify.ObjectRecentSec) * time.Second,
		Objects:            tasks.NewObjectStore(objects),
		ObjectBucket:       cfg.UploadBucket,
	}
	if isClusterMode(cfg) {
		vc.Allows = func(context.Context, string) (bool, error) { return false, nil }
	}
	v := tasks.NewVerifier(scheduler, vc)
	scheduler.SetVerifier(v)
	log.Printf("verifier: every=%s budget_bytes_per_sec=%d", vc.Every, cfg.Verify.BudgetBytesPerSec)
	spawn(func() { v.Run(ctx) })
}

// startHealthEvaluator wires the ADR 0009 phase 2 evaluator and rehasher
// (P2-3); the evaluator always runs. Meta mode writes through
// the metadata store (migration 000009; without it the legacy finding path
// keeps the managed kinds); standalone writes {data_dir}/.health.json. The
// fence is the task lease in cluster mode, else the persisted node id.
func startHealthEvaluator(ctx context.Context, cfg config.Config, scheduler *tasks.Scheduler, metaStore appMetaStore, workerID string, spawn func(func())) error {
	ecfg := tasks.EvaluatorConfig{Sweep: time.Duration(cfg.Health.SweepIntervalSec) * time.Second, Cluster: isClusterMode(cfg),
		UnreachableAfter: time.Duration(cfg.Health.UnreachableAfterSec) * time.Second,
		SignalStaleAfter: time.Duration(cfg.Health.SignalStaleAfterSec) * time.Second,
		NoProgressAfter:  time.Duration(cfg.Health.NoProgressAfterSec) * time.Second,
		StaleAfter:       time.Duration(cfg.Health.StaleAfterSec) * time.Second,
		NodeGoneAfter:    time.Duration(cfg.Health.NodeGoneAfterSec) * time.Second,
		LeaseTTL:         time.Duration(cfg.Cluster.LeaseTTLSec) * time.Second,
		ClaimInterval:    2 * time.Second,
		StartToReady:     10 * time.Second,
	}
	killBound := ownership.SessionKillBound(time.Duration(cfg.Cluster.LeaseTTLSec) * time.Second)
	ecfg.LockWait = killBound
	ecfg.IdleKill = killBound
	if len(cfg.Health.UnreachableAfterTaskSec) > 0 {
		ecfg.UnreachableAfterTask = map[string]time.Duration{}
		for id, sec := range cfg.Health.UnreachableAfterTaskSec {
			ecfg.UnreachableAfterTask[id] = time.Duration(sec) * time.Second
		}
	}
	// design 0009 §21: upload grace, UPLOAD_LATE escalation, object listing.
	ecfg.UploadGrace = time.Duration(cfg.UploadGraceSec) * time.Second
	ecfg.UploadGraceTask = secondsMap(cfg.UploadGraceTaskSec)
	ecfg.UploadLateCriticalAfter = time.Duration(cfg.Health.UploadLateCriticalAfterSec) * time.Second
	ecfg.UploadLateCriticalAfterTask = secondsMap(cfg.Health.UploadLateCriticalAfterTaskSec)
	ecfg.ObjectListInterval = time.Duration(cfg.Health.ObjectListIntervalSec) * time.Second
	ecfg.ObjectListLimit = cfg.Health.ObjectListLimit
	var err error
	if ecfg.Instance, err = tasks.RandomID(); err != nil {
		return err
	}
	if metaStore != nil {
		hs, ok := metaStore.(healthstore.Store)
		if !ok {
			return fmt.Errorf("health evaluator: the metadata store has no health store")
		}
		ecfg.Store = hs
		if cs, ok := metaStore.(tasks.RehashCursorStore); ok {
			ecfg.RehashCursor = cs
		}
	} else {
		ecfg.Store = healthstore.NewFileStore(filepath.Join(cfg.DataDir, healthstore.HealthFileName))
	}
	if ecfg.NodeID, ecfg.ControlPlaneOnly, err = healthEvaluatorIdentity(cfg, workerID); err != nil {
		return err
	}
	if url := strings.TrimSpace(cfg.Notify.WebhookURL); url != "" {
		ecfg.Notify = notify.Webhook{
			URL:      url,
			Token:    cfg.Notify.WebhookToken,
			Timeout:  time.Duration(cfg.Notify.TimeoutSec) * time.Second,
			Attempts: cfg.Notify.Attempts,
			Backoff:  time.Duration(cfg.Notify.BackoffMs) * time.Millisecond,
		}
	}
	e := tasks.NewEvaluator(scheduler, ecfg)
	spawn(func() { e.Run(ctx) })
	// The source liveness clock keeps running while the task is out of
	// RUNNING: a silent source is probed (P2-4, design §18).
	spawn(func() { scheduler.RunSourceLiveness(ctx) })
	rehasher := tasks.NewRehasher(e, cfg.Health.RehashBytesPerSec)
	spawn(func() { rehasher.Run(ctx) })
	log.Printf("health evaluator on (sweep %s, rehash %d B/s, node %s, control-plane only %v)", ecfg.Sweep, cfg.Health.RehashBytesPerSec, ecfg.NodeID, ecfg.ControlPlaneOnly)
	return nil
}

// healthEvaluatorIdentity is the evaluator's node id on every role (design
// 0009 §24.13): a worker uses its worker id (the task lease owner); a
// control-plane-only node, which resolves no worker id, uses
// cluster.worker_id when configured, else the node id persisted in
// data_dir, so it can hold _health and proxy unowned tasks. controlPlaneOnly
// keeps such a node from ever evaluating as a task holder. Standalone uses
// the persisted node id.
func healthEvaluatorIdentity(cfg config.Config, workerID string) (nodeID string, controlPlaneOnly bool, err error) {
	if isClusterMode(cfg) {
		if workerID != "" {
			return workerID, false, nil
		}
		if configured := strings.TrimSpace(cfg.Cluster.WorkerID); configured != "" {
			return configured, true, nil
		}
		if nodeID, err = tasks.LoadOrCreateNodeID(cfg.DataDir); err != nil {
			return "", true, fmt.Errorf("health node id: %w", err)
		}
		return nodeID, true, nil
	}
	if nodeID, err = tasks.LoadOrCreateNodeID(cfg.DataDir); err != nil {
		return "", false, fmt.Errorf("health node id: %w", err)
	}
	return nodeID, false, nil
}

// secondsMap turns a per-task seconds map into durations (nil when empty).
func secondsMap(m map[string]int) map[string]time.Duration {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]time.Duration, len(m))
	for id, sec := range m {
		out[id] = time.Duration(sec) * time.Second
	}
	return out
}
