// Package tasks provides module-level functionality for tasks.
// input: failed upload metadata via failedUploadFileReader, manual and background retry requests, local sealed files, and object storage uploader operations
// output: retry-upload execution results, background retries of sealed UPLOAD_FAILED rows (a row of an unrepaired earlier-build failback is held until the repair) including a checksum mismatch re-upload and an unfinished checksum re-check, refusal of a name ClassifySegment rejects, ErrFailedUploadLookupNotAvailable when lookup is missing, failure aggregations, and retry metrics snapshots
// pos: scheduler upload-retry compensation, background retry loop, and failure-observability logic
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"binlog_server/internal/binlog"
)

const (
	defaultRetryUploadLimit              = 100
	maxRetryUploadLimit                  = 1000
	defaultBackgroundUploadRetryInterval = 15 * time.Second
)

// retryUploadOptions selects store sync and whether a missing local file is left unchanged.
// The manual API syncs and still attempts a missing file. The background loop skips it.
type retryUploadOptions struct {
	syncFromStore    bool
	skipMissingLocal bool
}

func (s *Scheduler) RetryFailedUploads(taskID string, limit int) (UploadRetryStats, error) {
	stats, err := s.retryFailedUploads(taskID, limit, retryUploadOptions{syncFromStore: true})
	if err != nil {
		return UploadRetryStats{}, err
	}
	s.recordUploadRetryMetrics(stats)
	return stats, nil
}

