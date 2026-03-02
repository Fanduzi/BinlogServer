// Package tasks provides module-level functionality for tasks.
// input: failed upload metadata, retry requests, and object storage uploader operations
// output: retry-upload execution results, failure aggregations, and retry metrics snapshots
// pos: scheduler upload-retry compensation and failure-observability logic
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"sort"
	"strings"
	"time"
)

func (s *Scheduler) RetryFailedUploads(taskID string, limit int) (UploadRetryStats, error) {
	const (
		defaultLimit = 100
		maxLimit     = 1000
	)
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		return UploadRetryStats{}, ErrInvalidRetryUploadLimit
	}
	if err := s.syncTasksFromStore(); err != nil {
		return UploadRetryStats{}, err
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

	// Step 2: 拉取候选并逐个重试，单文件失败不影响其他文件。
	files, err := s.listRetryUploadCandidates(taskID, limit, fileStore)
	if err != nil {
		return UploadRetryStats{}, err
	}

	var stats UploadRetryStats
	for _, file := range files {
		stats.Scanned++
		if !strings.EqualFold(file.UploadState, "UPLOAD_FAILED") {
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

		if err := uploader.UploadFile(context.Background(), taskID, file.FilePath, file.ObjectKey); err != nil {
			stats.Failed++
			_ = s.markRetryUploadFailure(fileStore, file, err.Error())
			continue
		}

		file.UploadState = "UPLOADED"
		file.UploadError = ""
		file.UploadedAt = time.Now()
		if err := fileStore.UpsertBinlogFile(context.Background(), file); err != nil {
			stats.Failed++
			continue
		}
		stats.Succeeded++
	}

	// Step 3: 汇总到全局 retry metrics。
	s.recordUploadRetryMetrics(stats)

	return stats, nil
}

// listRetryUploadCandidates 优先使用“失败文件专用查询”，否则退化到全量查询。
func (s *Scheduler) listRetryUploadCandidates(taskID string, limit int, fileStore FileStore) ([]BinlogFile, error) {
	if reader, ok := fileStore.(failedUploadFileReader); ok {
		return reader.ListFailedUploadBinlogFiles(context.Background(), taskID, limit)
	}
	return fileStore.ListBinlogFiles(context.Background(), taskID, limit)
}

// markRetryUploadFailure 记录单文件补传失败状态和错误原因。
func (s *Scheduler) markRetryUploadFailure(fileStore FileStore, file BinlogFile, reason string) error {
	file.UploadState = "UPLOAD_FAILED"
	file.UploadError = reason
	return fileStore.UpsertBinlogFile(context.Background(), file)
}

// isSealedFileForRetry 判定文件是否满足补传前提（已 seal 且非 open 文件）。
func isSealedFileForRetry(file BinlogFile) bool {
	name := strings.ToLower(strings.TrimSpace(file.FileName))
	path := strings.ToLower(strings.TrimSpace(file.FilePath))
	if strings.Contains(name, ".open.e") || strings.Contains(path, ".open.e") {
		return false
	}
	return !file.SealedAt.IsZero()
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
		return counter.CountUploadFailures(context.Background())
	}

	var total int64
	const allFilesLimit = int(^uint(0) >> 1)
	for _, taskID := range taskIDs {
		files, err := fileStore.ListBinlogFiles(context.Background(), taskID, allFilesLimit)
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
		return reader.ListUploadFailureReasons(context.Background(), taskID, limit)
	}

	const allFilesLimit = int(^uint(0) >> 1)
	files, err := fileStore.ListBinlogFiles(context.Background(), taskID, allFilesLimit)
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

// ListRuns 返回任务运行历史（按 started_at 倒序）。
