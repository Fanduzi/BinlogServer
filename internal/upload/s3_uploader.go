// Package upload provides module-level functionality for upload.
// input: local binlog files, object store credentials/config, upload retry context, and an existing object key
// output: sealed-file upload, a HEAD ETag comparison against that sealed file, a reader plus the object size at open for one stored key, a HEAD size for one stored key (a missing key is os.ErrNotExist; a NoSuchBucket body is not), a bucket HEAD for the startup probe whose errors name the bucket and endpoint and not the credentials, and deletion of one stored key
// pos: outbound storage adapter for sealed binlog upload, checksum, download, and retention delete
// note: if this file changes, update this header and module README.md.
package upload

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// minioPartSize is the part size FPutObject uses when PartSize is 0 (minio-go v7 minPartSize).
// ponytail: multipart ETag uses this fixed 16MiB until the object exceeds 16MiB*10000; update it if that SDK default changes.
const minioPartSize int64 = 16 << 20

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
	client   *minio.Client
	bucket   string
	endpoint string
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
		client:   client,
		bucket:   cfg.Bucket,
		endpoint: cfg.Endpoint,
	}, nil
}

// UploadFile 上传本地文件到指定 object key。
func (u *S3Uploader) UploadFile(ctx context.Context, _ string, localPath, objectKey string) error {
	_, err := u.client.FPutObject(ctx, u.bucket, filepath.ToSlash(objectKey), localPath, minio.PutObjectOptions{})
	return err
}

// ListObjects lists the keys under prefix with their sizes (design 0009
// §21.3). It stops after limit keys (limit <= 0 means no limit) and
// reports truncated, so the caller never treats a partial listing as
// complete. Any listing error is returned and nothing is judged from it.
func (u *S3Uploader) ListObjects(ctx context.Context, prefix string, limit int) (map[string]int64, bool, error) {
	if u == nil || u.client == nil {
		return nil, false, fmt.Errorf("object storage is not configured")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := map[string]int64{}
	for obj := range u.client.ListObjects(ctx, u.bucket, minio.ListObjectsOptions{Prefix: filepath.ToSlash(prefix), Recursive: true}) {
		if obj.Err != nil {
			return nil, false, obj.Err
		}
		if limit > 0 && len(out) >= limit {
			return out, true, nil
		}
		out[obj.Key] = obj.Size
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return out, false, nil
}

// SealedObjectMatches reports whether the stored object is the same bytes as the sealed local file.
// It HEADs the object and compares that ETag with the ETag of the local file.
// A single PUT at or under 16MiB uses the file MD5. A larger upload uses the same 16MiB part boundaries as UploadFile.
// A missing ETag or a size difference is not a match. A stat error is returned so the caller can leave checksum empty.
func (u *S3Uploader) SealedObjectMatches(ctx context.Context, localPath, objectKey string) (bool, error) {
	if u == nil || u.client == nil {
		return false, fmt.Errorf("object storage is not configured")
	}
	key := filepath.ToSlash(strings.TrimSpace(objectKey))
	if key == "" {
		return false, fmt.Errorf("empty object key")
	}
	want, err := sealedETag(localPath, minioPartSize)
	if err != nil {
		return false, err
	}
	info, err := u.client.StatObject(ctx, u.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return false, err
	}
	st, err := os.Stat(localPath)
	if err != nil {
		return false, err
	}
	if info.Size >= 0 && info.Size != st.Size() {
		return false, nil
	}
	got := strings.Trim(strings.TrimSpace(info.ETag), `"`)
	if got == "" {
		return false, nil
	}
	return strings.EqualFold(got, want), nil
}

// sealedETag is the S3 ETag UploadFile's FPutObject produces for this part size.
func sealedETag(localPath string, partSize int64) (string, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if partSize <= 0 {
		partSize = minioPartSize
	}
	if st.Size() <= partSize {
		h := md5.New()
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	buf := make([]byte, partSize)
	concat := make([]byte, 0, md5.Size*2)
	parts := 0
	for {
		n, err := io.ReadFull(f, buf)
		if n > 0 {
			sum := md5.Sum(buf[:n])
			concat = append(concat, sum[:]...)
			parts++
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	if parts == 0 {
		return hex.EncodeToString(md5.New().Sum(nil)), nil
	}
	sum := md5.Sum(concat)
	return hex.EncodeToString(sum[:]) + "-" + strconv.Itoa(parts), nil
}

// ProbeStorage HEADs the bucket. A missing bucket, an auth failure, and a
// network error are returned, each naming the bucket and endpoint and none
// of them the credentials. A bucket that answers is success, including when
// the sentinel object key is absent. Startup uses this so a missing bucket
// is not mistaken for a missing key: an object HEAD with an empty body is
// reported as NoSuchKey. A policy that can ListBucket but not GetObject is
// not visible here; the verifier sees it when it HEADs an uploaded object.
func (u *S3Uploader) ProbeStorage(ctx context.Context) error {
	if u == nil || u.client == nil {
		return fmt.Errorf("object storage is not configured")
	}
	exists, err := u.client.BucketExists(ctx, u.bucket)
	if err != nil {
		return u.probeErr(err)
	}
	if !exists {
		return u.probeErr(fmt.Errorf("NoSuchBucket"))
	}
	return nil
}

func (u *S3Uploader) probeErr(err error) error {
	return fmt.Errorf("%w bucket %s endpoint %s", err, u.bucket, u.endpoint)
}

// HeadObject reports the size of one stored object. A missing object is os.ErrNotExist.
func (u *S3Uploader) HeadObject(ctx context.Context, objectKey string) (int64, error) {
	if u == nil || u.client == nil {
		return 0, fmt.Errorf("object storage is not configured")
	}
	key := filepath.ToSlash(strings.TrimSpace(objectKey))
	if key == "" {
		return 0, fmt.Errorf("empty object key")
	}
	info, err := u.client.StatObject(ctx, u.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if objectMissing(err) {
			return 0, os.ErrNotExist
		}
		return 0, err
	}
	return info.Size, nil
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

// DeleteObject removes one stored object. The key is slash-normalized the same way as UploadFile.
// A missing object is success so retention can retry after a partial purge.
// Other storage errors are returned and the caller keeps the local segment.
func (u *S3Uploader) DeleteObject(ctx context.Context, objectKey string) error {
	if u == nil || u.client == nil {
		return fmt.Errorf("object storage is not configured")
	}
	key := filepath.ToSlash(strings.TrimSpace(objectKey))
	if key == "" {
		return fmt.Errorf("empty object key")
	}
	err := u.client.RemoveObject(ctx, u.bucket, key, minio.RemoveObjectOptions{})
	if err != nil && objectMissing(err) {
		return nil
	}
	return err
}

func objectMissing(err error) bool {
	resp := minio.ToErrorResponse(err)
	// A missing bucket is often 404 too. Leaving it as ErrNotExist would
	// make the startup probe, and a later HEAD, look like a missing key.
	if resp.Code == "NoSuchBucket" {
		return false
	}
	return resp.Code == "NoSuchKey" || resp.StatusCode == http.StatusNotFound
}
