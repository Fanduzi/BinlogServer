// Package upload provides module-level functionality for upload.
// input: local binlog files, object store credentials/config, upload retry context, and an existing object key
// output: object storage upload operations, sealed ETag cases, a reader for one stored object at its size at open, a bucket HEAD that separates a missing bucket, an auth error, and a dead endpoint, and deletion of one stored key including a missing object; UseSSL true sends a TLS client hello; upload.tls.ca_file trusts only that CA (ADR 0012); a protocol-version alert is not a probe timeout; concurrent TLS failures each keep their own text
// pos: outbound storage adapter layer for sealed binlog upload and download
// note: if this file changes, update this header and module README.md.
package upload

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func newTestUploader(t *testing.T, serverURL string, useSSL bool) *S3Uploader {
	t.Helper()

	endpoint := strings.TrimPrefix(serverURL, "http://")
	endpoint = strings.TrimPrefix(endpoint, "https://")
	uploader, err := NewS3Uploader(S3Config{
		Endpoint:  endpoint,
		Bucket:    "binlog",
		AccessKey: "test-access",
		SecretKey: "test-secret",
		Region:    "us-east-1",
		UseSSL:    useSSL,
	})
	if err != nil {
		t.Fatalf("NewS3Uploader returned error: %v", err)
	}
	return uploader
}

// TestNewS3Uploader_RejectsIncompleteConfig 验证相关行为。
func TestNewS3Uploader_RejectsIncompleteConfig(t *testing.T) {
	_, err := NewS3Uploader(S3Config{
		Endpoint: "s3.example.com",
		Bucket:   "binlog",
	})
	if err == nil {
		t.Fatal("expected error for incomplete config")
	}
}

// TestNewS3Uploader_AcceptsMinimalValidConfig 验证完整必填配置可正常初始化。
func TestNewS3Uploader_AcceptsMinimalValidConfig(t *testing.T) {
	uploader, err := NewS3Uploader(S3Config{
		Endpoint:  "127.0.0.1:9000",
		Bucket:    "binlog",
		AccessKey: "test-access",
		SecretKey: "test-secret",
	})
	if err != nil {
		t.Fatalf("NewS3Uploader returned error: %v", err)
	}
	if uploader == nil {
		t.Fatal("expected uploader instance")
	}
	if uploader.bucket != "binlog" {
		t.Fatalf("expected bucket binlog, got %s", uploader.bucket)
	}
}

// TestNewS3Uploader_RejectsInvalidEndpoint 验证非法 endpoint 会返回初始化错误。
func TestNewS3Uploader_RejectsInvalidEndpoint(t *testing.T) {
	_, err := NewS3Uploader(S3Config{
		Endpoint:  "://bad-endpoint",
		Bucket:    "binlog",
		AccessKey: "test-access",
		SecretKey: "test-secret",
	})
	if err == nil {
		t.Fatal("expected error for invalid endpoint")
	}
}

