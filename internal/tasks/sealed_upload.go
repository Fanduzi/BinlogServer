// Package tasks provides module-level functionality for tasks.
// input: sealed binlog file metadata, FileUploader, and file metadata writer
// output: ObjectKey, UPLOADED or UPLOAD_FAILED metadata; upload failure does not fail the caller
// pos: single best-effort upload caller used after seal and on retry
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"strings"
	"time"
)

// ObjectKey builds the object-store key. It joins with "/" and does not filepath.Clean.
func ObjectKey(prefix, clusterKey, sourceServerUUID, fileName string) string {
	parts := make([]string, 0, 4)
	if p := strings.Trim(strings.TrimSpace(prefix), "/"); p != "" {
		parts = append(parts, p)
	}
	parts = append(parts,
		strings.Trim(strings.TrimSpace(clusterKey), "/"),
		strings.Trim(strings.TrimSpace(sourceServerUUID), "/"),
		strings.Trim(strings.TrimSpace(fileName), "/"),
	)
	return strings.Join(parts, "/")
}

// BinlogFileWriter persists binlog file metadata.
type BinlogFileWriter interface {
	UpsertBinlogFile(ctx context.Context, meta BinlogFile) error
}

// ApplySealedUpload uploads one sealed file and records UPLOADED or UPLOAD_FAILED.
// Upload errors are stored on the file row and not returned (best-effort).
// Writer errors are returned so metadata is not silently lost.
func ApplySealedUpload(ctx context.Context, uploader FileUploader, writer BinlogFileWriter, file BinlogFile) (BinlogFile, error) {
	if uploader == nil {
		return file, nil
	}
	err := uploader.UploadFile(ctx, file.TaskID, file.FilePath, file.ObjectKey)
	if err != nil {
		file.UploadState = "UPLOAD_FAILED"
		file.UploadError = err.Error()
		if writer == nil {
			return file, nil
		}
		return file, writer.UpsertBinlogFile(ctx, file)
	}
	file.UploadState = "UPLOADED"
	file.UploadError = ""
	file.UploadedAt = time.Now()
	if writer == nil {
		return file, nil
	}
	return file, writer.UpsertBinlogFile(ctx, file)
}
