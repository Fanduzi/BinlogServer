// Package tasks provides module-level functionality for tasks.
// input: ApplySealedUpload with fake uploader and file writer
// output: UPLOADED on success, UPLOAD_FAILED on upload error without failing the caller
// pos: best-effort upload caller tests
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
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
}
