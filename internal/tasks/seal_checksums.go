// Package tasks provides module-level functionality for tasks.
// input: a task's segment listing, the catalog's sealed sha256 rows (SealedChecksumLister, migration 000008) and the task directory's .seal-sha256 ledger
// output: sealChecksums: the sha256 and size each sealed segment had at seal, by base name; a catalog row wins over a sha on the listing, which wins over the ledger
// pos: ADR 0009 P2-3 rehash source (#380 QA); without it the rehasher reads nothing
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"strings"

	"binlog_server/internal/binlog"
)

// SealedChecksumLister is a catalog that can return the sha256 recorded at
// seal (migration 000008). Rows carry FileName, Epoch, SizeBytes and SHA256.
type SealedChecksumLister interface {
	ListSealedChecksums(ctx context.Context, taskID string) ([]BinlogFile, error)
}

// sealChecksums is the checksum at seal of each sealed segment in files, by
// base name. A segment with no recorded checksum is absent.
func (s *Scheduler) sealChecksums(taskID string, files []BinlogFile) map[string]binlog.SealSum {
	s.mu.Lock()
	dataDir, store := s.dataDir, s.fileStore
	s.mu.Unlock()
	out := map[string]binlog.SealSum{}
	if dir, ok := taskBinlogDir(dataDir, taskID); ok {
		if sums, err := binlog.LoadSealSums(dir); err == nil {
			for name, sum := range sums {
				out[name] = sum
			}
		}
	}
	type rowKey struct {
		name  string
		epoch int64
	}
	names := map[rowKey]string{}
	for _, f := range files {
		name := segmentInventoryBasename(f)
		if name == "" || segmentIsOpen(f) {
			continue
		}
		names[rowKey{f.FileName, f.Epoch}] = name
		if sha := strings.ToLower(strings.TrimSpace(f.SHA256)); sha != "" {
			size := f.SizeBytes
			if size <= 0 {
				size = -1
			}
			out[name] = binlog.SealSum{SHA256: sha, Size: size}
		}
	}
	if lister, ok := store.(SealedChecksumLister); ok {
		ctx, cancel := s.withReadTimeout(context.Background())
		rows, err := lister.ListSealedChecksums(ctx, taskID)
		cancel()
		if err == nil {
			for _, row := range rows {
				name, ok := names[rowKey{row.FileName, row.Epoch}]
				sha := strings.ToLower(strings.TrimSpace(row.SHA256))
				if !ok || sha == "" {
					continue
				}
				size := row.SizeBytes
				if size <= 0 {
					size = -1
				}
				out[name] = binlog.SealSum{SHA256: sha, Size: size}
			}
		}
	}
	return out
}
