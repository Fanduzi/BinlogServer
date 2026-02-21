package upload

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config 是 S3/兼容对象存储上传配置。
type S3Config struct {
	// Endpoint 是对象存储访问地址（支持 S3 兼容端点）。
	Endpoint  string
	// Bucket 是目标桶名。
	Bucket    string
	// AccessKey/SecretKey 是访问凭证。
	AccessKey string
	SecretKey string
	// Region 是区域标识（部分厂商可选）。
	Region    string
	// UseSSL 控制是否使用 HTTPS。
	UseSSL    bool
}

// S3Uploader 是基于 minio-go 的 S3 兼容上传实现。
type S3Uploader struct {
	client *minio.Client
	bucket string
}

// NewS3Uploader 校验配置并创建上传客户端。
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

// UploadFile 上传本地文件到指定 object key。
func (u *S3Uploader) UploadFile(ctx context.Context, _ string, localPath, objectKey string) error {
	_, err := u.client.FPutObject(ctx, u.bucket, filepath.ToSlash(objectKey), localPath, minio.PutObjectOptions{})
	return err
}
