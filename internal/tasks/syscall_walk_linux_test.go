// Package tasks provides module-level functionality for tasks.
// input: a task directory of 150 and 1500 segments, and strace on PATH
// output: a failure when the cold evaluator pass's filesystem syscalls or the bytes returned by getdents64, read, pread64 and readv exceed the per-segment bound or 10.5 times the 150-segment count; missing strace fails the test
// pos: the kernel count for one cold walk, including preamble-only opens, the open tail, and work an AfterFunc scheduled during the pass; a directory reread the syscall count still accepts fails on the byte total
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
	"runtime"
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

	// Measured cold pass: 23140 syscalls and 150467 bytes for 150 segments,
	// 231117 syscalls and 1500475 bytes for 1500. About 154 syscalls and
	// 1003 bytes per segment. 231 and 1506 are 1.5 times those slopes,
	// and 16 and 4096 are the fixed slack. A directory listing a constant
	// number of times stays linear. Listing again every few segments
	// grows with the directory, so the 1500/150 byte ratio (at most
	// 10.5x) rejects it even when the syscall ratio still fits.
	syscallPerSeg   = 231
	syscallOverhead = 16
	bytePerSeg      = 1506
	byteOverhead    = 4096
	syscallSmallN   = 150
	syscallLargeN   = 1500
)

// walkByteSyscalls are the calls whose positive return value is bytes the
// kernel copied. A directory reread grows this sum with the directory.
var walkByteSyscalls = map[string]bool{
	"getdents64": true,
	"read":       true,
	"pread64":    true,
	"readv":      true,
}

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
	t.Logf("%d segments %d syscalls %d bytes; %d segments %d syscalls %d bytes", syscallSmallN, small.n, small.bytes, syscallLargeN, large.n, large.bytes)
	checkCalls := func(segs int, got walkCount) {
		t.Helper()
		if got.n < segs {
			t.Fatalf("%d-segment pass did %d filesystem syscalls, want at least one per segment", segs, got.n)
		}
		if got.n > segs*syscallPerSeg+syscallOverhead {
			t.Fatalf("%d-segment pass did %d filesystem syscalls, want <= %d per segment plus %d\n%s", segs, got.n, syscallPerSeg, syscallOverhead, got.detail)
		}
	}
	checkBytes := func(segs int, got walkCount) {
		t.Helper()
		if got.bytes < segs {
			t.Fatalf("%d-segment pass returned %d filesystem bytes, want at least one per segment", segs, got.bytes)
		}
		if got.bytes > segs*bytePerSeg+byteOverhead {
			t.Fatalf("%d-segment pass returned %d bytes from getdents64/read/pread64/readv, want <= %d per segment plus %d\n%s", segs, got.bytes, bytePerSeg, byteOverhead, got.detail)
		}
	}
	checkCalls(syscallSmallN, small)
	if small.n <= 0 || large.n*2 > small.n*21 {
		t.Fatalf("%d segments did %d syscalls, %d did %d; want at most 10.5x\n%s\n%s", syscallLargeN, large.n, syscallSmallN, small.n, small.detail, large.detail)
	}
	checkCalls(syscallLargeN, large)
	checkBytes(syscallSmallN, small)
	if small.bytes <= 0 || large.bytes*2 > small.bytes*21 {
		t.Fatalf("%d segments returned %d bytes, %d returned %d; want at most 10.5x\n%s\n%s", syscallLargeN, large.bytes, syscallSmallN, small.bytes, small.detail, large.detail)
	}
	checkBytes(syscallLargeN, large)
}