// TestUploadFile_Success 验证成功上传时会发起对象写入请求。
func TestUploadFile_Success(t *testing.T) {
	var requestPath atomic.Value
	var requestBody atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestPath.Store(r.URL.Path)
		requestBody.Store(string(body))
		w.Header().Set("ETag", `"test-etag"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	uploader := newTestUploader(t, server.URL, false)
	localPath := filepath.Join(t.TempDir(), "mysql-bin.000001")
	if err := os.WriteFile(localPath, []byte("binlog-data"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	err := uploader.UploadFile(context.Background(), "task-1", localPath, "prefix/task-1/mysql-bin.000001")
	if err != nil {
		t.Fatalf("UploadFile returned error: %v", err)
	}

	if got := requestPath.Load(); got != "/binlog/prefix/task-1/mysql-bin.000001" {
		t.Fatalf("unexpected request path: %v", got)
	}
	if got := requestBody.Load(); !strings.Contains(fmt.Sprint(got), "binlog-data") {
		t.Fatalf("unexpected request body: %v", got)
	}
}

// TestUploadFile_PreservesObjectKeyLiteralPath 验证 object key 保持当前字面路径语义。
func TestUploadFile_PreservesObjectKeyLiteralPath(t *testing.T) {
	var requestPath atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath.Store(r.URL.Path)
		w.Header().Set("ETag", `"test-etag"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	uploader := newTestUploader(t, server.URL, false)
	localPath := filepath.Join(t.TempDir(), "mysql-bin.000001")
	if err := os.WriteFile(localPath, []byte("binlog-data"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	err := uploader.UploadFile(context.Background(), "task-1", localPath, `prefix\task-1\mysql-bin.000001`)
	if err != nil {
		t.Fatalf("UploadFile returned error: %v", err)
	}

	if got := requestPath.Load(); got != `/binlog/prefix\task-1\mysql-bin.000001` {
		t.Fatalf("unexpected literal request path: %v", got)
	}
}

// TestUploadFile_LocalFileMissingDoesNotSendRequest 验证本地文件不存在时不会继续上传。
func TestUploadFile_LocalFileMissingDoesNotSendRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("ETag", `"test-etag"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	uploader := newTestUploader(t, server.URL, false)
	err := uploader.UploadFile(context.Background(), "task-1", filepath.Join(t.TempDir(), "missing.binlog"), "prefix/task-1/missing.binlog")
	if err == nil {
		t.Fatal("expected error for missing local file")
	}
	if requests.Load() != 0 {
		t.Fatalf("expected no upload request, got %d", requests.Load())
	}
}

// TestUploadFile_PropagatesObjectStorageError 验证对象存储错误按现有语义向上返回。
func TestUploadFile_PropagatesObjectStorageError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upload failed", http.StatusInternalServerError)
	}))
	defer server.Close()

	uploader := newTestUploader(t, server.URL, false)
	localPath := filepath.Join(t.TempDir(), "mysql-bin.000001")
	if err := os.WriteFile(localPath, []byte("binlog-data"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	err := uploader.UploadFile(context.Background(), "task-1", localPath, "prefix/task-1/mysql-bin.000001")
	if err == nil {
		t.Fatal("expected upload error")
	}
	if !strings.Contains(err.Error(), "upload failed") && !strings.Contains(err.Error(), "500 Internal Server Error") {
		t.Fatalf("expected storage error details, got %v", err)
	}
}

// TestUploadFile_PropagatesContextCancel 验证 context cancel 会终止上传并返回上下文错误。
func TestUploadFile_PropagatesContextCancel(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(500 * time.Millisecond)
	}))
	defer server.Close()

	uploader := newTestUploader(t, server.URL, false)
	localPath := filepath.Join(t.TempDir(), "mysql-bin.000001")
	if err := os.WriteFile(localPath, []byte(strings.Repeat("a", 1<<20)), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- uploader.UploadFile(ctx, "task-1", localPath, "prefix/task-1/mysql-bin.000001")
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upload request did not start")
	}
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected context cancellation error")
		}
		if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), context.Canceled.Error()) {
			t.Fatalf("expected context canceled, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("UploadFile did not return after context cancel")
	}
}

// TestUploadFile_UseSSLAgainstTLSServer 验证启用 SSL 时可对接 HTTPS endpoint。
func TestUploadFile_UseSSLAgainstTLSServer(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"test-etag"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	endpoint := strings.TrimPrefix(server.URL, "https://")
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4("test-access", "test-secret", ""),
		Secure: true,
		Region: "us-east-1",
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	})
	if err != nil {
		t.Fatalf("minio.New returned error: %v", err)
	}
	uploader := &S3Uploader{client: client, bucket: "binlog"}

	localPath := filepath.Join(t.TempDir(), "mysql-bin.000001")
	if err := os.WriteFile(localPath, []byte("binlog-data"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	if err := uploader.UploadFile(context.Background(), "task-1", localPath, "prefix/task-1/mysql-bin.000001"); err != nil {
		t.Fatalf("UploadFile returned error: %v", err)
	}
}

// TestUploadFile_EmptyObjectKeyReturnsError 验证空 object key 会按现有语义返回错误。
func TestUploadFile_EmptyObjectKeyReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	uploader := newTestUploader(t, server.URL, false)
	localPath := filepath.Join(t.TempDir(), "mysql-bin.000001")
	if err := os.WriteFile(localPath, []byte("binlog-data"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	if err := uploader.UploadFile(context.Background(), "task-1", localPath, ""); err == nil {
		t.Fatal("expected error for empty object key")
	}
}

// TestNewS3Uploader_PreservesEndpointHostPort 验证 endpoint host:port 组合可被正常接受。
func TestNewS3Uploader_PreservesEndpointHostPort(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"test-etag"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("url.Parse returned error: %v", err)
	}

	uploader, err := NewS3Uploader(S3Config{
		Endpoint:  parsed.Host,
		Bucket:    "binlog",
		AccessKey: "test-access",
		SecretKey: "test-secret",
		Region:    "us-east-1",
	})
	if err != nil {
		t.Fatalf("NewS3Uploader returned error: %v", err)
	}
	if uploader.bucket != "binlog" {
		t.Fatalf("unexpected bucket: %s", uploader.bucket)
	}
}

// TestUploadFile_PropagatesDeadlineExceeded 验证 deadline 结束时返回上下文超时。
func TestUploadFile_PropagatesDeadlineExceeded(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(500 * time.Millisecond)
	}))
	defer server.Close()

	uploader := newTestUploader(t, server.URL, false)
	localPath := filepath.Join(t.TempDir(), "mysql-bin.000001")
	if err := os.WriteFile(localPath, []byte(strings.Repeat("a", 1<<20)), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- uploader.UploadFile(ctx, "task-1", localPath, "prefix/task-1/mysql-bin.000001")
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upload request did not start")
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected deadline exceeded error")
		}
		if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
			t.Fatalf("expected deadline exceeded, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("UploadFile did not return after deadline exceeded")
	}
}

func TestOpenObject_ReadsSizeAtOpen(t *testing.T) {
	objectBody := []byte("sealed-object-bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/binlog/prefix/cluster/mysql-bin.000001" {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message><Resource>` + r.URL.Path + `</Resource></Error>`))
			return
		}
		w.Header().Set("ETag", `"test-etag"`)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("Content-Length", strconv.Itoa(len(objectBody)))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write(objectBody)
	}))
	defer server.Close()

	uploader := newTestUploader(t, server.URL, false)
	rc, size, err := uploader.OpenObject(context.Background(), "prefix/cluster/mysql-bin.000001")
	if err != nil {
		t.Fatalf("OpenObject returned error: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(objectBody)) || !bytes.Equal(got, objectBody) {
		t.Fatalf("size %d body %q", size, got)
	}

	_, _, err = uploader.OpenObject(context.Background(), "prefix/cluster/missing")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing object: %v", err)
	}
	if _, _, err := uploader.OpenObject(context.Background(), "  "); err == nil {
		t.Fatal("expected error for empty object key")
	}
}

