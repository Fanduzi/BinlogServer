// Package app provides module-level functionality for app.
// input: a standalone process with a short verify.every_sec and a HEAD/GET object client, a cluster process with the same client, an upload client that cannot HEAD, and fake clients that fail HEAD with an auth error, a missing bucket, or a network error
// output: proof that the running worker reports a finding on the first pass and another on the ticker pass, that cluster mode does not read, open a finding or write .verify.json, that a startup HEAD failure is WARN detail on /api/health while status stays ok, and that startVerifier stores upload.bucket on the verifier
// pos: process-level checks for the ADR 0009 phase 3 worker (docs/design/0009-phase3-verifier.md §10, production-readiness A4)
// note: if this file changes, update this header and module README.md.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"binlog_server/internal/config"
	"binlog_server/internal/replication"
	"binlog_server/internal/taskrecords"
	"binlog_server/internal/tasks"
	"binlog_server/internal/upload"
)

type countingObjects struct {
	mu         sync.Mutex
	heads      int
	opens      int
	body       []byte
	missingKey string
}

func (c *countingObjects) UploadFile(context.Context, string, string, string) error { return nil }
func (c *countingObjects) DeleteObject(context.Context, string) error               { return nil }
func (c *countingObjects) HeadObject(_ context.Context, key string) (int64, error) {
	c.mu.Lock()
	c.heads++
	c.mu.Unlock()
	if key == c.missingKey || c.body == nil {
		return 0, os.ErrNotExist
	}
	return int64(len(c.body)), nil
}
func (c *countingObjects) OpenObject(_ context.Context, key string) (io.ReadCloser, int64, error) {
	c.mu.Lock()
	c.opens++
	c.mu.Unlock()
	if key == c.missingKey || c.body == nil {
		return nil, 0, os.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(string(c.body))), int64(len(c.body)), nil
}

func (c *countingObjects) counts() (heads, opens int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.heads, c.opens
}

// TestApp_VerifierTickerReportsTwoPasses starts standalone. The first pass
// sees the segment that was already on disk. The ticker pass sees the one
// written after that finding is visible.
func TestApp_VerifierTickerReportsTwoPasses(t *testing.T) {
	data := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "task.key")
	key, err := taskrecords.GenerateKeyFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	const id = "21"
	taskDir := filepath.Join(data, id)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	first, second := "mysql-bin.000001", "mysql-bin.000002"
	// Magic plus a zero event size does not parse, so VERIFY_FAILED names the
	// file. Bytes without the magic scan as an empty log and open nothing.
	bad := make([]byte, 64)
	copy(bad[:4], []byte{0xfe, 'b', 'i', 'n'})
	if err := os.WriteFile(filepath.Join(taskDir, first), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	plain, _ := json.Marshal(map[string]any{
		"seq": 21, "saved_at": time.Now().UTC(),
		"tasks": []map[string]any{{
			"id": id, "name": "verify", "cluster_key": "verify-key",
			"source":      map[string]any{"host": "127.0.0.1", "port": 3306, "user": "repl", "password": "secret"},
			"desired_run": "STOP",
		}},
	})
	if err := taskrecords.Open(data, key, nil).Save(plain); err != nil {
		t.Fatal(err)
	}

	objs := &countingObjects{}
	restoreUpload := newUploadClientForRun
	newUploadClientForRun = func(upload.S3Config) (uploadClient, error) { return objs, nil }
	t.Cleanup(func() { newUploadClientForRun = restoreUpload })
	restoreRunner := newRunnerForRun
	newRunnerForRun = func(config.Config, ...replication.RunnerOption) tasks.Runner { return &appFakeRunner{} }
	t.Cleanup(func() { newRunnerForRun = restoreRunner })

	a := New(config.Config{
		DataDir: data, Mode: "standalone", ListenAddr: "127.0.0.1:0",
		UploadEndpoint: "127.0.0.1:9000", UploadBucket: "binlogs", UploadAccessKey: "key", UploadSecretKey: "secret",
		Standalone: config.StandaloneConfig{TaskRecords: "on", TaskKeyFile: keyFile},
		Verify:     config.VerifyConfig{EverySec: 1, BudgetBytesPerSec: -1},
	})
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- a.Run(ctx) }()
	defer func() {
		cancel()
		waitRunExit(t, errCh)
	}()
	waitReady(t, a)
	base := "http://" + a.Addr()

	waitTaskFinding(t, base, id, first, true)
	if findingSubject(t, base, id, second) {
		t.Fatal("second segment was reported before it existed")
	}
	if err := os.WriteFile(filepath.Join(taskDir, second), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	waitTaskFinding(t, base, id, second, true)
}