type walkCount struct {
	n, bytes int
	detail   string
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
	// -ttt stamps seconds since the epoch so a line from another thread
	// sorts back between the sentinels, including a pass that crosses
	// midnight. -s 80 keeps the return byte count on the line.
	cmd := exec.CommandContext(ctx, "strace", "-f", "-ttt", "-s", "80", "-e", "trace=%file,%desc,getdents64", "-o", logPath, "--", exe, "-syscall-walk", strconv.Itoa(n))
	cmd.Env = envWithoutMeta()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("syscall walk of %d segments: %v\n%s", n, err, out)
	}
	text, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := countWalkSyscalls(string(text))
	if err != nil {
		t.Fatalf("%v\n%s", err, straceSample(string(text)))
	}
	if tr.n < n {
		t.Fatalf("%d-segment trace counted %d filesystem syscalls\n%s", n, tr.n, straceSample(string(text)))
	}
	got := walkCount{n: tr.n, bytes: tr.bytes, detail: formatSyscallCounts(tr.counts) + " bytes " + formatSyscallCounts(tr.byteCounts)}
	t.Logf("%d segments %d syscalls %d bytes %s", n, got.n, got.bytes, got.detail)
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
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("mysql-bin.%06d", i)
		next := fmt.Sprintf("mysql-bin.%06d", i+1)
		if err := os.WriteFile(filepath.Join(dir, name), syscallWalkSegment(int64(37+i-1), next), 0o644); err != nil {
			return err
		}
		// An earlier open file with only a log_pos 0 rotate is the
		// preamble path (PreambleOnly). The last open file holds a real
		// event, so the open-tail path runs on it.
		var open []byte
		if i < n {
			open = syscallWalkPreamble(next)
		} else {
			open = syscallWalkSegment(int64(37+n), next)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".open.e1"), open, 0o644); err != nil {
			return err
		}
	}
	e := NewEvaluator(s, EvaluatorConfig{
		Store:  healthstore.NewFileStore(filepath.Join(data, healthstore.HealthFileName)),
		NodeID: evalNode, Instance: "i1", Sweep: time.Minute, Debounce: time.Millisecond,
	})
	// Keep the two sentinel unlinks on this OS thread. The walk itself may
	// still run on others; -ttt puts those lines back in order.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	quiet := runtime.NumGoroutine()
	_ = syscall.Unlink(walkSentinelStart)
	_, err = e.EvaluateNow(context.Background(), task.ID)
	// A pass can arm time.AfterFunc and return before that callback reads
	// the directory. Wait until those goroutines have finished, then mark
	// the end, so the callback's syscalls stay inside the window.
	waitWalkQuiescent(quiet)
	_ = syscall.Unlink(walkSentinelEnd)
	return err
}

// waitWalkQuiescent gives a callback scheduled as the pass returns time to
// start, then waits until the goroutine count has been back at the
// pre-pass level. The cap is the whole strace budget's problem if a
// callback never returns.
func waitWalkQuiescent(base int) {
	deadline := time.Now().Add(30 * time.Second)
	start := time.Now()
	var quiet time.Time
	for time.Now().Before(deadline) {
		settled := runtime.NumGoroutine() <= base && time.Since(start) >= 50*time.Millisecond
		if settled {
			if quiet.IsZero() {
				quiet = time.Now()
			}
			if time.Since(quiet) >= 20*time.Millisecond {
				return
			}
		} else {
			quiet = time.Time{}
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func syscallWalkSegment(seq int64, next string) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0xfe, 'b', 'i', 'n'})
	sid, err := hex.DecodeString(strings.ReplaceAll(gtidPITRUUID, "-", ""))
	if err != nil {
		panic(err)
	}
	payload := make([]byte, 1+goreplication.SidLength+8)
	copy(payload[1:], sid)
	binary.LittleEndian.PutUint64(payload[1+goreplication.SidLength:], uint64(seq))
	syscallWalkEvent(&buf, goreplication.GTID_EVENT, payload, 128)
	rot := make([]byte, 8+len(next))
	binary.LittleEndian.PutUint64(rot[:8], 4)
	copy(rot[8:], next)
	syscallWalkEvent(&buf, goreplication.ROTATE_EVENT, rot, 256)
	return buf.Bytes()
}