// TestNewS3Uploader_AllowsRegionAndSSLCombination 验证 region/use_ssl 组合配置可初始化。
func TestNewS3Uploader_AllowsRegionAndSSLCombination(t *testing.T) {
	uploader, err := NewS3Uploader(S3Config{
		Endpoint:  "127.0.0.1:9000",
		Bucket:    "binlog",
		AccessKey: "test-access",
		SecretKey: "test-secret",
		Region:    "cn-north-1",
		UseSSL:    true,
	})
	if err != nil {
		t.Fatalf("NewS3Uploader returned error: %v", err)
	}
	if uploader == nil {
		t.Fatal("expected uploader instance")
	}
}

func TestSealedETag_SinglePartAtBoundaryAndMultipart(t *testing.T) {
	dir := t.TempDir()
	small := filepath.Join(dir, "small")
	if err := os.WriteFile(small, []byte("binlog-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := sealedETag(small, 16)
	if err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum([]byte("binlog-data"))
	if got != hex.EncodeToString(sum[:]) {
		t.Fatalf("single etag %s", got)
	}

	exact := filepath.Join(dir, "exact")
	if err := os.WriteFile(exact, []byte("abcd"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = sealedETag(exact, 4)
	if err != nil {
		t.Fatal(err)
	}
	exactSum := md5.Sum([]byte("abcd"))
	if got != hex.EncodeToString(exactSum[:]) {
		t.Fatalf("boundary etag %s, want single-part md5", got)
	}

	payload := []byte("abcdefghij")
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = sealedETag(big, 4)
	if err != nil {
		t.Fatal(err)
	}
	p1 := md5.Sum(payload[:4])
	p2 := md5.Sum(payload[4:8])
	p3 := md5.Sum(payload[8:])
	concat := append(append(p1[:], p2[:]...), p3[:]...)
	all := md5.Sum(concat)
	want := hex.EncodeToString(all[:]) + "-3"
	if got != want {
		t.Fatalf("multipart etag %s, want %s", got, want)
	}
}

func TestDeleteObject_RemovesKey(t *testing.T) {
	var method atomic.Value
	var requestPath atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method.Store(r.Method)
		requestPath.Store(r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	uploader := newTestUploader(t, server.URL, false)
	if err := uploader.DeleteObject(context.Background(), "prefix/cluster/mysql-bin.000001"); err != nil {
		t.Fatalf("DeleteObject returned error: %v", err)
	}
	if method.Load() != http.MethodDelete {
		t.Fatalf("method = %v", method.Load())
	}
	if requestPath.Load() != "/binlog/prefix/cluster/mysql-bin.000001" {
		t.Fatalf("path = %v", requestPath.Load())
	}
}

func TestObjectMissing_IgnoresNoSuchBucket(t *testing.T) {
	if objectMissing(minio.ErrorResponse{Code: "NoSuchBucket", StatusCode: http.StatusNotFound}) {
		t.Fatal("NoSuchBucket counted as a missing object")
	}
	if !objectMissing(minio.ErrorResponse{Code: "NoSuchKey", StatusCode: http.StatusNotFound}) {
		t.Fatal("NoSuchKey was not a missing object")
	}
}

func TestProbeStorage_ClassifiesBucketHEAD(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   string
	}{
		{name: "ok", status: http.StatusOK},
		{name: "missing-bucket", status: http.StatusNotFound, want: "NoSuchBucket"},
		{name: "auth", status: http.StatusForbidden, want: "Access Denied"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			uploader := newTestUploader(t, server.URL, false)
			err := uploader.ProbeStorage(context.Background())
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("probe %v, want %s", err, tc.want)
			}
			if !strings.Contains(err.Error(), "bucket binlog") || !strings.Contains(err.Error(), "endpoint ") {
				t.Fatalf("probe does not name bucket and endpoint: %v", err)
			}
			if strings.Contains(err.Error(), "test-secret") || strings.Contains(err.Error(), "test-access") {
				t.Fatalf("probe includes a credential: %v", err)
			}
		})
	}

	uploader, err := NewS3Uploader(S3Config{
		Endpoint: "127.0.0.1:1", Bucket: "binlog", AccessKey: "key", SecretKey: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = uploader.ProbeStorage(ctx)
	if err == nil {
		t.Fatal("dead endpoint probe succeeded")
	}
	if !strings.Contains(err.Error(), "bucket binlog") || !strings.Contains(err.Error(), "endpoint 127.0.0.1:1") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("dead endpoint probe: %v", err)
	}
}

func TestDeleteObject_MissingObjectIsSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>`))
	}))
	defer server.Close()

	uploader := newTestUploader(t, server.URL, false)
	if err := uploader.DeleteObject(context.Background(), "prefix/cluster/missing"); err != nil {
		t.Fatalf("missing object returned error: %v", err)
	}
}

func TestDeleteObject_PropagatesStorageError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "delete failed", http.StatusInternalServerError)
	}))
	defer server.Close()

	uploader := newTestUploader(t, server.URL, false)
	err := uploader.DeleteObject(context.Background(), "prefix/cluster/mysql-bin.000001")
	if err == nil {
		t.Fatal("expected delete error")
	}
	if !strings.Contains(err.Error(), "delete failed") && !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected storage error, got %v", err)
	}
}

func TestDeleteObject_EmptyObjectKeyReturnsError(t *testing.T) {
	uploader := newTestUploader(t, "http://127.0.0.1:9", false)
	if err := uploader.DeleteObject(context.Background(), "  "); err == nil {
		t.Fatal("expected error for empty object key")
	}
}

func TestDeleteObject_MinIORemovesObject(t *testing.T) {
	endpoint := os.Getenv("BINLOG_E2E_MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("set BINLOG_E2E_MINIO_ENDPOINT to delete a real object")
	}
	bucket := os.Getenv("BINLOG_E2E_MINIO_BUCKET")
	access := os.Getenv("BINLOG_E2E_MINIO_ACCESS_KEY")
	secret := os.Getenv("BINLOG_E2E_MINIO_SECRET_KEY")
	if bucket == "" || access == "" || secret == "" {
		t.Fatal("MinIO endpoint is set but bucket or credentials are empty")
	}
	uploader, err := NewS3Uploader(S3Config{
		Endpoint:  endpoint,
		Bucket:    bucket,
		AccessKey: access,
		SecretKey: secret,
		Region:    "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(access, secret, ""),
		Secure: false,
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	exists, err := raw.BucketExists(ctx, bucket)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		if err := raw.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
			t.Fatal(err)
		}
	}
	key := "e2e/retention-purge/" + strconv.FormatInt(time.Now().UnixNano(), 10) + "/mysql-bin.000001"
	body := bytes.NewReader([]byte("sealed-segment-to-purge"))
	if _, err := raw.PutObject(ctx, bucket, key, body, int64(body.Len()), minio.PutObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := uploader.DeleteObject(ctx, key); err != nil {
		t.Fatal(err)
	}
	_, err = raw.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
	if err == nil {
		t.Fatal("object still exists after DeleteObject")
	}
	resp := minio.ToErrorResponse(err)
	if resp.Code != "NoSuchKey" && resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stat after delete: %v", err)
	}
	if err := uploader.DeleteObject(ctx, key); err != nil {
		t.Fatalf("second delete of a missing object: %v", err)
	}
}

// TestUseSSLTrueSendsTLSClientHello proves UseSSL selects TLS on the wire.
// The config default is covered by TestUploadUseSSLDefaultsToTLS.
func TestUseSSLTrueSendsTLSClientHello(t *testing.T) {
	for _, useSSL := range []bool{true, false} {
		t.Run(strconv.FormatBool(useSSL), func(t *testing.T) {
			first := firstDialByte(t, useSSL)
			if useSSL && first != 0x16 {
				t.Fatalf("UseSSL true first byte = %#x, want TLS client hello 0x16", first)
			}
			if !useSSL && first == 0x16 {
				t.Fatal("UseSSL false sent a TLS client hello")
			}
		})
	}
}

func firstDialByte(t *testing.T, useSSL bool) byte {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var b [1]byte
		_, _ = conn.Read(b[:])
		got <- b[0]
	}()
	up, err := NewS3Uploader(S3Config{
		Endpoint:  ln.Addr().String(),
		Bucket:    "binlog",
		AccessKey: "test-access",
		SecretKey: "test-secret",
		Region:    "us-east-1",
		UseSSL:    useSSL,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _, _, _ = up.ListObjects(ctx, "p", 1) }()
	select {
	case b := <-got:
		cancel()
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("client did not dial")
		return 0
	}
}

func TestCAFileTrustsOnlyThatCA(t *testing.T) {
	caPEM, cert := privateServerCert(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { defer c.Close(); _ = c.(*tls.Conn).Handshake() }(c)
		}
	}()
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	wrongPEM, _ := privateServerCert(t)
	wrongPath := filepath.Join(t.TempDir(), "wrong.pem")
	if err := os.WriteFile(wrongPath, wrongPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	probe := func(ca string) error {
		t.Helper()
		up, err := NewS3Uploader(S3Config{
			Endpoint: ln.Addr().String(), Bucket: "binlog",
			AccessKey: "k", SecretKey: "s", UseSSL: true, CAFile: ca,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return up.ProbeStorage(ctx)
	}
	if err := probe(caPath); err == nil || TLSTransportFailure(err.Error()) {
		t.Fatalf("trusted CA = %v", err)
	}
	if err := probe(wrongPath); err == nil || !TLSTransportFailure(err.Error()) {
		t.Fatalf("untrusted CA = %v", err)
	}
	if err := probe(""); err == nil || !TLSTransportFailure(err.Error()) {
		t.Fatalf("system roots = %v", err)
	}
}

func TestProbeStorage_UntrustedCAIsNotATimeout(t *testing.T) {
	_, cert := privateServerCert(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var accepts atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			go func(c net.Conn) { defer c.Close(); _ = c.(*tls.Conn).Handshake() }(c)
		}
	}()
	wrongPEM, _ := privateServerCert(t)
	wrongPath := filepath.Join(t.TempDir(), "wrong.pem")
	if err := os.WriteFile(wrongPath, wrongPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	up, err := NewS3Uploader(S3Config{
		Endpoint: ln.Addr().String(), Bucket: "binlog",
		AccessKey: "k", SecretKey: "s", UseSSL: true, CAFile: wrongPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	err = up.ProbeStorage(ctx)
	elapsed := time.Since(start)
	if err == nil || !TLSTransportFailure(err.Error()) || strings.Contains(err.Error(), "timed out") {
		t.Fatalf("probe %v", err)
	}
	if !strings.Contains(err.Error(), "unknown authority") || !strings.Contains(err.Error(), "upload.tls.ca_file") || strings.Contains(err.Error(), "upload.use_ssl: false") {
		t.Fatalf("probe %v", err)
	}
	var ua x509.UnknownAuthorityError
	if !errors.As(err, &ua) {
		t.Fatalf("probe %v lost the unknown-authority type", err)
	}
	if elapsed > time.Second {
		t.Fatalf("probe took %s; a bad CA was retried until the deadline", elapsed)
	}
	if got := accepts.Load(); got != 1 {
		t.Fatalf("accepts %d, want 1 (a bad CA is not retried)", got)
	}
}

func TestCAFileTransportKeepsMinIODefaults(t *testing.T) {
	caPEM, _ := privateServerCert(t)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	pool, err := loadCAFile(caPath)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := objectTransport(pool)
	if err != nil {
		t.Fatal(err)
	}
	base, ok := rt.(*stopTLSRetry).base.(*http.Transport)
	if !ok || base.Proxy == nil {
		t.Fatal("transport is not minio's default")
	}
	if base.ResponseHeaderTimeout != time.Minute || base.TLSHandshakeTimeout != 10*time.Second || base.IdleConnTimeout != time.Minute || base.ExpectContinueTimeout != 10*time.Second {
		t.Fatalf("timeouts header=%s handshake=%s idle=%s continue=%s", base.ResponseHeaderTimeout, base.TLSHandshakeTimeout, base.IdleConnTimeout, base.ExpectContinueTimeout)
	}
	if base.MaxIdleConns != 256 || base.MaxIdleConnsPerHost != 16 || !base.DisableCompression {
		t.Fatalf("pool idle=%d perHost=%d compression=%v", base.MaxIdleConns, base.MaxIdleConnsPerHost, base.DisableCompression)
	}
	if base.TLSClientConfig == nil || base.TLSClientConfig.RootCAs != pool || base.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatal("RootCAs was not the only trust change")
	}
	req := &http.Request{URL: &url.URL{Scheme: "https", Host: "example.com"}}
	got, err := base.Proxy(req)
	if err != nil {
		t.Fatal(err)
	}
	want, err := http.ProxyFromEnvironment(req)
	if err != nil {
		t.Fatal(err)
	}
	if (got == nil) != (want == nil) || (got != nil && got.String() != want.String()) {
		t.Fatalf("proxy %v, want %v", got, want)
	}
}

func TestProbeStorage_ProtocolVersionIsNotATimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1024)
				_, _ = c.Read(buf)
				// fatal protocol_version alert
				_, _ = c.Write([]byte{21, 3, 1, 0, 2, 2, 70})
			}(c)
		}
	}()
	up, err := NewS3Uploader(S3Config{
		Endpoint: ln.Addr().String(), Bucket: "binlog", AccessKey: "k", SecretKey: "s", UseSSL: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	err = up.ProbeStorage(ctx)
	if err == nil || time.Since(start) > time.Second || strings.Contains(err.Error(), "timed out") || !TLSTransportFailure(err.Error()) || strings.Contains(err.Error(), "upload.tls.ca_file") || strings.Contains(err.Error(), "upload.use_ssl: false") {
		t.Fatalf("protocol version after %s = %v", time.Since(start), err)
	}
	if !strings.Contains(err.Error(), "protocol version") && !strings.Contains(err.Error(), "remote error: tls:") {
		t.Fatalf("protocol version text lost: %v", err)
	}
}

type tlsTokKey struct{}

type ctxErrRT struct{}

func (ctxErrRT) RoundTrip(req *http.Request) (*http.Response, error) {
	msg, _ := req.Context().Value(tlsTokKey{}).(string)
	return nil, errors.New(msg)
}

func TestFinishTLSConcurrentFailuresKeepOwnText(t *testing.T) {
	const a = "tls: first record does not look like a TLS handshake marker-a"
	const b = "remote error: tls: handshake failure marker-b"
	stop := &stopTLSRetry{base: ctxErrRT{}}
	ctxA := withTLSSave(context.WithValue(context.Background(), tlsTokKey{}, a))
	ctxB := withTLSSave(context.WithValue(context.Background(), tlsTokKey{}, b))
	req := func(ctx context.Context) *http.Request {
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://127.0.0.1/bucket", nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	var rtA, rtB error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, rtA = stop.RoundTrip(req(ctxA))
	}()
	go func() {
		defer wg.Done()
		_, rtB = stop.RoundTrip(req(ctxB))
	}()
	wg.Wait()
	u := &S3Uploader{}
	wrap := func(err error) error {
		return &url.Error{Op: "Get", URL: "https://127.0.0.1/bucket", Err: err}
	}
	gotA := u.finishTLS(ctxA, wrap(rtA))
	gotB := u.finishTLS(ctxB, wrap(rtB))
	if gotA == nil || !strings.Contains(gotA.Error(), "marker-a") || strings.Contains(gotA.Error(), "marker-b") {
		t.Fatalf("A = %v", gotA)
	}
	if gotB == nil || !strings.Contains(gotB.Error(), "marker-b") || strings.Contains(gotB.Error(), "marker-a") || strings.Contains(gotB.Error(), "upload.tls.ca_file") {
		t.Fatalf("B = %v", gotB)
	}
}

func TestProbeStorage_PlainHTTPHintsUseSSL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	endpoint := strings.TrimPrefix(srv.URL, "http://")
	up, err := NewS3Uploader(S3Config{
		Endpoint: endpoint, Bucket: "binlog", AccessKey: "k", SecretKey: "s", UseSSL: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = up.ProbeStorage(ctx)
	if err == nil || !plainHTTPEndpoint(err.Error()) || !strings.Contains(err.Error(), "upload.use_ssl: false") || strings.Contains(err.Error(), "upload.tls.ca_file") {
		t.Fatalf("plain HTTP = %v", err)
	}
}

func TestAnnotateTLS_Classes(t *testing.T) {
	httpErr := annotateTLS(errors.New("http: server gave HTTP response to HTTPS client"))
	if httpErr == nil || !strings.Contains(httpErr.Error(), "HTTP response to HTTPS client") || !strings.Contains(httpErr.Error(), "upload.use_ssl: false") || strings.Contains(httpErr.Error(), "upload.tls.ca_file") {
		t.Fatalf("http-to-https = %v", httpErr)
	}
	hostErr := annotateTLS(x509.HostnameError{Certificate: &x509.Certificate{DNSNames: []string{"wrong.example"}}, Host: "db.example"})
	if hostErr == nil || !strings.Contains(hostErr.Error(), "hostname does not match") || strings.Contains(hostErr.Error(), "upload.tls.ca_file") || strings.Contains(hostErr.Error(), "upload.use_ssl: false") {
		t.Fatalf("hostname = %v", hostErr)
	}
	uaErr := annotateTLS(x509.UnknownAuthorityError{})
	if uaErr == nil || !strings.Contains(uaErr.Error(), "unknown authority") || !strings.Contains(uaErr.Error(), "upload.tls.ca_file") || strings.Contains(uaErr.Error(), "upload.use_ssl: false") {
		t.Fatalf("unknown authority = %v", uaErr)
	}
	other := annotateTLS(x509.CertificateInvalidError{Reason: x509.Expired})
	if other == nil || other.Error() != (x509.CertificateInvalidError{Reason: x509.Expired}).Error() || strings.Contains(other.Error(), "upload.tls.ca_file") || strings.Contains(other.Error(), "upload.use_ssl: false") {
		t.Fatalf("other = %v", other)
	}
	again := annotateTLS(uaErr)
	if again.Error() != uaErr.Error() {
		t.Fatalf("hint stacked: %v", again)
	}
}

func TestProbeStorage_HostnameMismatchHint(t *testing.T) {
	caPEM, cert := certForName(t, "wrong.example")
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { defer c.Close(); _ = c.(*tls.Conn).Handshake() }(c)
		}
	}()
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	up, err := NewS3Uploader(S3Config{
		Endpoint: ln.Addr().String(), Bucket: "binlog",
		AccessKey: "k", SecretKey: "s", UseSSL: true, CAFile: caPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = up.ProbeStorage(ctx)
	if err == nil || !strings.Contains(err.Error(), "hostname does not match") || strings.Contains(err.Error(), "upload.tls.ca_file") || strings.Contains(err.Error(), "upload.use_ssl: false") {
		t.Fatalf("hostname = %v", err)
	}
	var host x509.HostnameError
	if !errors.As(err, &host) {
		t.Fatalf("hostname type lost: %v", err)
	}
}

func certForName(t *testing.T, dnsName string) ([]byte, tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "upload-test-ca"},
		IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: dnsName},
		DNSNames:     []string{dnsName},
		NotBefore:    time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}
}

func privateServerCert(t *testing.T) ([]byte, tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "upload-test-ca"},
		IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		NotBefore:    time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}
}
