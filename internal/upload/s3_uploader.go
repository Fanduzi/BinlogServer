package upload

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type S3Config struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string
	UseSSL    bool
}

type S3Uploader struct {
	client *minio.Client
	bucket string
}

func NewS3Uploader(cfg S3Config) (*S3Uploader, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("incomplete s3 config")
	}

	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, err
	}

	return &S3Uploader{
		client: client,
		bucket: cfg.Bucket,
	}, nil
}

func (u *S3Uploader) UploadFile(ctx context.Context, _ string, localPath, objectKey string) error {
	_, err := u.client.FPutObject(ctx, u.bucket, filepath.ToSlash(objectKey), localPath, minio.PutObjectOptions{})
	return err
}
