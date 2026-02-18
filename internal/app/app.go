package app

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"binlog_server/internal/api"
	"binlog_server/internal/config"
	"binlog_server/internal/meta"
	"binlog_server/internal/replication"
	"binlog_server/internal/tasks"
	"binlog_server/internal/upload"
)

type App struct {
	cfg       config.Config
	readyCh   chan struct{}
	readyOnce sync.Once

	mu   sync.Mutex
	addr string
}

func New(cfg config.Config) *App {
	return &App{
		cfg:     cfg,
		readyCh: make(chan struct{}),
	}
}

func (a *App) Run(ctx context.Context) error {
	controlPlaneEnabled, workerEnabled := resolveRoleMode(a.cfg)

	opts := []tasks.Option{}
	var runnerOpts []replication.RunnerOption

	var mysqlStore *meta.MySQLTaskStore
	var leaseStore *meta.LeaseStore
	if a.cfg.MetaDSN != "" {
		// 如果配置了 metadata DB，把它同时注入 scheduler（control plane）
		// 和 runner（data plane 的 checkpoint/file metadata）。
		var err error
		mysqlStore, err = meta.NewMySQLTaskStore(a.cfg.MetaDSN)
		if err != nil {
			return err
		}
		defer mysqlStore.Close()
		opts = append(opts, tasks.WithStore(mysqlStore))
		opts = append(opts, tasks.WithCheckpointReader(mysqlStore))
		opts = append(opts, tasks.WithEventStore(mysqlStore))
		opts = append(opts, tasks.WithFileStore(mysqlStore))
		runnerOpts = append(runnerOpts, replication.WithCheckpointStore(mysqlStore))
		runnerOpts = append(runnerOpts, replication.WithFileMetaStore(mysqlStore))
		leaseStore = meta.NewLeaseStoreFromTaskStore(mysqlStore)
	}

	opts, runnerOpts = applyClusterRuntimeOptions(
		a.cfg,
		leaseStore,
		leaseVerifierFromStore{leaseStore: leaseStore},
		opts,
		runnerOpts,
	)

	if a.cfg.UploadEndpoint != "" && a.cfg.UploadBucket != "" && a.cfg.UploadAccessKey != "" && a.cfg.UploadSecretKey != "" {
		// upload 是可选能力；启用后 runner 会上传 sealed binlog file。
		uploader, err := upload.NewS3Uploader(upload.S3Config{
			Endpoint:  a.cfg.UploadEndpoint,
			Bucket:    a.cfg.UploadBucket,
			AccessKey: a.cfg.UploadAccessKey,
			SecretKey: a.cfg.UploadSecretKey,
			Region:    a.cfg.UploadRegion,
			UseSSL:    a.cfg.UploadUseSSL,
		})
		if err != nil {
			return err
		}
		runnerOpts = append(runnerOpts, replication.WithUploader(uploader, a.cfg.UploadPrefix))
	}

	scheduler := tasks.NewScheduler(opts...)
	if workerEnabled {
		runnerOpts = append(runnerOpts, replication.WithProgressReporter(scheduler))
		runner := replication.NewMySQLRunner(a.cfg.DataDir, runnerOpts...)
		scheduler.SetRunner(runner)
	}
	// Restore 必须在对外服务前执行，保证 API 看到的是恢复后的稳定状态。
	if err := scheduler.Restore(context.Background()); err != nil {
		return err
	}
	if workerEnabled && isClusterMode(a.cfg) {
		stats := resumeClusterWorkerTasks(scheduler)
		if stats.StopErrors > 0 || stats.StartErrors > 0 {
			log.Printf(
				"cluster resume completed with errors considered=%d resumed=%d stop_errors=%d start_errors=%d",
				stats.Considered,
				stats.Resumed,
				stats.StopErrors,
				stats.StartErrors,
			)
		}
	}
	if workerEnabled && isClusterMode(a.cfg) {
		if mysqlStore != nil {
			sink := workerHeartbeatSink(mysqlStore)
			workerID := effectiveClusterWorkerID(a.cfg)
			go startWorkerHeartbeatLoop(
				ctx,
				sink,
				workerID,
				effectiveHostname(workerID),
				effectiveBinaryVersion(),
				5*time.Second,
			)
		}
	}

	if !controlPlaneEnabled {
		a.setAddr("")
		a.readyOnce.Do(func() { close(a.readyCh) })
		<-ctx.Done()
		return nil
	}

	handler := api.NewServer(scheduler)
	server := &http.Server{Handler: handler}

	ln, err := net.Listen("tcp", a.cfg.ListenAddr)
	if err != nil {
		return err
	}
	// 暴露实际绑定地址（ListenAddr 使用 :0 时尤其有用）。
	a.setAddr(ln.Addr().String())
	// readyCh 只在 listener 绑定成功后关闭。
	a.readyOnce.Do(func() { close(a.readyCh) })

	go func() {
		<-ctx.Done()
		// graceful shutdown 允许 in-flight request 尽量完成。
		_ = server.Shutdown(context.Background())
	}()

	err = server.Serve(ln)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (a *App) Ready() <-chan struct{} {
	return a.readyCh
}

func (a *App) Addr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.addr
}

