package config

import "os"

type Config struct {
	ListenAddr string
	DataDir    string
	MetaDSN    string

	UploadEndpoint  string
	UploadBucket    string
	UploadAccessKey string
	UploadSecretKey string
	UploadRegion    string
	UploadPrefix    string
	UploadUseSSL    bool
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
	cfg.UploadEndpoint = os.Getenv("BINLOG_SERVER_UPLOAD_ENDPOINT")
	cfg.UploadBucket = os.Getenv("BINLOG_SERVER_UPLOAD_BUCKET")
	cfg.UploadAccessKey = os.Getenv("BINLOG_SERVER_UPLOAD_ACCESS_KEY")
	cfg.UploadSecretKey = os.Getenv("BINLOG_SERVER_UPLOAD_SECRET_KEY")
	cfg.UploadRegion = os.Getenv("BINLOG_SERVER_UPLOAD_REGION")
	cfg.UploadPrefix = os.Getenv("BINLOG_SERVER_UPLOAD_PREFIX")
	cfg.UploadUseSSL = os.Getenv("BINLOG_SERVER_UPLOAD_USE_SSL") == "1" || os.Getenv("BINLOG_SERVER_UPLOAD_USE_SSL") == "true"
	return cfg, nil
}
