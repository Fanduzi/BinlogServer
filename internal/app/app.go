package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"

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
	var runnerOpts []replication.RunnerOption
	opts := []tasks.Option{}

	var mysqlStore *meta.MySQLTaskStore
	if a.cfg.MetaDSN != "" {
		var err error
		mysqlStore, err = meta.NewMySQLTaskStore(a.cfg.MetaDSN)
		if err != nil {
			return err
		}
		defer mysqlStore.Close()
		opts = append(opts, tasks.WithStore(mysqlStore))
		opts = append(opts, tasks.WithCheckpointReader(mysqlStore))
		runnerOpts = append(runnerOpts, replication.WithCheckpointStore(mysqlStore))
	}

	if a.cfg.UploadEndpoint != "" && a.cfg.UploadBucket != "" && a.cfg.UploadAccessKey != "" && a.cfg.UploadSecretKey != "" {
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

	runner := replication.NewMySQLRunner(a.cfg.DataDir, runnerOpts...)
	opts = append(opts, tasks.WithRunner(runner))

	scheduler := tasks.NewScheduler(opts...)
	if err := scheduler.Restore(context.Background()); err != nil {
		return err
	}

	handler := api.NewServer(scheduler)
	server := &http.Server{Handler: handler}

	ln, err := net.Listen("tcp", a.cfg.ListenAddr)
	if err != nil {
		return err
	}
	a.setAddr(ln.Addr().String())
	a.readyOnce.Do(func() { close(a.readyCh) })

	go func() {
		<-ctx.Done()
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