// syscallWalkPreamble is an open file of one log_pos 0 rotate: big enough
// to pass the empty-leftover size check, with no GTID event, so the chain
// calls PreambleOnly.
func syscallWalkPreamble(next string) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0xfe, 'b', 'i', 'n'})
	rot := make([]byte, 8+len(next))
	copy(rot[8:], next)
	syscallWalkEvent(&buf, goreplication.ROTATE_EVENT, rot, 0)
	return buf.Bytes()
}

func syscallWalkEvent(buf *bytes.Buffer, kind goreplication.EventType, payload []byte, logPos uint32) {
	size := uint32(goreplication.EventHeaderSize + len(payload))
	var hdr [19]byte
	hdr[4] = byte(kind)
	binary.LittleEndian.PutUint32(hdr[9:13], size)
	binary.LittleEndian.PutUint32(hdr[13:17], logPos)
	buf.Write(hdr[:])
	buf.Write(payload)
}

type walkTrace struct {
	n, bytes   int
	counts     map[string]int
	byteCounts map[string]int
}

func countWalkSyscalls(text string) (walkTrace, error) {
	lines := straceLinesInOrder(text)
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
		return walkTrace{}, fmt.Errorf("sentinel window missing (start=%d end=%d lines=%d)", start, end, len(lines))
	}
	tr := walkTrace{counts: map[string]int{}, byteCounts: map[string]int{}}
	for _, line := range lines[start+1 : end] {
		name, ok := walkSyscallName(line)
		if ok && !walkSyscallSkip[name] {
			tr.counts[name]++
			tr.n++
		}
		bname, nbytes, bok := walkReturnBytes(line)
		if bok {
			tr.byteCounts[bname] += nbytes
			tr.bytes += nbytes
		}
	}
	return tr, nil
}

// straceLinesInOrder sorts timestamped lines. strace -f does not promise
// that a line is printed before a later syscall on another thread.
// Timestamps are seconds since the epoch (-ttt), so a pass that crosses
// midnight stays in order.
func straceLinesInOrder(text string) []string {
	lines := strings.Split(text, "\n")
	type stamped struct {
		ts, line string
		i        int
	}
	items := make([]stamped, len(lines))
	saw := false
	for i, line := range lines {
		ts, _ := stracePrefix(line)
		if ts != "" {
			saw = true
		}
		items[i] = stamped{ts: ts, line: line, i: i}
	}
	if !saw {
		return lines
	}
	sort.SliceStable(items, func(a, b int) bool {
		if items[a].ts == items[b].ts {
			return items[a].i < items[b].i
		}
		if items[a].ts == "" {
			return false
		}
		if items[b].ts == "" {
			return true
		}
		return items[a].ts < items[b].ts
	})
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.line
	}
	return out
}

func stracePrefix(line string) (ts, rest string) {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "[pid ") {
		i := strings.Index(line, "] ")
		if i < 0 {
			return "", line
		}
		line = strings.TrimSpace(line[i+2:])
	}
	if i := strings.IndexByte(line, ' '); i > 0 && digitsOnly(line[:i]) {
		line = strings.TrimSpace(line[i+1:])
	}
	if sp := strings.IndexByte(line, ' '); sp > 0 && isStraceTimestamp(line[:sp]) {
		return line[:sp], strings.TrimSpace(line[sp+1:])
	}
	return "", line
}

func isStraceTimestamp(s string) bool {
	// -ttt: seconds since the epoch, a dot, then microseconds.
	dot := strings.IndexByte(s, '.')
	if dot <= 0 || dot == len(s)-1 || strings.IndexByte(s[dot+1:], '.') >= 0 {
		return false
	}
	return digitsOnly(s[:dot]) && digitsOnly(s[dot+1:])
}

