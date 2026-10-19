// Package tasks provides module-level functionality for tasks.
// input: a task's catalog rows (when there is a catalog) and its task directory listing
// output: chainOrder, the segment order, catalog rows and an error the health evaluator's chain reads: without a catalog the /replay order of the directory; with one, the catalog's rows reconciled with the directory, so a segment on disk with no row (a hand-added copy) is on the order and is I4/I6 evidence, and every catalogued segment carries its row (no I6_MISSING_ROW flood); RetentionRemoved comes from ListRetentionMarkers, and a real marker-query error is returned so the chain scope stays incomplete (1054 is an empty marker set)
// pos: ADR 0009 phase 2 P2-3 QA round 1 (#380): the chain order with meta
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"sort"
	"strings"
	"time"

	"binlog_server/internal/assurance"
)

// chainOrder is the evaluator's chain order and, with a catalog, its rows.
// A listing error leaves the order empty, like replayOrder.
func (s *Scheduler) chainOrder(taskID, dataDir string, catalog bool) ([]string, []assurance.Row, error) {
	if !catalog {
		return s.replayOrder(taskID), nil, nil
	}
	files, err := s.ListFiles(taskID, segmentInventoryLimit)
	if err != nil {
		return nil, nil, nil
	}
	markers, err := s.retentionMarkers(taskID)
	if err != nil {
		log.Printf("health: retention markers task=%s: %v", taskID, err)
		return nil, nil, err
	}
	known := map[string]bool{}
	rows := make([]assurance.Row, 0, len(files))
	for _, f := range files {
		name := segmentInventoryBasename(f)
		if name == "" {
			continue
		}
		known[name] = true
		_, marked := markers[RetentionMarkerKey(f.FileName, f.Epoch)]
		rows = append(rows, assurance.Row{
			Name: name, Source: f.FileName, Epoch: f.Epoch, Sealed: !segmentIsOpen(f), Size: f.SizeBytes,
			UploadState: f.UploadState, Checksum: f.Checksum, ObjectKey: f.ObjectKey, CreatedAt: f.CreatedAt, SealedAt: f.SealedAt,
			RetentionRemoved: marked,
		})
	}
	merged := append([]BinlogFile(nil), files...)
	if disk, err := listTaskBinlogFilesOnDisk(dataDir, taskID, segmentInventoryLimit); err == nil {
		for _, f := range disk {
			if name := segmentInventoryBasename(f); name != "" && !known[name] {
				merged = append(merged, f)
			}
		}
	}
	chosen := SelectReplayFiles(merged)
	order := make([]string, 0, len(chosen))
	for _, f := range chosen {
		if name := segmentInventoryBasename(f); name != "" {
			order = append(order, name)
		}
	}
	return order, rows, nil
}

// retentionMarkers reads 000010. A store that cannot answer is no markers,
// not an error. A real read error leaves the chain scope incomplete (§26.4).
func (s *Scheduler) retentionMarkers(taskID string) (map[string]time.Time, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	store := s.fileStore
	s.mu.Unlock()
	lister, ok := store.(interface {
		ListRetentionMarkers(ctx context.Context, taskID string) (map[string]time.Time, error)
	})
	if !ok || lister == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return lister.ListRetentionMarkers(ctx, taskID)
}

// rowsKey is a short digest of the row fields the chain reads, so a cached
// chain result is reused only while the catalog says the same.
func rowsKey(rows []assurance.Row) string {
	if len(rows) == 0 {
		return ""
	}
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		lines = append(lines, fmt.Sprintf("%s|%d|%t|%d|%s|%s|%s|%t", r.Name, r.Epoch, r.Sealed, r.Size, r.UploadState, r.Checksum, r.ObjectKey, r.RetentionRemoved))
	}
	sort.Strings(lines)
	h := fnv.New64a()
	h.Write([]byte(strings.Join(lines, "\n")))
	return fmt.Sprintf("%x", h.Sum64())
}
