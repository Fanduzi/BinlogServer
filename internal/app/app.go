package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"

	"binlog_server/internal/api"
	"binlog_server/internal/config"
	"binlog_server/internal/tasks"
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
	scheduler := tasks.NewScheduler()
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