// RunBackgroundUploadRetry retries sealed UPLOAD_FAILED rows until ctx is cancelled.
// A checksum mismatch is uploaded again. An unfinished checksum check is checked again and is not uploaded again.
// A match becomes UPLOADED through the same path as RetryFailedUploads.
// Open segments are not uploaded. A sealed file that is not on this machine is skipped, so its catalog row stays as it was.
// interval <= 0 uses the default. No config key. The manual retry API is unchanged.
func (s *Scheduler) RunBackgroundUploadRetry(ctx context.Context, interval time.Duration) {
	s.mu.Lock()
	ready := s.fileUploader != nil && s.fileStore != nil
	if ready {
		_, ready = s.fileStore.(failedUploadFileReader)
	}
	s.mu.Unlock()
	if !ready {
		return
	}
	if interval <= 0 {
		interval = defaultBackgroundUploadRetryInterval
	}

	log.Printf("background upload retry started interval=%s", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		s.retryKnownFailedUploads()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// retryKnownFailedUploads runs one background pass over the tasks loaded in this process.
func (s *Scheduler) retryKnownFailedUploads() {
	if err := s.syncTasksFromStore(); err != nil {
		log.Printf("background upload retry sync tasks: %v", err)
		return
	}
	s.mu.Lock()
	fileStore := s.fileStore
	ids := make([]string, 0, len(s.tasks))
	for id := range s.tasks {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	if counter, ok := fileStore.(uploadFailureCounter); ok {
		ctx, cancel := s.withReadTimeout(context.Background())
		total, err := counter.CountUploadFailures(ctx)
		cancel()
		if err != nil {
			log.Printf("background upload retry count failures: %v", err)
		} else if total == 0 {
			return
		}
	}
	sort.Strings(ids)

	for _, id := range ids {
		stats, err := s.retryFailedUploads(id, defaultRetryUploadLimit, retryUploadOptions{skipMissingLocal: true})
		if err != nil {
			if errors.Is(err, ErrUploadRetryInProgress) || errors.Is(err, ErrTaskNotFound) {
				continue
			}
			log.Printf("background upload retry task=%s err=%v", id, err)
			continue
		}
		if stats.Succeeded == 0 && stats.Failed == 0 {
			continue
		}
		s.recordUploadRetryMetrics(stats)
		log.Printf("background upload retry task=%s succeeded=%d failed=%d skipped=%d", id, stats.Succeeded, stats.Failed, stats.Skipped)
	}
}

func (s *Scheduler) retryFailedUploads(taskID string, limit int, opts retryUploadOptions) (UploadRetryStats, error) {
	if limit <= 0 {
		limit = defaultRetryUploadLimit
	}
	if limit > maxRetryUploadLimit {
		return UploadRetryStats{}, ErrInvalidRetryUploadLimit
	}
	if opts.syncFromStore {
		if err := s.syncTasksFromStore(); err != nil {
			return UploadRetryStats{}, err
		}
	}

	// Step 1: 参数归一化 + 任务存在性 + 并发互斥校验。
	s.mu.Lock()
	if _, ok := s.tasks[taskID]; !ok {
		s.mu.Unlock()
		return UploadRetryStats{}, ErrTaskNotFound
	}
	if _, running := s.retryUploads[taskID]; running {
		s.mu.Unlock()
		return UploadRetryStats{}, ErrUploadRetryInProgress
	}
	fileStore := s.fileStore
	uploader := s.fileUploader
	s.retryUploads[taskID] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.retryUploads, taskID)
		s.mu.Unlock()
	}()

	if fileStore == nil || uploader == nil {
		return UploadRetryStats{}, ErrUploadRetryNotAvailable
	}
	if _, ok := fileStore.(failedUploadFileReader); !ok {
		return UploadRetryStats{}, ErrFailedUploadLookupNotAvailable
	}

	// Step 2: 拉取候选并逐个重试，单文件失败不影响其他文件。
	files, err := s.listRetryUploadCandidates(taskID, limit, fileStore)
	if err != nil {
		return UploadRetryStats{}, err
	}

	held := s.legacyHeldSources(taskID)
	var stats UploadRetryStats
	for _, file := range files {
		stats.Scanned++
		if !strings.EqualFold(file.UploadState, "UPLOAD_FAILED") {
			stats.Skipped++
			continue
		}
		// A row an earlier build pointed at the wrong failback file keeps
		// its state until the repair rewrites it: uploading it now would put
		// the later stint's bytes under the old key.
		if held[strings.TrimSpace(file.FileName)] {
			stats.Skipped++
			continue
		}
		if !isSealedFileForRetry(file) {
			stats.Skipped++
			continue
		}
		if strings.TrimSpace(file.FilePath) == "" {
			stats.Failed++
			_ = s.markRetryUploadFailure(fileStore, file, "retry upload skipped: empty file_path")
			continue
		}
		if strings.TrimSpace(file.ObjectKey) == "" {
			stats.Failed++
			_ = s.markRetryUploadFailure(fileStore, file, "retry upload skipped: empty object_key")
			continue
		}
		if opts.skipMissingLocal {
			if _, statErr := os.Stat(file.FilePath); statErr != nil {
				stats.Skipped++
				continue
			}
		}

		uploadCtx, cancelUpload := s.withUploadTimeout(context.Background())
		var updated BinlogFile
		var err error
		if checksumVerifyPending(file) {
			updated, err = verifySealedUpload(uploadCtx, uploader, fileStore, file)
		} else {
			updated, err = ApplySealedUpload(uploadCtx, uploader, fileStore, file)
		}
		cancelUpload()
		if err != nil || updated.UploadState != "UPLOADED" {
			stats.Failed++
			continue
		}
		stats.Succeeded++
	}

	return stats, nil
}

// listRetryUploadCandidates 只走失败文件查询，不再用限量 ListBinlogFiles 冒充「没有失败」。
func (s *Scheduler) listRetryUploadCandidates(taskID string, limit int, fileStore FileStore) ([]BinlogFile, error) {
	reader, ok := fileStore.(failedUploadFileReader)
	if !ok {
		return nil, ErrFailedUploadLookupNotAvailable
	}
	ctx, cancel := s.withReadTimeout(context.Background())
	files, err := reader.ListFailedUploadBinlogFiles(ctx, taskID, limit)
	cancel()
	return files, err
}

// markRetryUploadFailure 记录单文件补传失败状态和错误原因。
func (s *Scheduler) markRetryUploadFailure(fileStore FileStore, file BinlogFile, reason string) error {
	file.UploadState = "UPLOAD_FAILED"
	file.UploadError = reason
	ctx, cancel := s.withWriteTimeout(context.Background())
	err := fileStore.UpsertBinlogFile(ctx, file)
	cancel()
	return err
}

// isSealedFileForRetry 判定文件是否满足补传前提（已 seal 且非 open 文件）。
func isSealedFileForRetry(file BinlogFile) bool {
	if CatalogRowOpen(file) || file.SealedAt.IsZero() {
		return false
	}
	name := filepath.Base(strings.TrimSpace(file.FilePath))
	if name == "" || name == "." || name == ".." {
		name = strings.TrimSpace(file.FileName)
	}
	return binlog.SealedName(name) || binlog.SealedName(strings.TrimSpace(file.FileName))
}

// CountUploadFailures 统计全局上传失败记录数（metrics 使用）。
func (s *Scheduler) CountUploadFailures() (int64, error) {
	s.mu.Lock()
	fileStore := s.fileStore
	taskIDs := make([]string, 0, len(s.tasks))
	for taskID := range s.tasks {
		taskIDs = append(taskIDs, taskID)
	}
	s.mu.Unlock()

	if fileStore == nil {
		return 0, nil
	}
	if counter, ok := fileStore.(uploadFailureCounter); ok {
		ctx, cancel := s.withReadTimeout(context.Background())
		total, err := counter.CountUploadFailures(ctx)
		cancel()
		return total, err
	}

	var total int64
	const allFilesLimit = int(^uint(0) >> 1)
	for _, taskID := range taskIDs {
		ctx, cancel := s.withReadTimeout(context.Background())
		files, err := fileStore.ListBinlogFiles(ctx, taskID, allFilesLimit)
		cancel()
		if err != nil {
			continue
		}
		for _, file := range files {
			if strings.EqualFold(file.UploadState, "UPLOAD_FAILED") {
				total++
			}
		}
	}
	return total, nil
}

// GetUploadRetryMetrics 返回 retry-upload 的累计观测指标。
func (s *Scheduler) GetUploadRetryMetrics() UploadRetryMetrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	return UploadRetryMetrics{
		Success: s.retrySuccess,
		Failed:  s.retryFailed,
		Skipped: s.retrySkipped,
		LastTs:  s.retryLastTS,
	}
}

