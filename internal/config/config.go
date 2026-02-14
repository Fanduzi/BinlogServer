package config

import "os"

type Config struct {
	ListenAddr string
}

func LoadConfig(path string) (Config, error) {
	_ = path

	cfg := Config{
		ListenAddr: ":8080",
	}
	if v := os.Getenv("BINLOG_SERVER_LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	}
	return cfg, nil
}
