// Package healthstore provides module-level functionality for healthstore.
// input: transitions and health of standalone tasks
// output: FileStore: {data_dir}/.health.json written atomically (temp file, fsync, rename); standalone fence only; pending transitions saved in the same file
// pos: standalone (no meta DSN) Store
// note: if this file changes, update this header and module README.md.
package healthstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"binlog_server/internal/health"
)

// HealthFileName is the standalone health file under data_dir. It starts
// with a dot so the on-disk backup listing never treats it as a task.
const HealthFileName = ".health.json"

// maxFileTransitions bounds the transition log kept in the file; older
// rows are dropped (their digests with them).
const maxFileTransitions = 20000

type fileFinding struct {
	health.Finding
	Count      int64      `json:"count"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

type fileHealth struct {
	health.TaskHealth
	Generation int64  `json:"generation"`
	Stored     Stored `json:"fence"`
}

type fileState struct {
	Findings    []fileFinding                  `json:"findings"`
	Health      map[string]fileHealth          `json:"health"`
	Transitions []Transition                   `json:"transitions"`
	Pending     map[string][]health.Transition `json:"pending,omitempty"`
}

// FileStore is the standalone Store.
type FileStore struct {
	mu   sync.Mutex
	path string
}

var _ Store = (*FileStore)(nil)

// NewFileStore stores health in path.
func NewFileStore(path string) *FileStore { return &FileStore{path: path} }

func (s *FileStore) load() (*fileState, error) {
	st := &fileState{Health: map[string]fileHealth{}, Pending: map[string][]health.Transition{}}
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	if st.Health == nil {
		st.Health = map[string]fileHealth{}
	}
	if st.Pending == nil {
		st.Pending = map[string][]health.Transition{}
	}
	return st, nil
}

func (s *FileStore) save(st *fileState) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".health-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// ApplyTransitions implements Store for the standalone fence.
func (s *FileStore) ApplyTransitions(ctx context.Context, taskID string, fence health.Fence, ts []health.Transition, h health.TaskHealth) (Applied, error) {
	if fence.Mode != health.FenceStandalone {
		return Applied{}, ErrModeUnsupported
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return Applied{}, err
	}
	rec, ok := st.Health[taskID]
	var stored *Stored
	if ok {
		stored = &rec.Stored
	}
	if err := CheckFence(fence, nil, stored); err != nil {
		return Applied{}, err
	}
	digests := map[string]bool{}
	for _, t := range st.Transitions {
		if t.TaskID == taskID {
			digests[t.Digest] = true
		}
	}
	index := map[health.Key]int{}
	for i, f := range st.Findings {
		index[f.Key()] = i
	}
	now := time.Now().UTC()
	out := Applied{Generations: make([]int64, len(ts))}
	gen := rec.Generation
	for i, t := range ts {
		d := health.TransitionDigest(taskID, t)
		if digests[d] {
			continue
		}
		digests[d] = true
		gen++
		out.Generations[i] = gen
		fact := t.FactAt
		if fact.IsZero() {
			fact = now
		}
		st.Transitions = append(st.Transitions, Transition{TaskID: taskID, Kind: string(t.Kind), Subject: t.Subject, Op: string(t.Op),
			Reason: t.Reason, Severity: string(t.Instance.Severity), Digest: d, Generation: gen, KindVersion: t.KindVersion, FactAt: fact})
		key := health.Key{TaskID: taskID, Kind: t.Kind, Subject: t.Subject}
		j, exists := index[key]
		if t.Op == health.OpClose {
			if exists && st.Findings[j].ResolvedAt == nil {
				at := now
				st.Findings[j].ResolvedAt = &at
				st.Findings[j].Generation = gen
			}
			continue
		}
		if !exists {
			st.Findings = append(st.Findings, fileFinding{Finding: health.Finding{TaskID: taskID, Kind: t.Kind, Subject: t.Subject, FirstSeen: now}})
			j = len(st.Findings) - 1
			index[key] = j
		}
		f := &st.Findings[j]
		f.Severity, f.Detail, f.Evidence = t.Instance.Severity, t.Instance.Detail, t.Instance.Evidence
		f.Segments = append([]string(nil), t.Instance.Segments...)
		f.LastSeen, f.Generation, f.ResolvedAt = now, gen, nil
		f.Count++
	}
	if len(st.Transitions) > maxFileTransitions {
		st.Transitions = st.Transitions[len(st.Transitions)-maxFileTransitions:]
	}
	h.TaskID = taskID
	st.Health[taskID] = fileHealth{TaskHealth: h, Generation: gen, Stored: Stored{Mode: fence.Mode, Node: fence.Node, Instance: fence.Instance}}
	if err := s.save(st); err != nil {
		return Applied{}, err
	}
	return out, nil
}

// ListOpenFindingsForTask implements Store.
func (s *FileStore) ListOpenFindingsForTask(ctx context.Context, taskID string) ([]health.Finding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return nil, err
	}
	var out []health.Finding
	for _, f := range st.Findings {
		if f.TaskID == taskID && f.ResolvedAt == nil {
			out = append(out, f.Finding)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Subject < out[j].Subject
	})
	return out, nil
}

// ListHealth implements Store.
func (s *FileStore) ListHealth(ctx context.Context) ([]health.TaskHealth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]health.TaskHealth, 0, len(st.Health))
	for _, h := range st.Health {
		out = append(out, h.TaskHealth)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TaskID < out[j].TaskID })
	return out, nil
}

// ListTransitions implements TransitionLister.
func (s *FileStore) ListTransitions(ctx context.Context, taskID string) ([]Transition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return nil, err
	}
	var out []Transition
	for _, t := range st.Transitions {
		if t.TaskID == taskID {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Generation < out[j].Generation })
	return out, nil
}

// SavePending persists transitions not yet applied (an empty list clears
// them), so a standalone restart replays them (design §5.1 step 6).
func (s *FileStore) SavePending(taskID string, ts []health.Transition) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	if len(ts) == 0 {
		delete(st.Pending, taskID)
	} else {
		st.Pending[taskID] = ts
	}
	return s.save(st)
}

// LoadPending returns the saved pending transitions of every task.
func (s *FileStore) LoadPending() (map[string][]health.Transition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return nil, err
	}
	return st.Pending, nil
}