// walkReturnBytes is the positive return count of a byte-copying call.
// A resumed line carries the count when the first line was unfinished.
func walkReturnBytes(line string) (string, int, bool) {
	_, rest := stracePrefix(line)
	name := ""
	if strings.HasPrefix(rest, "<...") {
		end := strings.Index(rest, " resumed>")
		if end < 0 {
			return "", 0, false
		}
		name = strings.TrimSpace(rest[len("<..."):end])
	} else if n, ok := walkSyscallName(line); ok {
		name = n
	}
	if !walkByteSyscalls[name] {
		return "", 0, false
	}
	n, ok := straceReturn(rest)
	if !ok {
		return "", 0, false
	}
	return name, n, true
}

func straceReturn(rest string) (int, bool) {
	i := strings.LastIndex(rest, " = ")
	if i < 0 {
		return 0, false
	}
	field := strings.TrimSpace(rest[i+3:])
	if sp := strings.IndexAny(field, " <"); sp >= 0 {
		field = field[:sp]
	}
	n, err := strconv.Atoi(field)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func walkSyscallName(line string) (string, bool) {
	_, rest := stracePrefix(line)
	if rest == "" || strings.Contains(rest, "+++") || strings.Contains(rest, "resumed>") {
		return "", false
	}
	if strings.HasPrefix(rest, "---") || strings.HasPrefix(rest, "<...") {
		return "", false
	}
	paren := strings.IndexByte(rest, '(')
	if paren <= 0 {
		return "", false
	}
	name := rest[:paren]
	for _, c := range name {
		if c != '_' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return "", false
		}
	}
	return name, true
}

func straceSample(text string) string {
	lines := strings.Split(text, "\n")
	if len(lines) > 24 {
		lines = append(lines[:12], append([]string{"..."}, lines[len(lines)-12:]...)...)
	}
	return strings.Join(lines, "\n")
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
		`9 pread64(3, "y", 8, 0) = 7`,
		`9 readv(3, [], 2) = 9`,
		`9 read(4, "z", 1) = -1 EIO (Input/output error)`,
		`9 +++ exited with 0 +++`,
		`12 unlinkat(AT_FDCWD, "/tmp/binlog-walk-sentinel-end", 0) = -1 ENOENT (No such file or directory)`,
	}, "\n")
	tr, err := countWalkSyscalls(log)
	if err != nil {
		t.Fatal(err)
	}
	if tr.n != 6 || tr.counts["openat"] != 1 || tr.counts["getdents64"] != 1 || tr.counts["read"] != 2 || tr.counts["pread64"] != 1 || tr.counts["readv"] != 1 || tr.counts["mmap"] != 0 {
		t.Fatalf("total %d counts %v", tr.n, tr.counts)
	}
	// 100 getdents + 4 resumed read + 1 read + 7 pread64 + 9 readv. The EIO read adds a call and no bytes.
	if tr.bytes != 121 || tr.byteCounts["getdents64"] != 100 || tr.byteCounts["read"] != 5 || tr.byteCounts["pread64"] != 7 || tr.byteCounts["readv"] != 9 {
		t.Fatalf("bytes %d by call %v", tr.bytes, tr.byteCounts)
	}
	// Printed end-before-start. Epoch seconds still put the window in order,
	// which a HH:MM:SS stamp would not do across midnight.
	reordered := strings.Join([]string{
		`9 1700000000.300000 unlinkat(AT_FDCWD, "/tmp/binlog-walk-sentinel-end", 0) = -1 ENOENT`,
		`9 1700000000.200000 openat(AT_FDCWD, "a", O_RDONLY) = 3`,
		`[pid 10] 1699999999.900000 unlinkat(AT_FDCWD, "/tmp/binlog-walk-sentinel-start", 0) = -1 ENOENT`,
		`[pid 10] 1700000000.250000 getdents64(3, [], 8192) = 10`,
	}, "\n")
	tr, err = countWalkSyscalls(reordered)
	if err != nil {
		t.Fatal(err)
	}
	if tr.n != 2 || tr.counts["openat"] != 1 || tr.counts["getdents64"] != 1 || tr.bytes != 10 {
		t.Fatalf("reordered total %d bytes %d counts %v", tr.n, tr.bytes, tr.counts)
	}
}
