// Package upload provides module-level functionality for upload.
// input: local binlog files, object store credentials/config, upload retry context, and an existing object key
// output: sealed-file upload, and a reader plus the object size at open for one stored key
// pos: outbound storage adapter for sealed binlog upload and for reading an already uploaded object
// note: if this file changes, update this header and module README.md.
package upload

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config 是 S3/兼容对象存储上传配置。
type S3Config struct {
	// Endpoint 是对象存储访问地址（支持 S3 兼容端点）。
	Endpoint string
	// Bucket 是目标桶名。
	Bucket string
	// AccessKey/SecretKey 是访问凭证。
	AccessKey string
	SecretKey string
	// Region 是区域标识（部分厂商可选）。
	Region string
	// UseSSL 控制是否使用 HTTPS。
	UseSSL bool
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

// OpenObject opens one stored object and reports its size at open.
// The key is the catalog object_key, slash-normalized the same way as UploadFile.
// A missing object is os.ErrNotExist. The caller caps the read at the returned size.
func (u *S3Uploader) OpenObject(ctx context.Context, objectKey string) (io.ReadCloser, int64, error) {
	if u == nil || u.client == nil {
		return nil, 0, fmt.Errorf("object storage is not configured")
	}
	key := filepath.ToSlash(strings.TrimSpace(objectKey))
	if key == "" {
		return nil, 0, fmt.Errorf("empty object key")
	}
	obj, err := u.client.GetObject(ctx, u.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		if objectMissing(err) {
			return nil, 0, os.ErrNotExist
		}
		return nil, 0, err
	}
	info, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		if objectMissing(err) {
			return nil, 0, os.ErrNotExist
		}
		return nil, 0, err
	}
	if info.Size < 0 {
		_ = obj.Close()
		return nil, 0, fmt.Errorf("object size unknown")
	}
	return obj, info.Size, nil
}

func objectMissing(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "NoSuchKey" || resp.StatusCode == http.StatusNotFound
}
