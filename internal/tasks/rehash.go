// Package tasks provides module-level functionality for tasks.
// input: the sealed segments of every task the evaluator is responsible for, with the checksum each had at seal (catalog sha256 from migration 000008, else the task directory's .seal-sha256 ledger), and a global byte budget
// output: Rehasher: an asynchronous sha256 re-check, one segment at a time on a cross-task cursor (persisted in rehash_cursor when the store has it), token-bucket limited to health.rehash_bytes_per_sec, idle I/O priority and no page-cache pollution on Linux; results go to the evaluator as facts and mark the task dirty (REHASH); a segment without a recorded sha256, or whose size is no longer the size at seal, is skipped and counted unavailable; a cycle is one snapshot of the targets, so a step does no catalog listing
// pos: ADR 0009 phase 2 P2-3 (design §5.7); never on the evaluation path, so a pass hashes nothing
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"binlog_server/internal/binlog"

	"golang.org/x/time/rate"
)

// RehashCursorStore persists the rehasher's cursor (one row, rehash_cursor).
type RehashCursorStore interface {
	LoadRehashCursor(ctx context.Context) (taskID, segment string, err error)
	SaveRehashCursor(ctx context.Context, taskID, segment string) error
}

// RehashStats counts the rehasher's work.
type RehashStats struct {
	Bytes, Segments, Mismatches, Unavailable int64
}

// Rehasher re-reads sealed segments and compares them with the sha256
// recorded at seal (bit rot does not change size or mtime).
type Rehasher struct {
	e       *Evaluator
	limiter *rate.Limiter
	chunk   int

	mu         sync.Mutex
	cursorTask string
	cursorSeg  string
	loaded     bool
	queue      []rehashTarget

	bytes, segments, mismatches, unavailable atomic.Int64
}

// NewRehasher limits reads to bytesPerSec (default 4 MiB/s) across all tasks.
func NewRehasher(e *Evaluator, bytesPerSec int64) *Rehasher {
	if bytesPerSec <= 0 {
		bytesPerSec = 4 << 20
	}
	chunk := int(bytesPerSec / 8)
	if chunk < 4<<10 {
		chunk = 4 << 10
	}
	if chunk > 64<<10 {
		chunk = 64 << 10
	}
	return &Rehasher{e: e, limiter: rate.NewLimiter(rate.Limit(bytesPerSec), chunk), chunk: chunk}
}

// Stats is a snapshot of the counters.
func (r *Rehasher) Stats() RehashStats {
	return RehashStats{Bytes: r.bytes.Load(), Segments: r.segments.Load(), Mismatches: r.mismatches.Load(), Unavailable: r.unavailable.Load()}
}

type rehashTarget struct {
	taskID, name, path, sha string
	size                    int64 // bytes at seal; -1 when the source does not say
}

// next is the next segment of the current cycle. A cycle is a snapshot of
// every responsible task's sealed segments in (task, segment) order, taken
// when the previous one is used up and starting after the cursor, so a
// step costs one stat and one read, not a catalog listing per task.
func (r *Rehasher) next() (rehashTarget, bool) {
	r.mu.Lock()
	if len(r.queue) > 0 {
		t := r.queue[0]
		r.queue = r.queue[1:]
		r.mu.Unlock()
		return t, true
	}
	ct, cs := r.cursorTask, r.cursorSeg
	r.mu.Unlock()
	all := r.collect()
	if len(all) == 0 {
		return rehashTarget{}, false
	}
	start := 0 // a full cycle wraps back to the start
	for i, t := range all {
		if t.taskID > ct || (t.taskID == ct && t.name > cs) {
			start = i
			break
		}
	}
	cycle := append(append([]rehashTarget(nil), all[start:]...), all[:start]...)
	r.mu.Lock()
	r.queue = cycle[1:]
	r.mu.Unlock()
	return cycle[0], true
}

