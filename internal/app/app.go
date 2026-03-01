// Package app provides module-level functionality for app.
// input: runtime config, scheduler/runner/meta store dependencies, process context
// output: application lifecycle control including startup, role wiring, and shutdown
// pos: application composition layer that wires modules into runnable service modes
// note: if this file changes, update this header and module README.md.
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
	"sync/atomic"
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
var errWorkerRegistrationOwnershipLost = errors.New("worker registration ownership lost")

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
	// runCtx 统一管理本次运行生命周期：
	// 上游取消或内部致命事件（如注册所有权丢失）都会触发全链路退出。
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	// 解析运行角色：是否对外提供 API（control-plane）以及是否执行任务（worker）。
	controlPlaneEnabled, workerEnabled := resolveRoleMode(a.cfg)

	// opts 供 scheduler 使用；runnerOpts 供数据面 runner 使用。
	opts := []tasks.Option{}
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

	// 先组装 scheduler，再根据 worker 开关决定是否挂载 runner。
	scheduler := tasks.NewScheduler(opts...)
	if workerEnabled {
		// 将 runner 进度回传给 scheduler，供 API/状态机读取。
		runnerOpts = append(runnerOpts, replication.WithProgressReporter(scheduler))
		runner := replication.NewMySQLRunner(a.cfg.DataDir, runnerOpts...)
		scheduler.SetRunner(runner)
	}
	// Restore 必须在对外服务前执行，保证 API 看到的是恢复后的稳定状态。
	if err := scheduler.Restore(context.Background()); err != nil {
		return err
	}
	if workerEnabled && isClusterMode(a.cfg) {
		// worker 重启后把可恢复任务重新拉起，清理遗留的中间态。
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
			// worker 在线状态通过心跳写入元数据表供 control-plane 观测。
			sink := workerHeartbeatSink(mysqlStore)
			go startWorkerHeartbeatLoop(
				runCtx,
				sink,
				resolvedWorkerID,
				effectiveHostname(resolvedWorkerID),
				effectiveBinaryVersion(),
				5*time.Second,
			)
			// 定期认领 STARTING 任务，驱动分发后的任务进入真实执行。
			go startWorkerClaimLoop(runCtx, scheduler, 2*time.Second)
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

	handler := api.NewServer(scheduler, api.WithAuth(api.AuthConfig{
		Enabled:        a.cfg.API.Auth.Enabled,
		Mode:           api.AuthMode(strings.ToLower(strings.TrimSpace(a.cfg.API.Auth.Mode))),
		BearerToken:    a.cfg.API.Auth.BearerToken,
		APIKey:         a.cfg.API.Auth.APIKey,
		APIKeyHeader:   a.cfg.API.Auth.APIKeyHeader,
		ProtectAPI:     a.cfg.API.Auth.ProtectAPI,
		ProtectMetrics: a.cfg.API.Auth.ProtectMetrics,
	}))
	server := buildHTTPServer(handler, a.cfg.HTTP.ControlPlane)

	ln, err := net.Listen("tcp", a.cfg.ListenAddr)
	if err != nil {
		return err
	}
	// 暴露实际绑定地址（ListenAddr 使用 :0 时尤其有用）。
	a.setAddr(ln.Addr().String())
	// readyCh 只在 listener 绑定成功后关闭。
	a.readyOnce.Do(func() { close(a.readyCh) })

	go func() {
		<-runCtx.Done()
		// graceful shutdown 允许 in-flight request 尽量完成。
		_ = server.Shutdown(context.Background())
	}()

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

type leaseVerifierFromStore struct {
	leaseStore *meta.LeaseStore
}

// VerifyLease 校验任务在元数据库中的 lease 归属是否仍有效。
func (v leaseVerifierFromStore) VerifyLease(ctx context.Context, task tasks.Task) (bool, error) {
	if v.leaseStore == nil {
		return false, nil
	}
	if task.ID == "" || task.OwnerWorkerID == "" || task.Epoch <= 0 {
		return false, nil
	}
	return v.leaseStore.VerifyOwnership(ctx, task.ID, task.OwnerWorkerID, task.Epoch)
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

type startingTaskClaimer interface {
	ClaimStartingTasks() (int, error)
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

// startWorkerClaimLoop 周期触发 STARTING 任务认领，驱动 worker 拉起待执行任务。
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
			// claim 是幂等轮询：失败重试，成功按返回数量记录。
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

	server := buildHTTPServer(mux, timeoutCfg)
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

// buildHTTPServer 根据配置构建带超时的 HTTP server，避免慢连接拖垮控制面。
func buildHTTPServer(handler http.Handler, timeoutCfg config.HTTPServerTimeoutConfig) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: time.Duration(timeoutCfg.ReadHeaderTimeoutSec) * time.Second,
		ReadTimeout:       time.Duration(timeoutCfg.ReadTimeoutSec) * time.Second,
		WriteTimeout:      time.Duration(timeoutCfg.WriteTimeoutSec) * time.Second,
		IdleTimeout:       time.Duration(timeoutCfg.IdleTimeoutSec) * time.Second,
	}
}

// applyClusterRuntimeOptions 在 cluster 模式下注入 lease 与 worker 标识相关运行时选项。
func applyClusterRuntimeOptions(
	cfg config.Config,
	workerID string,
	leaseManager tasks.LeaseManager,
	leaseVerifier replication.LeaseVerifier,
	opts []tasks.Option,
	runnerOpts []replication.RunnerOption,
) ([]tasks.Option, []replication.RunnerOption) {
	if !isClusterMode(cfg) || !isNonNilInterface(leaseManager) {
		// 非 cluster 或 lease manager 无效时，不注入任何 cluster 运行时能力。
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
		// runner 在关键写路径再次验租，避免失租后继续产出数据。
		runnerOpts = append(runnerOpts, replication.WithLeaseVerifier(leaseVerifier))
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

// resumeClusterWorkerTasks 在 worker 启动时重置并恢复可运行任务，避免残留状态阻塞调度。
func resumeClusterWorkerTasks(scheduler clusterTaskResumer) resumeClusterStats {
	var stats resumeClusterStats
	for _, task := range scheduler.ListTasks() {
		switch task.State {
		case tasks.StateRunning, tasks.StateStarting, tasks.StateRetryBackoff, tasks.StateLeaseDegraded:
			// 这些状态都需要 worker 继续推进；通过 stop->start 让其在当前实例重新走 lease 获取流程。
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
