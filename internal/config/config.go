package config

import "os"

type Config struct {
	ListenAddr string
	DataDir    string
	MetaDSN    string
}

func LoadConfig(path string) (Config, error) {
	_ = path

	cfg := Config{
		ListenAddr: ":8080",
		DataDir:    "./data",
	}
	if v := os.Getenv("BINLOG_SERVER_LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("BINLOG_SERVER_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv("BINLOG_SERVER_META_DSN"); v != "" {
		cfg.MetaDSN = v
	}
	return cfg, nil
}
