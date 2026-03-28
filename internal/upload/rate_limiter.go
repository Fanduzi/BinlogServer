// Package upload provides module-level functionality for upload.
// input: local binlog files, object store credentials/config, upload retry context
// output: object storage upload operations and upload status/error outcomes
// pos: outbound storage adapter layer for sealed binlog artifact distribution
// note: if this file changes, update this header and module README.md.
package upload

import (
	"context"

	"golang.org/x/time/rate"
)

// UploadRateLimiter 限制单位时间内的上传操作数（令牌桶）。
// 用于防止大量 binlog 文件突发上传时打爆对象存储带宽或 API 限额。
type UploadRateLimiter struct {
	limiter *rate.Limiter
}

// NewUploadRateLimiter 创建上传限速器。
// uploadsPerSecond <= 0 表示不限速（rate.Inf）。
// burst 为突发容量，至少为 1；若传入 0 则自动置为 1。
func NewUploadRateLimiter(uploadsPerSecond float64, burst int) *UploadRateLimiter {
	if burst < 1 {
		burst = 1
	}
	var lim *rate.Limiter
	if uploadsPerSecond <= 0 {
		lim = rate.NewLimiter(rate.Inf, burst)
	} else {
		lim = rate.NewLimiter(rate.Limit(uploadsPerSecond), burst)
	}
	return &UploadRateLimiter{limiter: lim}
}

// Wait 在令牌可用前阻塞，ctx 取消时立即返回错误。
func (r *UploadRateLimiter) Wait(ctx context.Context) error {
	return r.limiter.Wait(ctx)
}
