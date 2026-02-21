package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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

const (
	workerIDFileName                  = ".worker-id"
	maxWorkerIDLength                 = 128
	defaultWorkerRegistrationTTL      = 15 * time.Second
	defaultWorkerRegistrationInterval = 5 * time.Second
)

var workerIDInvalidCharPattern = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

type App struct {
	cfg       config.Config
	readyCh   chan struct{}
	readyOnce sync.Once

	mu               sync.Mutex
	addr             string
	workerHealthAddr string
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

	var resolvedWorkerID string
	if workerEnabled && isClusterMode(a.cfg) {
		var err error
		resolvedWorkerID, err = resolveClusterWorkerID(a.cfg)
		if err != nil {
			return err
		}
	}

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

	if workerEnabled && isClusterMode(a.cfg) && mysqlStore != nil {
		sessionID, err := generateWorkerSessionID()
		if err != nil {
			return err
		}
		registrationTTL := effectiveWorkerRegistrationTTL(a.cfg)
		ok, err := mysqlStore.AcquireWorkerRegistration(ctx, resolvedWorkerID, sessionID, registrationTTL)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("worker_id is already in use: %s", resolvedWorkerID)
		}

		registrationCtx, registrationCancel := context.WithCancel(context.Background())
		defer registrationCancel()
		go startWorkerRegistrationRenewLoop(
			registrationCtx,
			mysqlStore,
			resolvedWorkerID,
			sessionID,
			effectiveWorkerRegistrationRenewInterval(a.cfg, registrationTTL),
			registrationTTL,
		)
		defer func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := mysqlStore.ReleaseWorkerRegistration(releaseCtx, resolvedWorkerID, sessionID); err != nil {
				log.Printf("worker registration release failed worker=%s session=%s err=%v", resolvedWorkerID, sessionID, err)
			}
		}()
	}

	opts, runnerOpts = applyClusterRuntimeOptions(
		a.cfg,
		resolvedWorkerID,
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
		opts = append(opts, tasks.WithFileUploader(uploader))
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
			go startWorkerHeartbeatLoop(
				ctx,
				sink,
				resolvedWorkerID,
				effectiveHostname(resolvedWorkerID),
				effectiveBinaryVersion(),
				5*time.Second,
			)
			go startWorkerClaimLoop(ctx, scheduler, 2*time.Second)
		}
	}

	if !controlPlaneEnabled {
		if workerEnabled && strings.TrimSpace(a.cfg.Cluster.WorkerHealthListenAddr) != "" {
			addr, err := startWorkerHealthServer(ctx, strings.TrimSpace(a.cfg.Cluster.WorkerHealthListenAddr))
			if err != nil {
				return err
			}
			a.setWorkerHealthAddr(addr)
		} else {
			a.setWorkerHealthAddr("")
		}
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

func (a *App) WorkerHealthAddr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.workerHealthAddr
}

func (a *App) setWorkerHealthAddr(addr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.workerHealthAddr = addr
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

func resolveClusterWorkerID(cfg config.Config) (string, error) {
	configured := strings.TrimSpace(cfg.Cluster.WorkerID)
	if configured != "" {
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

func readPersistedWorkerID(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
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

func writePersistedWorkerIDAtomically(path, workerID string) error {
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

func generateAutoWorkerID(hostname, ip string) (string, error) {
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

func sanitizeWorkerIDComponent(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	return workerIDInvalidCharPattern.ReplaceAllString(value, "-")
}

func sanitizeWorkerIPComponent(ip string) string {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return "noip"
	}
	return strings.ReplaceAll(ip, ".", "-")
}

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

func generateWorkerSessionID() (string, error) {
	return randomHex(8)
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

type workerRegistrationStore interface {
	AcquireWorkerRegistration(ctx context.Context, workerID, sessionID string, ttl time.Duration) (bool, error)
	RenewWorkerRegistration(ctx context.Context, workerID, sessionID string, ttl time.Duration) (bool, error)
	ReleaseWorkerRegistration(ctx context.Context, workerID, sessionID string) error
}

type startingTaskClaimer interface {
	ClaimStartingTasks() (int, error)
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

func startWorkerClaimLoop(ctx context.Context, claimer startingTaskClaimer, interval time.Duration) {
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
			claimed, err := claimer.ClaimStartingTasks()
			if err != nil {
				log.Printf("worker claim starting tasks failed err=%v", err)
				continue
			}
			if claimed > 0 {
				log.Printf("worker claimed starting tasks count=%d", claimed)
			}
		}
	}
}

func startWorkerRegistrationRenewLoop(ctx context.Context, store workerRegistrationStore, workerID, sessionID string, interval, ttl time.Duration) {
	if store == nil || workerID == "" || sessionID == "" {
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
			timeoutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			ok, err := store.RenewWorkerRegistration(timeoutCtx, workerID, sessionID, ttl)
			cancel()
			if err != nil {
				log.Printf("worker registration renew failed worker=%s session=%s err=%v", workerID, sessionID, err)
				continue
			}
			if !ok {
				log.Printf("worker registration renew lost ownership worker=%s session=%s", workerID, sessionID)
			}
		}
	}
}

func startWorkerHealthServer(ctx context.Context, listenAddr string) (string, error) {
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

	server := &http.Server{Handler: mux}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return "", err
	}

	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.Background())
	}()

	go func() {
		err := server.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("worker health server stopped err=%v", err)
		}
	}()

	return ln.Addr().String(), nil
}

func applyClusterRuntimeOptions(
	cfg config.Config,
	workerID string,
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
		tasks.WithClusterWorkerID(workerID),
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

func effectiveWorkerRegistrationTTL(cfg config.Config) time.Duration {
	ttl := time.Duration(cfg.Cluster.LeaseTTLSec) * time.Second
	if ttl <= 0 {
		return defaultWorkerRegistrationTTL
	}
	return ttl
}

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
