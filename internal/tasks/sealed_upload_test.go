// Package tasks provides module-level functionality for tasks.
// input: ApplySealedUpload with fake uploader and file writer
// output: UPLOADED on a checksum match, UPLOAD_FAILED on upload error, checksum mismatch, or a failed object HEAD, without failing the caller; an empty SHA256 is filled from the sealed file, an existing digest is kept, and stored event times are not overwritten by the fill
// pos: best-effort upload caller tests
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"binlog_server/internal/binlog"
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

	writer := &sealedUploadWriter{}
	mismatched, err := ApplySealedUpload(context.Background(), &storedUploader{swap: true}, writer, file)
	if err != nil {
		t.Fatalf("mismatch must not fail the caller, got %v", err)
	}
	if mismatched.UploadState != "UPLOAD_FAILED" || mismatched.Checksum != ChecksumMismatch || mismatched.UploadError != ChecksumMismatchError {
		t.Fatalf("mismatch path: state=%s checksum=%s err=%q", mismatched.UploadState, mismatched.Checksum, mismatched.UploadError)
	}
	if len(writer.files) != 1 || writer.files[0].UploadState != "UPLOAD_FAILED" || writer.files[0].Checksum != ChecksumMismatch {
		t.Fatalf("persisted mismatch=%+v", writer.files)
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
	if updated.UploadState != "UPLOAD_FAILED" {
		t.Fatalf("state=%s, want UPLOAD_FAILED", updated.UploadState)
	}
	if updated.Checksum == ChecksumMatch || updated.Checksum == ChecksumMismatch || updated.Checksum != "" {
		t.Fatalf("checksum=%q, want empty after a failed HEAD", updated.Checksum)
	}
	if !strings.HasPrefix(updated.UploadError, ChecksumVerifyPrefix) {
		t.Fatalf("upload_error=%q, want %s prefix", updated.UploadError, ChecksumVerifyPrefix)
	}
	if len(writer.files) != 1 || writer.files[0].Checksum != "" || writer.files[0].UploadState != "UPLOAD_FAILED" {
		t.Fatalf("persisted row=%+v", writer.files)
	}
}

func TestApplySealedUpload_FillsEmptySHA256(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "mysql-bin.000003")
	raw, err := os.ReadFile(filepath.Join("..", "binlog", "testdata", "ledger", "live", "mysql-bin.000003"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	facts, err := binlog.ReadSealFacts(local)
	if err != nil {
		t.Fatal(err)
	}
	writer := &sealedUploadWriter{}
	updated, err := ApplySealedUpload(context.Background(), okUploader{}, writer, BinlogFile{TaskID: "1", FilePath: local, ObjectKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.SHA256 != facts.SHA256 {
		t.Fatalf("sha256 %q, file %q", updated.SHA256, facts.SHA256)
	}
	if len(writer.files) != 1 || writer.files[0].SHA256 != facts.SHA256 {
		t.Fatalf("persisted sha %+v", writer.files)
	}
	sums, err := binlog.LoadSealSums(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sums["mysql-bin.000003"].SHA256 != facts.SHA256 {
		t.Fatalf("ledger %+v", sums["mysql-bin.000003"])
	}
	kept, err := ApplySealedUpload(context.Background(), okUploader{}, &sealedUploadWriter{}, BinlogFile{TaskID: "1", FilePath: local, ObjectKey: "k", SHA256: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if kept.SHA256 != "abc" {
		t.Fatalf("existing digest was replaced with %q", kept.SHA256)
	}
}

func TestApplySealedUpload_FillKeepsStoredEventTimes(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "mysql-bin.000003")
	raw, err := os.ReadFile(filepath.Join("..", "binlog", "testdata", "ledger", "live", "mysql-bin.000003"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	facts, err := binlog.ReadSealFacts(local)
	if err != nil || !facts.HasSpan {
		t.Fatalf("fixture facts %+v %v", facts, err)
	}
	first := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	last := time.Date(2026, 1, 1, 0, 0, 2, 0, time.UTC)
	writer := &sealedUploadWriter{}
	updated, err := ApplySealedUpload(context.Background(), okUploader{}, writer,
		BinlogFile{TaskID: "1", FilePath: local, ObjectKey: "k", FirstEventAt: &first, LastEventAt: &last})
	if err != nil {
		t.Fatal(err)
	}
	if updated.SHA256 != facts.SHA256 {
		t.Fatalf("sha256 not filled: %q", updated.SHA256)
	}
	if !updated.FirstEventAt.Equal(first) || !updated.LastEventAt.Equal(last) {
		t.Fatalf("stored event times overwritten: first=%v last=%v", updated.FirstEventAt, updated.LastEventAt)
	}
	// No stored times: the file's span fills them.
	empty, err := ApplySealedUpload(context.Background(), okUploader{}, &sealedUploadWriter{}, BinlogFile{TaskID: "1", FilePath: local, ObjectKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if empty.FirstEventAt == nil || !empty.FirstEventAt.Equal(facts.First) || empty.LastEventAt == nil || !empty.LastEventAt.Equal(facts.Last) {
		t.Fatalf("event times not filled: %+v", empty)
	}
}
