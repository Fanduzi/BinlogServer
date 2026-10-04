// Package upload provides module-level functionality for upload.
// input: local binlog files, object store credentials/config, upload retry context, and an existing object key
// output: object storage upload operations, sealed ETag cases, a reader for one stored object at its size at open, and deletion of one stored key including a missing object
// pos: outbound storage adapter layer for sealed binlog upload and download
// note: if this file changes, update this header and module README.md.
package upload

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
