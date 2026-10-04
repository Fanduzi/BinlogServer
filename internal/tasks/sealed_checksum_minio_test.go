// Package tasks provides module-level functionality for tasks.
// input: a running MinIO endpoint from the retry-upload e2e, a sealed local file, and S3Uploader
// output: proof that a matching object reports checksum match and a same-size different object reports mismatch without failing the caller
// pos: object-store checksum test for ApplySealedUpload; skipped unless BINLOG_E2E_MINIO_ENDPOINT is set
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"binlog_server/internal/upload"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func TestApplySealedUpload_MinIOChecksum(t *testing.T) {
	endpoint := os.Getenv("BINLOG_E2E_MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("set BINLOG_E2E_MINIO_ENDPOINT to compare a sealed file with a real object")
	}
	bucket := os.Getenv("BINLOG_E2E_MINIO_BUCKET")
	access := os.Getenv("BINLOG_E2E_MINIO_ACCESS_KEY")
	secret := os.Getenv("BINLOG_E2E_MINIO_SECRET_KEY")
	if bucket == "" || access == "" || secret == "" {
		t.Fatal("MinIO endpoint is set but bucket or credentials are empty")
	}

	uploader, err := upload.NewS3Uploader(upload.S3Config{
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

	dir := t.TempDir()
	small := filepath.Join(dir, "mysql-bin.000001")
	smallBytes := []byte("sealed-segment-bytes-for-minio")
	if err := os.WriteFile(small, smallBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	smallKey := "e2e/checksum-probe/" + strconv.FormatInt(time.Now().UnixNano(), 10) + "/mysql-bin.000001"
	smallFile := BinlogFile{TaskID: "checksum", FileName: "mysql-bin.000001", FilePath: small, ObjectKey: smallKey}

	matched, err := ApplySealedUpload(ctx, uploader, &sealedUploadWriter{}, smallFile)
	if err != nil {
		t.Fatalf("matching upload returned %v", err)
	}
	if matched.UploadState != "UPLOADED" || matched.Checksum != ChecksumMatch {
		t.Fatalf("matching upload: state=%s checksum=%s", matched.UploadState, matched.Checksum)
	}

	large := filepath.Join(dir, "mysql-bin.000002")
	if err := writeSize(large, int(16<<20)+1); err != nil {
		t.Fatal(err)
	}
	largeKey := "e2e/checksum-probe/" + strconv.FormatInt(time.Now().UnixNano(), 10) + "/mysql-bin.000002"
	largeMatched, err := ApplySealedUpload(ctx, uploader, &sealedUploadWriter{}, BinlogFile{
		TaskID: "checksum", FileName: "mysql-bin.000002", FilePath: large, ObjectKey: largeKey,
	})
	if err != nil {
		t.Fatalf("multipart matching upload returned %v", err)
	}
	if largeMatched.UploadState != "UPLOADED" || largeMatched.Checksum != ChecksumMatch {
		t.Fatalf("multipart matching upload: state=%s checksum=%s", largeMatched.UploadState, largeMatched.Checksum)
	}

	swapped, err := ApplySealedUpload(ctx, corruptAfterUpload{S3Uploader: uploader, raw: raw, bucket: bucket}, &sealedUploadWriter{}, smallFile)
	if err != nil {
		t.Fatalf("mismatch must not fail the caller, got %v", err)
	}
	if swapped.UploadState != "UPLOADED" || swapped.Checksum != ChecksumMismatch {
		t.Fatalf("mismatch: state=%s checksum=%s", swapped.UploadState, swapped.Checksum)
	}
}

// corruptAfterUpload stores the sealed file, then replaces the object with different bytes of the same size.
type corruptAfterUpload struct {
	*upload.S3Uploader
	raw    *minio.Client
	bucket string
}

func (c corruptAfterUpload) UploadFile(ctx context.Context, taskID, localPath, objectKey string) error {
	if err := c.S3Uploader.UploadFile(ctx, taskID, localPath, objectKey); err != nil {
		return err
	}
	local, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	body := bytes.Repeat([]byte{0x5a}, len(local))
	if bytes.Equal(body, local) && len(body) > 0 {
		body[0] ^= 0xff
	}
	_, err = c.raw.PutObject(ctx, c.bucket, objectKey, bytes.NewReader(body), int64(len(body)), minio.PutObjectOptions{})
	return err
}

func writeSize(path string, n int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 1024*1024)
	for i := range buf {
		buf[i] = byte(i)
	}
	written := 0
	for written < n {
		chunk := n - written
		if chunk > len(buf) {
			chunk = len(buf)
		}
		if _, err := f.Write(buf[:chunk]); err != nil {
			return err
		}
		written += chunk
	}
	return nil
}
