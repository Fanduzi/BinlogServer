// Package tasks provides module-level functionality for tasks.
// input: a task directory of 150 and 1500 segments, and strace on PATH
// output: a failure when the cold evaluator pass's filesystem syscalls exceed the per-segment bound or 10.5 times the 150-segment count; missing strace fails the test
// pos: the kernel count for one cold walk, so a directory read the in-process counter does not see still fails
// note: if this file changes, update this header and module README.md.
//go:build linux

package tasks

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	goreplication "github.com/go-mysql-org/go-mysql/replication"

	"binlog_server/internal/healthstore"
)

func init() { syscallWalkChild = runSyscallWalk }

const (
	walkSentinelStart = "/tmp/binlog-walk-sentinel-start"
	walkSentinelEnd   = "/tmp/binlog-walk-sentinel-end"

	// Measured cold pass: 9479 syscalls for 150 segments and 94568 for 1500,
	// about 63 per segment. 95 is 1.5 times that, and 16 is the fixed slack.
	// A directory listing once per segment grows with the directory, so the
	// 1500/150 ratio (at most 10.5x) rejects it even when the per-segment
	// cap still fits.
	syscallPerSeg   = 95
	syscallOverhead = 16
	syscallSmallN   = 150
	syscallLargeN   = 1500
)

// walkSyscallSkip is memory and poll noise that strace's %desc class also
// prints. A filesystem call is everything else in the traced set.
var walkSyscallSkip = map[string]bool{
	"mmap": true, "mmap2": true, "munmap": true, "mremap": true, "mprotect": true,
	"brk": true, "madvise": true, "arch_prctl": true, "futex": true,
	"epoll_ctl": true, "epoll_pwait": true, "epoll_wait": true, "epoll_pwait2": true,
}

func TestEvaluator_SyscallWalkNearLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("strace of the cold walk")
	}
	if _, err := exec.LookPath("strace"); err != nil {
		t.Fatalf("strace is required for the cold-walk syscall count: %v", err)
	}
	small := countColdWalk(t, syscallSmallN)
	large := countColdWalk(t, syscallLargeN)
	t.Logf("%d segments %d filesystem syscalls; %d segments %d", syscallSmallN, small.n, syscallLargeN, large.n)
	check := func(segs int, got walkCount) {
		t.Helper()
		if got.n < segs {
			t.Fatalf("%d-segment pass did %d filesystem syscalls, want at least one per segment", segs, got.n)
		}
		if got.n > segs*syscallPerSeg+syscallOverhead {
			t.Fatalf("%d-segment pass did %d filesystem syscalls, want <= %d per segment plus %d\n%s", segs, got.n, syscallPerSeg, syscallOverhead, got.detail)
		}
	}
	check(syscallSmallN, small)
	if small.n <= 0 || large.n*2 > small.n*21 {
		t.Fatalf("%d segments did %d syscalls, %d did %d; want at most 10.5x\n%s\n%s", syscallLargeN, large.n, syscallSmallN, small.n, small.detail, large.detail)
	}
	check(syscallLargeN, large)
}

type walkCount struct {
	n      int
	detail string
}

func countColdWalk(t *testing.T, n int) walkCount {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "strace.log")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "strace", "-f", "-s", "80", "-e", "trace=%file,%desc,getdents64", "-o", logPath, "--", exe, "-syscall-walk", strconv.Itoa(n))
	cmd.Env = envWithoutMeta()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("syscall walk of %d segments: %v\n%s", n, err, out)
	}
	text, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	total, counts, err := countWalkSyscalls(string(text))
	if err != nil {
		t.Fatal(err)
	}
	got := walkCount{n: total, detail: formatSyscallCounts(counts)}
	t.Logf("%d segments %d syscalls %s", n, total, got.detail)
	return got
}

func envWithoutMeta() []string {
	var out []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "BINLOG_SERVER_META_DSN=") {
			continue
		}
		out = append(out, e)
	}
	return out
}

