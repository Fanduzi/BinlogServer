// Package assurance provides module-level functionality for assurance.
// input: VERIFY_MINIO_ENDPOINT, VERIFY_MINIO_ACCESS_KEY, VERIFY_MINIO_SECRET_KEY, VERIFY_MINIO_BUCKET; a reachable MinIO
// output: TestVerifyAgainstMinIO reports a deleted object, a one-byte object change and a one-byte local change against that MinIO; skipped when the endpoint is unset
// pos: real-object check for the phase 3 verifier, driven by scripts/e2e/smoke-verify.sh
// note: if this file changes, update this header and module README.md.
package assurance

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func TestVerifyAgainstMinIO(t *testing.T) {
	endpoint := os.Getenv("VERIFY_MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("set VERIFY_MINIO_ENDPOINT to run against MinIO")
	}
	access := getenv("VERIFY_MINIO_ACCESS_KEY", "minioadmin")
	secret := getenv("VERIFY_MINIO_SECRET_KEY", "minioadmin")
	bucket := getenv("VERIFY_MINIO_BUCKET", "e2e-verify")
	client, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(access, secret, ""), Secure: false})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	f := live(t)
	rows := verifyRows(t, f, time.Now().UTC().Add(-time.Hour))
	if len(rows) < 3 {
		t.Fatalf("need 3 sealed segments, got %d", len(rows))
	}
	rows = rows[:3]
	store := &minioObjects{c: client, bucket: bucket}
	for _, row := range rows {
		path := filepath.Join(f.dir, row.Name)
		if _, err := client.FPutObject(ctx, bucket, row.ObjectKey, path, minio.PutObjectOptions{}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.RemoveObject(ctx, bucket, row.ObjectKey, minio.RemoveObjectOptions{}) })
	}
	opt := Options{Now: time.Now().UTC(), RotationPeriods: 1}
	clean, err := Verify(ctx, f.dir, rows, store, nil, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(clean.Findings) != 0 {
		t.Fatalf("clean pass %+v", clean.Findings)
	}
	deleted, flippedObj, flippedLocal := rows[0], rows[1], rows[2]
	if err := client.RemoveObject(ctx, bucket, deleted.ObjectKey, minio.RemoveObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(f.dir, flippedObj.Name))
	if err != nil {
		t.Fatal(err)
	}
	body[len(body)/2] ^= 0xff
	if _, err := client.PutObject(ctx, bucket, flippedObj.ObjectKey, bytes.NewReader(body), int64(len(body)), minio.PutObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(f.dir, flippedLocal.Name)
	local, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	local[len(local)/2] ^= 0xff
	if err := os.WriteFile(localPath, local, 0o644); err != nil {
		t.Fatal(err)
	}
	bad, err := Verify(ctx, f.dir, rows, store, nil, opt)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFinding(bad.Findings, KindObjectMissing, deleted.Name) ||
		!hasFinding(bad.Findings, KindObjectCorrupt, flippedObj.Name) ||
		!hasFinding(bad.Findings, KindVerifyFailed, flippedLocal.Name) {
		t.Fatalf("minio pass %+v", bad.Findings)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

type minioObjects struct {
	c      *minio.Client
	bucket string
}

func (m *minioObjects) Head(ctx context.Context, key string) (int64, error) {
	info, err := m.c.StatObject(ctx, m.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" || minio.ToErrorResponse(err).StatusCode == 404 {
			return 0, os.ErrNotExist
		}
		return 0, err
	}
	return info.Size, nil
}

func (m *minioObjects) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := m.c.GetObject(ctx, m.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		if minio.ToErrorResponse(err).Code == "NoSuchKey" || minio.ToErrorResponse(err).StatusCode == 404 {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	return obj, nil
}
