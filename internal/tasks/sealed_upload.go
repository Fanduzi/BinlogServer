// Package tasks provides module-level functionality for tasks.
// input: sealed binlog file metadata, FileUploader, and file metadata writer
// output: ObjectKey and UPLOADED only when the checksum matches; UPLOAD_FAILED with checksum mismatch or an unfinished check, without failing the caller
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

// sealedObjectChecker reads the stored object after upload. FileUploader stays upload-only.
type sealedObjectChecker interface {
	SealedObjectMatches(ctx context.Context, localPath, objectKey string) (bool, error)
}

// ApplySealedUpload uploads one sealed file and records UPLOADED or UPLOAD_FAILED.
// Upload errors are stored on the file row and not returned (best-effort).
// A checksum mismatch is UPLOAD_FAILED with ChecksumMismatch and ChecksumMismatchError.
// It is not returned, so replication keeps running, and the upload retry uploads it again.
// A failed object HEAD is UPLOAD_FAILED with an empty checksum and an upload_error
// prefixed by ChecksumVerifyPrefix. Empty is not match, not mismatch, and not verified.
// The upload retry checks that object again and does not upload it again.
// An uploader that cannot read the object stays UPLOADED with an empty checksum.
// Writer errors are returned so metadata is not silently lost.
func ApplySealedUpload(ctx context.Context, uploader FileUploader, writer BinlogFileWriter, file BinlogFile) (BinlogFile, error) {
	if uploader == nil {
		return file, nil
	}
	err := uploader.UploadFile(ctx, file.TaskID, file.FilePath, file.ObjectKey)
	if err != nil {
		file.UploadState = "UPLOAD_FAILED"
		file.UploadError = err.Error()
		file.Checksum = ""
		return persistBinlogFile(ctx, writer, file)
	}
	file.UploadedAt = time.Now()
	file.UploadError = ""
	sum, checkErr := sealedChecksum(ctx, uploader, file)
	switch {
	case checkErr != nil:
		file.UploadState = "UPLOAD_FAILED"
		file.UploadError = ChecksumVerifyPrefix + checkErr.Error()
		file.Checksum = ""
	case sum == ChecksumMismatch:
		file.UploadState = "UPLOAD_FAILED"
		file.UploadError = ChecksumMismatchError
		file.Checksum = ChecksumMismatch
	case sum == ChecksumMatch:
		file.UploadState = "UPLOADED"
		file.Checksum = ChecksumMatch
	default:
		file.UploadState = "UPLOADED"
		file.Checksum = ""
	}
	return persistBinlogFile(ctx, writer, file)
}

// verifySealedUpload checks an object that was already stored.
// A match becomes UPLOADED. A difference becomes a checksum mismatch so the
// next retry uploads it again. Another check error stays UPLOAD_FAILED.
func verifySealedUpload(ctx context.Context, uploader FileUploader, writer BinlogFileWriter, file BinlogFile) (BinlogFile, error) {
	checker, ok := uploader.(sealedObjectChecker)
	if !ok {
		file.UploadState = "UPLOAD_FAILED"
		file.Checksum = ""
		if !strings.HasPrefix(strings.TrimSpace(file.UploadError), ChecksumVerifyPrefix) {
			file.UploadError = ChecksumVerifyPrefix + "uploader cannot read object"
		}
		return persistBinlogFile(ctx, writer, file)
	}
	match, err := checker.SealedObjectMatches(ctx, file.FilePath, file.ObjectKey)
	if err != nil {
		file.UploadState = "UPLOAD_FAILED"
		file.UploadError = ChecksumVerifyPrefix + err.Error()
		file.Checksum = ""
		return persistBinlogFile(ctx, writer, file)
	}
	if !match {
		file.UploadState = "UPLOAD_FAILED"
		file.UploadError = ChecksumMismatchError
		file.Checksum = ChecksumMismatch
		return persistBinlogFile(ctx, writer, file)
	}
	file.UploadState = "UPLOADED"
	file.UploadError = ""
	file.Checksum = ChecksumMatch
	if file.UploadedAt.IsZero() {
		file.UploadedAt = time.Now()
	}
	return persistBinlogFile(ctx, writer, file)
}

func persistBinlogFile(ctx context.Context, writer BinlogFileWriter, file BinlogFile) (BinlogFile, error) {
	if writer == nil {
		return file, nil
	}
	return file, writer.UpsertBinlogFile(ctx, file)
}

func checksumVerifyPending(file BinlogFile) bool {
	switch strings.ToLower(strings.TrimSpace(file.Checksum)) {
	case ChecksumMatch, ChecksumMismatch:
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(file.UploadError), ChecksumVerifyPrefix)
}

// sealedChecksum is match only when the stored object compares equal to the sealed file.
// mismatch is a finished comparison whose bytes differ.
// A HEAD error is returned so the caller can retry the check.
// An uploader that cannot read the object returns an empty checksum and no error.
func sealedChecksum(ctx context.Context, uploader FileUploader, file BinlogFile) (string, error) {
	checker, ok := uploader.(sealedObjectChecker)
	if !ok {
		return "", nil
	}
	match, err := checker.SealedObjectMatches(ctx, file.FilePath, file.ObjectKey)
	if err != nil {
		return "", err
	}
	if !match {
		return ChecksumMismatch, nil
	}
	return ChecksumMatch, nil
}
