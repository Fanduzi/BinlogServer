// Package tasks provides module-level functionality for tasks.
// input: ApplySealedUpload with fake uploader and file writer
// output: UPLOADED on success, UPLOAD_FAILED on upload error without failing the caller, checksum match or mismatch from the stored bytes, and an empty checksum when the object HEAD returns an error
// pos: best-effort upload caller tests
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type sealedUploadWriter struct {
	files []BinlogFile
	err   error
}

func (w *sealedUploadWriter) UpsertBinlogFile(_ context.Context, meta BinlogFile) error {
	if w.err != nil {
		return w.err
	}
	w.files = append(w.files, meta)
	return nil
}

func TestApplySealedUpload_RecordsFailureWithoutReturningIt(t *testing.T) {
	writer := &sealedUploadWriter{}
	file := BinlogFile{TaskID: "1", FilePath: "/tmp/a", ObjectKey: "k", UploadState: "LOCAL_ONLY"}
	updated, err := ApplySealedUpload(context.Background(), &retryTestUploader{errByObject: map[string]error{"k": errors.New("boom")}}, writer, file)
	if err != nil {
		t.Fatalf("best-effort upload should not return upload error, got %v", err)
	}
	if updated.UploadState != "UPLOAD_FAILED" {
		t.Fatalf("state=%s, want UPLOAD_FAILED", updated.UploadState)
	}
	if len(writer.files) != 1 || writer.files[0].UploadState != "UPLOAD_FAILED" {
		t.Fatalf("expected failed row persisted, got %+v", writer.files)
	}
}

type okUploader struct{}

func (okUploader) UploadFile(context.Context, string, string, string) error { return nil }

func TestApplySealedUpload_RecordsUploaded(t *testing.T) {
	writer := &sealedUploadWriter{}
	file := BinlogFile{TaskID: "1", FilePath: "/tmp/a", ObjectKey: "k"}
	updated, err := ApplySealedUpload(context.Background(), okUploader{}, writer, file)
	if err != nil {
		t.Fatalf("ApplySealedUpload: %v", err)
	}
	if updated.UploadState != "UPLOADED" {
		t.Fatalf("state=%s, want UPLOADED", updated.UploadState)
	}
	if updated.Checksum != "" {
		t.Fatalf("checksum=%q, want empty when the uploader cannot read the object", updated.Checksum)
	}
}

type storedUploader struct {
	objects map[string][]byte
	swap    bool
}

func (u *storedUploader) UploadFile(_ context.Context, _ string, localPath, objectKey string) error {
	b, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	if u.swap && len(b) > 0 {
		b[0] ^= 0xff
	}
	if u.objects == nil {
		u.objects = map[string][]byte{}
	}
	u.objects[objectKey] = append([]byte(nil), b...)
	return nil
}

func (u *storedUploader) SealedObjectMatches(_ context.Context, localPath, objectKey string) (bool, error) {
	local, err := os.ReadFile(localPath)
	if err != nil {
		return false, err
	}
	stored, ok := u.objects[objectKey]
	if !ok {
		return false, nil
	}
	return bytes.Equal(local, stored), nil
}

func TestApplySealedUpload_ChecksumFollowsStoredBytes(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "mysql-bin.000001")
	if err := os.WriteFile(local, []byte("sealed-segment-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := BinlogFile{TaskID: "1", FilePath: local, ObjectKey: "k/mysql-bin.000001"}

	matched, err := ApplySealedUpload(context.Background(), &storedUploader{}, &sealedUploadWriter{}, file)
	if err != nil {
		t.Fatalf("matching upload returned %v", err)
	}
	if matched.UploadState != "UPLOADED" || matched.Checksum != ChecksumMatch {
		t.Fatalf("match path: state=%s checksum=%s", matched.UploadState, matched.Checksum)
	}

	mismatched, err := ApplySealedUpload(context.Background(), &storedUploader{swap: true}, &sealedUploadWriter{}, file)
	if err != nil {
		t.Fatalf("mismatch must not fail the caller, got %v", err)
	}
	if mismatched.UploadState != "UPLOADED" || mismatched.Checksum != ChecksumMismatch {
		t.Fatalf("mismatch path: state=%s checksum=%s", mismatched.UploadState, mismatched.Checksum)
	}
}

// headFailUploader stores the sealed file, then the object HEAD returns an error.
type headFailUploader struct {
	storedUploader
}

func (u *headFailUploader) SealedObjectMatches(context.Context, string, string) (bool, error) {
	return false, errors.New("head object: connection reset")
}

func TestApplySealedUpload_HeadErrorLeavesChecksumEmpty(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "mysql-bin.000001")
	if err := os.WriteFile(local, []byte("sealed-segment-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	writer := &sealedUploadWriter{}
	file := BinlogFile{TaskID: "1", FilePath: local, ObjectKey: "k/mysql-bin.000001"}

	updated, err := ApplySealedUpload(context.Background(), &headFailUploader{}, writer, file)
	if err != nil {
		t.Fatalf("HEAD error must not fail the caller, got %v", err)
	}
	if updated.UploadState != "UPLOADED" {
		t.Fatalf("state=%s, want UPLOADED", updated.UploadState)
	}
	if updated.Checksum == ChecksumMatch || updated.Checksum == ChecksumMismatch || updated.Checksum != "" {
		t.Fatalf("checksum=%q, want empty after a failed HEAD", updated.Checksum)
	}
	if len(writer.files) != 1 || writer.files[0].Checksum != "" || writer.files[0].UploadState != "UPLOADED" {
		t.Fatalf("persisted row=%+v", writer.files)
	}
}