// recordUploadRetryMetrics 累加 retry-upload 的观测计数。
func (s *Scheduler) recordUploadRetryMetrics(stats UploadRetryStats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retrySuccess += int64(stats.Succeeded)
	s.retryFailed += int64(stats.Failed)
	s.retrySkipped += int64(stats.Skipped)
	s.retryLastTS = time.Now().Unix()
}

// ListUploadFailureReasons 按原因聚合上传失败，便于排障。
func (s *Scheduler) ListUploadFailureReasons(taskID string, limit int) ([]UploadFailureReason, error) {
	// 常见误解：
	// 这里返回的是“归一化后的原因聚合”，不是原始错误明细。
	// 设计目的：压缩噪声，便于直接看 Top N 问题类别。
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	if err := s.syncTasksFromStore(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	if _, ok := s.tasks[taskID]; !ok {
		s.mu.Unlock()
		return nil, ErrTaskNotFound
	}
	fileStore := s.fileStore
	s.mu.Unlock()

	if fileStore == nil {
		return []UploadFailureReason{}, nil
	}
	if reader, ok := fileStore.(uploadFailureReasonReader); ok {
		ctx, cancel := s.withReadTimeout(context.Background())
		reasons, err := reader.ListUploadFailureReasons(ctx, taskID, limit)
		cancel()
		return reasons, err
	}

	const allFilesLimit = int(^uint(0) >> 1)
	ctx, cancel := s.withReadTimeout(context.Background())
	files, err := fileStore.ListBinlogFiles(ctx, taskID, allFilesLimit)
	cancel()
	if err != nil {
		return nil, err
	}
	agg := make(map[string]UploadFailureReason)
	for _, file := range files {
		if !strings.EqualFold(file.UploadState, "UPLOAD_FAILED") {
			continue
		}
		reason := NormalizeUploadFailureReason(file.UploadError)
		item := agg[reason]
		item.Reason = reason
		item.Count++
		latest := file.UploadedAt
		if latest.IsZero() {
			latest = file.SealedAt
		}
		if latest.IsZero() {
			latest = file.CreatedAt
		}
		if latest.After(item.LatestTime) {
			item.LatestTime = latest
		}
		agg[reason] = item
	}

	out := make([]UploadFailureReason, 0, len(agg))
	for _, item := range agg {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if !out[i].LatestTime.Equal(out[j].LatestTime) {
			return out[i].LatestTime.After(out[j].LatestTime)
		}
		return out[i].Reason < out[j].Reason
	})
	if limit < len(out) {
		out = out[:limit]
	}
	return out, nil
}

// NormalizeUploadFailureReason 归一化失败原因（trim/压缩空白/空值转 unknown）。
func NormalizeUploadFailureReason(reason string) string {
	normalized := strings.Join(strings.Fields(strings.TrimSpace(reason)), " ")
	if normalized == "" {
		return "unknown"
	}
	return normalized
}

// legacyHeldSources is the source names of an unrepaired earlier-build
// failback in taskID (legacy_failback.go): every catalog row with one of
// these names waits for the repair. Empty when there is nothing to repair.
func (s *Scheduler) legacyHeldSources(taskID string) map[string]bool {
	s.mu.Lock()
	task, ok := s.tasks[taskID]
	s.mu.Unlock()
	if !ok {
		return nil
	}
	plan := s.legacyFailbackFor(task)
	if !plan.found() {
		return nil
	}
	out := make(map[string]bool)
	for _, name := range plan.Affected {
		if named, ok := binlog.ClassifySegment(name); ok {
			out[named.Source] = true
		}
	}
	for _, mv := range plan.Moves {
		if named, ok := binlog.ClassifySegment(mv.From); ok {
			out[named.Source] = true
		}
	}
	return out
}