func (a *App) setAddr(addr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.addr = addr
}

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

type leaseVerifierFromStore struct {
	leaseStore *meta.LeaseStore
}

func (v leaseVerifierFromStore) VerifyLease(ctx context.Context, task tasks.Task) (bool, error) {
	if v.leaseStore == nil {
		return false, nil
	}
	if task.ID == "" || task.OwnerWorkerID == "" || task.Epoch <= 0 {
		return false, nil
	}
	return v.leaseStore.VerifyOwnership(ctx, task.ID, task.OwnerWorkerID, task.Epoch)
}

func isClusterMode(cfg config.Config) bool {
	return strings.ToLower(strings.TrimSpace(cfg.Mode)) == "cluster"
}

func effectiveClusterWorkerID(cfg config.Config) string {
	id := strings.TrimSpace(cfg.Cluster.WorkerID)
	if id != "" {
		return id
	}
	host, err := os.Hostname()
	if err != nil {
		return "worker-default"
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return "worker-default"
	}
	return host
}

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

func startWorkerHeartbeatLoop(ctx context.Context, sink workerHeartbeatSink, workerID, host, version string, interval time.Duration) {
	if sink == nil || workerID == "" {
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
			log.Printf("worker heartbeat upsert failed worker=%s status=%s err=%v", workerID, status, err)
		}
	}

	report("ONLINE")
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			report("OFFLINE")
			return
		case <-ticker.C:
			report("ONLINE")
		}
	}
}

func applyClusterRuntimeOptions(
	cfg config.Config,
	leaseManager tasks.LeaseManager,
	leaseVerifier replication.LeaseVerifier,
	opts []tasks.Option,
	runnerOpts []replication.RunnerOption,
) ([]tasks.Option, []replication.RunnerOption) {
	if !isClusterMode(cfg) || !isNonNilInterface(leaseManager) {
		return opts, runnerOpts
	}

	opts = append(opts,
		tasks.WithClusterLeaseManager(leaseManager),
		tasks.WithClusterWorkerID(effectiveClusterWorkerID(cfg)),
		tasks.WithClusterLease(
			time.Duration(cfg.Cluster.LeaseTTLSec)*time.Second,
			time.Duration(cfg.Cluster.LeaseRenewIntervalSec)*time.Second,
			time.Duration(cfg.Cluster.LeaseGraceSec)*time.Second,
		),
	)
	if leaseVerifier != nil {
		runnerOpts = append(runnerOpts, replication.WithLeaseVerifier(leaseVerifier))
	}
	return opts, runnerOpts
}

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

type resumeClusterStats struct {
	Considered  int
	Resumed     int
	StopErrors  int
	StartErrors int
}

type clusterTaskResumer interface {
	ListTasks() []tasks.Task
	StopTask(id string) error
	StartTask(id string) error
}

func resumeClusterWorkerTasks(scheduler clusterTaskResumer) resumeClusterStats {
	var stats resumeClusterStats
	for _, task := range scheduler.ListTasks() {
		switch task.State {
		case tasks.StateRunning, tasks.StateStarting, tasks.StateRetryBackoff, tasks.StateLeaseDegraded:
			stats.Considered++
			if err := scheduler.StopTask(task.ID); err != nil {
				stats.StopErrors++
				log.Printf("cluster resume stop failed task=%s err=%v", task.ID, err)
				continue
			}
			if err := scheduler.StartTask(task.ID); err != nil {
				stats.StartErrors++
				log.Printf("cluster resume start failed task=%s err=%v", task.ID, err)
				continue
			}
			stats.Resumed++
		}
	}
	return stats
}