// collect lists the sealed segments of every responsible task with the
// checksum each one had at seal (sealChecksums).
func (r *Rehasher) collect() []rehashTarget {
	var all []rehashTarget
	for _, t := range r.e.responsible() {
		files, err := r.e.s.ListFiles(t.ID, segmentInventoryLimit)
		if err != nil {
			continue
		}
		sums := r.e.s.sealChecksums(t.ID, files)
		for _, f := range files {
			if segmentIsOpen(f) || strings.TrimSpace(f.FilePath) == "" {
				continue
			}
			name := segmentInventoryBasename(f)
			if name == "" {
				name = filepath.Base(f.FilePath)
			}
			sum, ok := sums[name]
			if !ok {
				sum = binlog.SealSum{Size: -1}
			}
			all = append(all, rehashTarget{taskID: t.ID, name: name, path: f.FilePath, sha: sum.SHA256, size: sum.Size})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].taskID != all[j].taskID {
			return all[i].taskID < all[j].taskID
		}
		return all[i].name < all[j].name
	})
	return all
}

func (r *Rehasher) loadCursor(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loaded {
		return
	}
	r.loaded = true
	if cs := r.e.cfg.RehashCursor; cs != nil {
		if task, seg, err := cs.LoadRehashCursor(ctx); err == nil {
			r.cursorTask, r.cursorSeg = task, seg
		}
	}
}

func (r *Rehasher) advance(ctx context.Context, t rehashTarget) {
	r.mu.Lock()
	r.cursorTask, r.cursorSeg = t.taskID, t.name
	r.mu.Unlock()
	if cs := r.e.cfg.RehashCursor; cs != nil {
		if err := cs.SaveRehashCursor(ctx, t.taskID, t.name); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("rehash cursor save: %v", err)
		}
	}
}

// Step re-checks the next segment and returns the bytes it read.
func (r *Rehasher) Step(ctx context.Context) (int64, error) {
	n, _, err := r.step(ctx)
	return n, err
}

// step is Step that also says whether there was a segment to look at.
func (r *Rehasher) step(ctx context.Context) (int64, bool, error) {
	r.loadCursor(ctx)
	t, ok := r.next()
	if !ok {
		return 0, false, nil
	}
	n, err := r.check(ctx, t)
	return n, true, err
}

func (r *Rehasher) check(ctx context.Context, t rehashTarget) (int64, error) {
	if t.sha == "" {
		r.unavailable.Add(1)
		r.advance(ctx, t)
		return 0, nil
	}
	info, err := os.Stat(t.path)
	if err != nil {
		r.advance(ctx, t) // gone: the chain's own checks judge that
		return 0, nil
	}
	if t.size >= 0 && info.Size() != t.size {
		// Not the file that was sealed (a legacy rewrite, a truncation):
		// the chain's own checks judge it; a checksum says nothing here.
		r.unavailable.Add(1)
		r.advance(ctx, t)
		return 0, nil
	}
	sum, n, err := r.hashFile(ctx, t.path)
	if err != nil {
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		r.advance(ctx, t)
		return n, nil
	}
	after, err := os.Stat(t.path)
	if err != nil || !binlog.StampOf(after).Equal(binlog.StampOf(info)) {
		r.advance(ctx, t) // changed while read: judged next cycle
		return n, nil
	}
	r.segments.Add(1)
	res := rehashResult{stamp: binlog.StampOf(info), ok: sum == t.sha}
	if !res.ok {
		r.mismatches.Add(1)
		res.evidence = "sha256 recorded=" + t.sha + " local=" + sum
	}
	if r.e.setRehash(t.taskID, t.name, res) {
		r.e.MarkDirty(t.taskID, ReasonRehash)
	}
	r.advance(ctx, t)
	return n, nil
}

func (r *Rehasher) hashFile(ctx context.Context, path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	defer dropPageCache(f)
	h := sha256.New()
	buf := make([]byte, r.chunk)
	var total int64
	for {
		n, err := io.ReadFull(f, buf)
		if n > 0 {
			if werr := r.limiter.WaitN(ctx, n); werr != nil {
				return "", total, werr
			}
		}
		if n > 0 {
			h.Write(buf[:n])
			total += int64(n)
			r.bytes.Add(int64(n))
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return hex.EncodeToString(h.Sum(nil)), total, nil
		}
		if err != nil {
			return "", total, err
		}
	}
}

// Run re-checks segments until ctx ends, on one goroutine at idle I/O
// priority where the OS allows it.
func (r *Rehasher) Run(ctx context.Context) {
	if r.e.cfg.Mode == EvaluatorOff {
		return
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	setIdleIOPriority()
	for ctx.Err() == nil {
		_, did, err := r.step(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("rehash: %v", err)
		}
		if !did {
			t := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}
}