func runSyscallWalk(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: -syscall-walk N")
		os.Exit(2)
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n <= 0 {
		fmt.Fprintln(os.Stderr, "segment count:", args[0])
		os.Exit(2)
	}
	if err := syscallWalkPass(n); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func syscallWalkPass(n int) error {
	data, err := os.MkdirTemp("", "syscall-walk-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(data)
	s := NewScheduler(WithDataDir(data))
	task, err := s.CreateTaskFromSpec("mysql", "mysql-key-syscall", &SourceConfig{Host: "127.0.0.1", Port: 3306, User: "repl", Password: "secret", Flavor: "mysql"}, &StartConfig{Mode: StartModeGTID, GTIDSet: gtidPITRUUID + ":1-36"}, nil)
	if err != nil {
		return err
	}
	dir := filepath.Join(data, task.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		path := filepath.Join(dir, fmt.Sprintf("mysql-bin.%06d", i+1))
		if err := os.WriteFile(path, syscallWalkSegment(int64(37+i)), 0o644); err != nil {
			return err
		}
	}
	e := NewEvaluator(s, EvaluatorConfig{
		Mode: EvaluatorShadow, Store: healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName)),
		NodeID: evalNode, Instance: "i1", Sweep: time.Minute, Debounce: time.Millisecond,
	})
	_ = syscall.Unlink(walkSentinelStart)
	_, err = e.EvaluateNow(context.Background(), task.ID)
	_ = syscall.Unlink(walkSentinelEnd)
	return err
}

func syscallWalkSegment(seq int64) []byte {
	when, err := ParsePITRDatetime("2024-01-01 09:00:00")
	if err != nil {
		panic(err)
	}
	var buf bytes.Buffer
	buf.Write([]byte{0xfe, 'b', 'i', 'n'})
	sid, err := hex.DecodeString(strings.ReplaceAll(gtidPITRUUID, "-", ""))
	if err != nil {
		panic(err)
	}
	payload := make([]byte, 1+goreplication.SidLength+8)
	copy(payload[1:], sid)
	binary.LittleEndian.PutUint64(payload[1+goreplication.SidLength:], uint64(seq))
	writeWindowEvent(&buf, when, goreplication.GTID_EVENT, payload)
	return buf.Bytes()
}

func countWalkSyscalls(text string) (int, map[string]int, error) {
	lines := strings.Split(text, "\n")
	start, end := -1, -1
	for i, line := range lines {
		if start < 0 && strings.Contains(line, walkSentinelStart) {
			start = i
		}
		if strings.Contains(line, walkSentinelEnd) {
			end = i
		}
	}
	if start < 0 || end <= start {
		return 0, nil, fmt.Errorf("sentinel window missing (start=%d end=%d)", start, end)
	}
	counts := map[string]int{}
	total := 0
	for _, line := range lines[start+1 : end] {
		name, ok := walkSyscallName(line)
		if !ok || walkSyscallSkip[name] {
			continue
		}
		counts[name]++
		total++
	}
	return total, counts, nil
}

func walkSyscallName(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.Contains(line, "+++") || strings.Contains(line, "resumed>") {
		return "", false
	}
	if strings.HasPrefix(line, "[pid ") {
		i := strings.Index(line, "] ")
		if i < 0 {
			return "", false
		}
		line = line[i+2:]
	} else if i := strings.IndexByte(line, ' '); i > 0 && digitsOnly(line[:i]) {
		line = line[i+1:]
	}
	if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "<...") {
		return "", false
	}
	paren := strings.IndexByte(line, '(')
	if paren <= 0 {
		return "", false
	}
	name := line[:paren]
	for _, c := range name {
		if c != '_' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return "", false
		}
	}
	return name, true
}

func digitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func formatSyscallCounts(counts map[string]int) string {
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if counts[names[i]] == counts[names[j]] {
			return names[i] < names[j]
		}
		return counts[names[i]] > counts[names[j]]
	})
	var b strings.Builder
	for i, name := range names {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s=%d", name, counts[name])
	}
	return b.String()
}

func TestCountWalkSyscalls_Window(t *testing.T) {
	log := strings.Join([]string{
		`12 unlinkat(AT_FDCWD, "/tmp/binlog-walk-sentinel-start", 0) = -1 ENOENT (No such file or directory)`,
		`12 openat(AT_FDCWD, "a", O_RDONLY) = 3`,
		`12 mmap(NULL, 4096, PROT_READ, MAP_PRIVATE|MAP_ANONYMOUS, -1, 0) = 0x1`,
		`[pid 9] getdents64(3, [], 8192) = 100`,
		`[pid 9] <... read resumed> = 4`,
		`9 read(3, "x", 1) = 1`,
		`9 +++ exited with 0 +++`,
		`12 unlinkat(AT_FDCWD, "/tmp/binlog-walk-sentinel-end", 0) = -1 ENOENT (No such file or directory)`,
	}, "\n")
	total, counts, err := countWalkSyscalls(log)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || counts["openat"] != 1 || counts["getdents64"] != 1 || counts["read"] != 1 || counts["mmap"] != 0 {
		t.Fatalf("total %d counts %v", total, counts)
	}
}
