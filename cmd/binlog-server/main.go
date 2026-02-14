package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"binlog_server/internal/app"
	"binlog_server/internal/config"
)

func main() {
	cfg, err := config.LoadConfig("")
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := app.New(cfg).Run(ctx); err != nil {
		log.Fatalf("run app: %v", err)
	}
}