func waitTaskFinding(t *testing.T, base, id, subject string, want bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = findingBody(t, base, id)
		if strings.Contains(last, subject) == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("subject %s present=%v\n%s", subject, !want, last)
}

func findingSubject(t *testing.T, base, id, subject string) bool {
	t.Helper()
	return strings.Contains(findingBody(t, base, id), subject)
}

func findingBody(t *testing.T, base, id string) string {
	t.Helper()
	resp, err := http.Get(base + "/api/tasks/" + id + "/findings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("findings status %d %s", resp.StatusCode, body)
	}
	return string(body)
}

type probeStore struct {
	mu    sync.Mutex
	lists int
	rows  map[string][]tasks.BinlogFile
}

func (p *probeStore) UpsertBinlogFile(context.Context, tasks.BinlogFile) error { return nil }
func (p *probeStore) ListBinlogFiles(_ context.Context, id string, _ int) ([]tasks.BinlogFile, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lists++
	out := append([]tasks.BinlogFile(nil), p.rows[id]...)
	return out, nil
}
func (p *probeStore) listCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lists
}

type probeObjects struct {
	mu    sync.Mutex
	heads int
	opens int
}

func (p *probeObjects) HeadObject(context.Context, string) (int64, error) {
	p.mu.Lock()
	p.heads++
	p.mu.Unlock()
	return 0, os.ErrNotExist
}
func (p *probeObjects) OpenObject(context.Context, string) (io.ReadCloser, int64, error) {
	p.mu.Lock()
	p.opens++
	p.mu.Unlock()
	return nil, 0, os.ErrNotExist
}
func (p *probeObjects) counts() (heads, opens int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.heads, p.opens
}

// TestStartVerifier_ClusterIsInert is the cluster Allows=false pin. The
// standalone subtest is the same wiring with Allows left unset, so a harness
// that never starts the worker fails there instead of passing here.
func TestStartVerifier_ClusterIsInert(t *testing.T) {
	t.Run("cluster", func(t *testing.T) {
		lists, heads, opens, findings, wrote := runVerifierProbe(t, "cluster")
		if lists != 0 || heads != 0 || opens != 0 || findings != 0 || wrote {
			t.Fatalf("cluster verified: lists=%d heads=%d opens=%d findings=%d wrote=%v", lists, heads, opens, findings, wrote)
		}
	})
	t.Run("standalone", func(t *testing.T) {
		lists, heads, opens, findings, wrote := runVerifierProbe(t, "standalone")
		if lists == 0 || heads == 0 || findings == 0 || !wrote {
			t.Fatalf("standalone did not verify: lists=%d heads=%d opens=%d findings=%d wrote=%v", lists, heads, opens, findings, wrote)
		}
	})
}

func runVerifierProbe(t *testing.T, mode string) (lists, heads, opens, findings int, wrote bool) {
	t.Helper()
	dir := t.TempDir()
	store := &probeStore{rows: map[string][]tasks.BinlogFile{}}
	objs := &probeObjects{}
	s := tasks.NewScheduler(tasks.WithDataDir(dir), tasks.WithFileStore(store), tasks.WithFindingStore(tasks.NewFileFindingStore(filepath.Join(dir, tasks.FindingFileName))))
	task, err := s.CreateTask("probe", "probe-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, task.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	name := "mysql-bin.000001"
	store.rows[task.ID] = []tasks.BinlogFile{{
		TaskID: task.ID, FileName: name, FilePath: filepath.Join(dir, task.ID, name),
		State: "SEALED", SizeBytes: 32, UploadState: "UPLOADED", ObjectKey: "k/" + name,
		SealedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}}
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startVerifier(ctx, config.Config{
		Mode: mode, DataDir: dir,
		Verify: config.VerifyConfig{EverySec: 1, BudgetBytesPerSec: -1},
	}, s, objs, func(f func()) {
		go func() {
			close(started)
			f()
		}()
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("verifier was not started")
	}
	time.Sleep(1300 * time.Millisecond)
	cancel()
	heads, opens = objs.counts()
	open, err := s.OpenFindings(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, tasks.VerifyStateFileName)); err == nil {
		wrote = true
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return store.listCount(), heads, opens, len(open), wrote
}

type headlessUpload struct{}

func (headlessUpload) UploadFile(context.Context, string, string, string) error { return nil }
func (headlessUpload) DeleteObject(context.Context, string) error               { return nil }
func (headlessUpload) OpenObject(context.Context, string) (io.ReadCloser, int64, error) {
	return io.NopCloser(strings.NewReader("")), 0, nil
}

func TestApp_UploadClientMustHeadAndGet(t *testing.T) {
	restore := newUploadClientForRun
	newUploadClientForRun = func(upload.S3Config) (uploadClient, error) { return headlessUpload{}, nil }
	t.Cleanup(func() { newUploadClientForRun = restore })
	status, detail := runProbe(t, nil)
	if status != "ok" || !strings.Contains(strings.Join(detail, " "), "HEAD") {
		t.Fatalf("status %s detail %v", status, detail)
	}
}

type probeUpload struct{ err error }

func (p probeUpload) UploadFile(context.Context, string, string, string) error { return nil }
func (p probeUpload) DeleteObject(context.Context, string) error               { return nil }
func (p probeUpload) OpenObject(context.Context, string) (io.ReadCloser, int64, error) {
	return io.NopCloser(strings.NewReader("")), 0, nil
}
func (p probeUpload) HeadObject(context.Context, string) (int64, error) { return 0, p.err }

func TestApp_ObjectProbeStaysHealthy(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "auth", err: errors.New("AccessDenied"), want: "AccessDenied"},
		{name: "missing-bucket", err: errors.New("NoSuchBucket"), want: "NoSuchBucket"},
		{name: "network", err: errors.New("connection refused"), want: "connection refused"},
		{name: "missing-key", err: os.ErrNotExist, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.err
			status, detail := runProbe(t, &err)
			joined := strings.Join(detail, " ")
			if status != "ok" {
				t.Fatalf("status %s body detail %v", status, detail)
			}
			if tc.want == "" {
				if joined != "" {
					t.Fatalf("missing key set detail %q", joined)
				}
				return
			}
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("detail %q does not contain %s", joined, tc.want)
			}
		})
	}
}

// runProbe starts one process. headErr nil installs a client with no HeadObject.
// A non-nil pointer installs a client whose HeadObject returns that error.
func runProbe(t *testing.T, headErr *error) (status string, detail []string) {
	t.Helper()
	restore := newUploadClientForRun
	newUploadClientForRun = func(upload.S3Config) (uploadClient, error) {
		if headErr == nil {
			return headlessUpload{}, nil
		}
		return probeUpload{err: *headErr}, nil
	}
	t.Cleanup(func() { newUploadClientForRun = restore })
	data := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "task.key")
	if _, err := taskrecords.GenerateKeyFile(keyFile); err != nil {
		t.Fatal(err)
	}
	a := New(config.Config{
		DataDir: data, ListenAddr: "127.0.0.1:0",
		UploadEndpoint: "127.0.0.1:9000", UploadBucket: "binlogs", UploadAccessKey: "key", UploadSecretKey: "secret",
		Standalone: config.StandaloneConfig{TaskRecords: "on", TaskKeyFile: keyFile},
	})
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- a.Run(ctx) }()
	defer func() {
		cancel()
		waitRunExit(t, errCh)
	}()
	waitReady(t, a)
	resp, err := http.Get("http://" + a.Addr() + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health %d", resp.StatusCode)
	}
	var body struct {
		Status string   `json:"status"`
		Detail []string `json:"detail"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Status, body.Detail
}

// TestStartVerifier_PassesUploadBucket is N16. upload.bucket reaches the
// verifier. The spawn does not start the pass loop.
func TestStartVerifier_PassesUploadBucket(t *testing.T) {
	dir := t.TempDir()
	s := tasks.NewScheduler(tasks.WithDataDir(dir))
	cfg := config.Config{DataDir: dir, UploadBucket: "binlogs"}
	startVerifier(context.Background(), cfg, s, nil, func(func()) {})
	if got := s.VerifierObjectBucket(); got != "binlogs" {
		t.Fatalf("upload bucket %q", got)
	}
}
