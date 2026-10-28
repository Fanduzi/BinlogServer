// Package tasks provides module-level functionality for tasks.
// input: the retained replay inventory, one ScanCover of each segment, the recovery-window breaks, and an optional full-backup GTID set
// output: GET /coverage for one task or one source, and POST /restore-plan (prechecks, manifest, restore.sh). Cluster mode returns ErrCoverageUnsupported. A limit is not applied
// pos: adapter for ADR 0009 phases 4 and 5; the decision is internal/assurance.Cover
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"binlog_server/internal/assurance"
	"binlog_server/internal/binlog"
)

var (
	// ErrCoverageUnsupported is the cluster answer while ADR 0011 PR-4 is unfinished.
	ErrCoverageUnsupported = errors.New("coverage is not supported in cluster mode yet")
	// ErrCoverageQuery means neither at nor gtid was set.
	ErrCoverageQuery = errors.New("at or gtid is required")
	// ErrCoverageExclusive means at and gtid were both set.
	ErrCoverageExclusive = errors.New("at and gtid cannot both be set")
	// ErrRestoreTarget means the plan did not name exactly one of at, gtid, or latest.
	ErrRestoreTarget = errors.New("set exactly one of at, gtid, or latest")
	// ErrInvalidCoverageAt is an at value ParsePITRDatetime rejects.
	ErrInvalidCoverageAt = errors.New("invalid at")
	// ErrInvalidCoverageGTID is a gtid that is not one MySQL uuid:seq.
	ErrInvalidCoverageGTID = errors.New("invalid gtid")
	// ErrCoverageGTIDFlavor is a gtid query on a flavor other than mysql.
	ErrCoverageGTIDFlavor = errors.New("gtid coverage is not supported for this flavor")
	// ErrInvalidCoverageSource is a source value that is not host:port.
	ErrInvalidCoverageSource = errors.New("invalid source")
)

// CoverageQuery is a point in time, one MySQL GTID, or the latest copied event.
type CoverageQuery struct {
	At     *time.Time
	GTID   string
	Latest bool
}

// TaskCoverage is one task's answer in a cross-task query.
type TaskCoverage struct {
	TaskID   string                `json:"task_id"`
	Coverage assurance.CoverResult `json:"coverage"`
}

// SourceCoverage is every task on one source.
type SourceCoverage struct {
	Source string         `json:"source"`
	Tasks  []TaskCoverage `json:"tasks"`
}

// RestoreTarget is the point the plan was built for.
type RestoreTarget struct {
	At     *time.Time `json:"at,omitempty"`
	GTID   string     `json:"gtid,omitempty"`
	Latest bool       `json:"latest,omitempty"`
}

// RestoreStep is one operator step. Kind is download, verify, or apply.
type RestoreStep struct {
	Kind    string   `json:"kind"`
	Files   []string `json:"files,omitempty"`
	Command string   `json:"command,omitempty"`
}

// RestoreFile is one manifest line.
type RestoreFile struct {
	Name      string `json:"name"`
	SHA256    string `json:"sha256,omitempty"`
	Size      int64  `json:"size,omitempty"`
	LocalPath string `json:"local_path,omitempty"`
	ObjectKey string `json:"object_key,omitempty"`
}

// RestoreBundle points at the existing replay archive. The plan does not
// add members to that tar.
type RestoreBundle struct {
	URL string `json:"url"`
}

// RestorePlan is the no-side-effect restore plan.
type RestorePlan struct {
	Target        RestoreTarget         `json:"target"`
	Coverage      assurance.CoverResult `json:"coverage"`
	Prerequisites []string              `json:"prerequisites"`
	Prechecks     []assurance.Precheck  `json:"prechecks"`
	Steps         []RestoreStep         `json:"steps"`
	Manifest      []RestoreFile         `json:"manifest"`
	RestoreSh     string                `json:"restore_sh"`
	Readme        string                `json:"readme"`
	Bundle        RestoreBundle         `json:"bundle"`
}

// Coverage reports which retained segments cover q.
func (s *Scheduler) Coverage(taskID string, q CoverageQuery) (assurance.CoverResult, error) {
	if err := s.coverageRefused(); err != nil {
		return assurance.CoverResult{}, err
	}
	if q.Latest || (q.At != nil && strings.TrimSpace(q.GTID) != "") {
		return assurance.CoverResult{}, ErrCoverageExclusive
	}
	if q.At == nil && strings.TrimSpace(q.GTID) == "" {
		return assurance.CoverResult{}, ErrCoverageQuery
	}
	task, aq, segs, breaks, err := s.coverChain(taskID, q)
	if err != nil {
		return assurance.CoverResult{}, err
	}
	_ = task
	return assurance.Cover(segs, aq, breaks)
}

// CoverageBySource reports coverage for every task on host:port.
func (s *Scheduler) CoverageBySource(host string, port int, q CoverageQuery) (SourceCoverage, error) {
	if err := s.coverageRefused(); err != nil {
		return SourceCoverage{}, err
	}
	host = strings.TrimSpace(host)
	if host == "" || port < 1 || port > 65535 {
		return SourceCoverage{}, ErrInvalidCoverageSource
	}
	out := SourceCoverage{Source: host + ":" + strconv.Itoa(port), Tasks: []TaskCoverage{}}
	for _, task := range s.ListTasks() {
		if int(task.Source.Port) != port || !SameSourceHost(task.Source.Host, host) {
			continue
		}
		cov, err := s.Coverage(task.ID, q)
		if err != nil {
			return SourceCoverage{}, err
		}
		out.Tasks = append(out.Tasks, TaskCoverage{TaskID: task.ID, Coverage: cov})
	}
	return out, nil
}

// RestorePlan builds the plan for exactly one of at, gtid, or latest.
// fullBackup is the operator's full-backup @@gtid_executed, or empty.
func (s *Scheduler) RestorePlan(taskID string, q CoverageQuery, fullBackup string) (RestorePlan, error) {
	if err := s.coverageRefused(); err != nil {
		return RestorePlan{}, err
	}
	n := 0
	if q.At != nil {
		n++
	}
	if strings.TrimSpace(q.GTID) != "" {
		n++
	}
	if q.Latest {
		n++
	}
	if n != 1 {
		return RestorePlan{}, ErrRestoreTarget
	}
	task, aq, segs, breaks, err := s.coverChain(taskID, q)
	if err != nil {
		return RestorePlan{}, err
	}
	cov, err := assurance.Cover(segs, aq, breaks)
	if err != nil {
		return RestorePlan{}, err
	}
	return s.planFrom(task, q, segs, cov, fullBackup), nil
}

func (s *Scheduler) coverageRefused() error {
	if s != nil && s.clusterMode {
		return ErrCoverageUnsupported
	}
	return nil
}

func (s *Scheduler) coverChain(taskID string, q CoverageQuery) (Task, assurance.CoverQuery, []assurance.CoverSegment, []assurance.Break, error) {
	task, err := s.GetTask(taskID)
	if err != nil {
		return Task{}, assurance.CoverQuery{}, nil, nil, err
	}
	aq, err := normalizeCoverQuery(task, q)
	if err != nil {
		return Task{}, assurance.CoverQuery{}, nil, nil, err
	}
	segs, breaks, err := s.readCoverChain(task)
	if err != nil {
		return Task{}, assurance.CoverQuery{}, nil, nil, err
	}
	return task, aq, segs, breaks, nil
}

func normalizeCoverQuery(task Task, q CoverageQuery) (assurance.CoverQuery, error) {
	out := assurance.CoverQuery{At: q.At, Latest: q.Latest}
	if strings.TrimSpace(q.GTID) == "" {
		return out, nil
	}
	if !mysqlFlavor(task.Source.Flavor) {
		return assurance.CoverQuery{}, ErrCoverageGTIDFlavor
	}
	gtid, err := ParseMySQLStopGTID(q.GTID)
	if err != nil {
		return assurance.CoverQuery{}, ErrInvalidCoverageGTID
	}
	out.GTID = gtid.UUID + ":" + strconv.FormatInt(gtid.Seq, 10)
	return out, nil
}

func mysqlFlavor(flavor string) bool {
	f := strings.TrimSpace(flavor)
	return f == "" || strings.EqualFold(f, "mysql")
}

func (s *Scheduler) readCoverChain(task Task) ([]assurance.CoverSegment, []assurance.Break, error) {
	files, err := s.ListFiles(task.ID, segmentInventoryLimit)
	if err != nil {
		return nil, nil, err
	}
	chosen, _ := s.SelectReplayFilesFor(task, files)
	s.mu.Lock()
	objectStorage := s.fileUploader != nil
	s.mu.Unlock()
	segs := make([]assurance.CoverSegment, 0, len(chosen))
	windows := make([]assurance.WindowSegment, 0, len(chosen))
	for _, file := range chosen {
		seg, win, err := s.scanCoverFile(task.ID, file)
		if err != nil {
			return nil, nil, err
		}
		segs = append(segs, seg)
		windows = append(windows, win)
	}
	derived, err := assurance.WindowOf(windows, task.Source.Flavor, objectStorage, StartGTIDText(task))
	if err != nil {
		return nil, nil, err
	}
	return segs, derived.Breaks, nil
}

func (s *Scheduler) scanCoverFile(taskID string, file BinlogFile) (assurance.CoverSegment, assurance.WindowSegment, error) {
	name := segmentInventoryBasename(file)
	seg := assurance.CoverSegment{
		Name: name, Source: strings.TrimSpace(file.FileName), SourceIdentity: file.SourceIdentity,
		Epoch: file.Epoch, ObjectKey: file.ObjectKey, UploadState: file.UploadState,
		Checksum: file.Checksum, SHA256: strings.TrimSpace(file.SHA256), Size: file.SizeBytes,
		LastVerified: s.segmentLastVerified(taskID, name),
	}
	if file.Location != "bucket" {
		seg.LocalPath = file.FilePath
	}
	win := windowSegmentOf(file)
	if name == "" {
		return seg, win, nil
	}
	body, _, err := s.OpenTaskSegment(taskID, name)
	if err != nil {
		if errors.Is(err, ErrSegmentNotOnProcess) {
			return seg, win, nil
		}
		return assurance.CoverSegment{}, assurance.WindowSegment{}, err
	}
	raw, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil {
		return assurance.CoverSegment{}, assurance.WindowSegment{}, readErr
	}
	if closeErr != nil {
		return assurance.CoverSegment{}, assurance.WindowSegment{}, closeErr
	}
	sum := sha256.Sum256(raw)
	if seg.SHA256 == "" {
		seg.SHA256 = hex.EncodeToString(sum[:])
	}
	if seg.Size == 0 {
		seg.Size = int64(len(raw))
	}
	scan, scanErr := binlog.ScanCover(bytes.NewReader(raw), !segmentIsOpen(file))
	if errors.Is(scanErr, binlog.ErrCorruptEventSize) {
		return seg, win, nil
	}
	if scanErr != nil {
		return assurance.CoverSegment{}, assurance.WindowSegment{}, scanErr
	}
	seg.Readable = true
	seg.Events = scan.Events
	seg.End = scan.End
	seg.Previous = scan.Previous
	seg.HasPrevious = scan.HasPrevious
	win.Readable = true
	win.Log = gtidLogOf(scan)
	first, last, ok := copiedSpan(scan.Events)
	win.SpanOK = ok
	win.First, win.Last = first, last
	return seg, win, nil
}

func gtidLogOf(scan binlog.CoverScan) binlog.SegmentGTIDLog {
	log := binlog.SegmentGTIDLog{Previous: scan.Previous, HasPrevious: scan.HasPrevious}
	for _, ev := range scan.Events {
		if ev.Seq <= 0 || ev.UUID == "" {
			continue
		}
		log.Events = append(log.Events, binlog.GTIDEventRef{UUID: ev.UUID, Seq: ev.Seq, Offset: ev.Offset, When: ev.When})
	}
	return log
}

func copiedSpan(events []binlog.CoverEvent) (first, last time.Time, ok bool) {
	for _, ev := range events {
		if !ev.Counts || ev.When.IsZero() {
			continue
		}
		if !ok || ev.When.Before(first) {
			first = ev.When
		}
		if !ok || ev.When.After(last) {
			last = ev.When
		}
		ok = true
	}
	return first, last, ok
}

func (s *Scheduler) segmentLastVerified(taskID, name string) time.Time {
	if s == nil || name == "" {
		return time.Time{}
	}
	v := s.verifier.Load()
	if v == nil {
		return time.Time{}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	st, ok := v.state.Tasks[taskID]
	if !ok || st.LastVerified == nil {
		return time.Time{}
	}
	return st.LastVerified[name]
}

func (s *Scheduler) verifyFresh() time.Duration {
	v := s.verifier.Load()
	if v == nil || v.cfg.Every <= 0 {
		return 48 * time.Hour
	}
	return v.cfg.Every * 2
}

func (s *Scheduler) planFrom(task Task, q CoverageQuery, segs []assurance.CoverSegment, cov assurance.CoverResult, fullBackup string) RestorePlan {
	client, _ := ReplayClient(task.Source.Flavor)
	if client == "" && mysqlFlavor(task.Source.Flavor) {
		client = "mysqlbinlog"
	}
	start := startSet(task, segs)
	paths := make([]string, 0, len(cov.Segments))
	var download []string
	manifest := make([]RestoreFile, 0, len(cov.Segments))
	for _, seg := range cov.Segments {
		path := seg.LocalPath
		if path == "" {
			path = seg.Name
			download = append(download, seg.Name)
		}
		paths = append(paths, path)
		manifest = append(manifest, RestoreFile{Name: seg.Name, SHA256: seg.SHA256, Size: seg.Size, LocalPath: seg.LocalPath, ObjectKey: seg.ObjectKey})
	}
	var stopPos uint64
	if cov.StopAt != nil {
		stopPos = cov.StopAt.Pos
	}
	var command string
	switch {
	case strings.TrimSpace(q.GTID) != "":
		command = FormatGTIDCommand(client, paths, nil, stopPos)
	case q.At != nil:
		stop := q.At.UTC().Truncate(time.Second).Add(time.Second)
		command = FormatPITRCommand(client, paths, nil, stop)
	default:
		command = formatLatestCommand(client, paths)
	}
	steps := make([]RestoreStep, 0, 3)
	if len(download) > 0 {
		steps = append(steps, RestoreStep{Kind: "download", Files: download, Command: downloadCommand(task.ID, download)})
	}
	if verify := verifyCommand(cov.Segments); verify != "" {
		steps = append(steps, RestoreStep{Kind: "verify", Files: namesOf(cov.Segments), Command: verify})
	}
	steps = append(steps, RestoreStep{Kind: "apply", Files: paths, Command: command})
	fresh := s.verifyFresh()
	pre := []assurance.Precheck{
		assurance.WindowPrecheck(cov),
		assurance.ChecksumPrecheck(cov, fresh, time.Now().UTC()),
		assurance.FullBackupPrecheck(start, cov.Applied, fullBackup),
	}
	pre = append(pre, assurance.ManualPrechecks(client)...)
	return RestorePlan{
		Target:        RestoreTarget{At: q.At, GTID: strings.TrimSpace(q.GTID), Latest: q.Latest},
		Coverage:      cov,
		Prerequisites: []string{fmt.Sprintf("full backup gtid_executed must contain %s and must not intersect %s", shownSet(start), shownSet(cov.Applied))},
		Prechecks:     pre,
		Steps:         steps,
		Manifest:      manifest,
		RestoreSh:     command,
		Readme:        restoreReadme,
		Bundle:        RestoreBundle{URL: bundleURL(task.ID, q)},
	}
}

func startSet(task Task, segs []assurance.CoverSegment) string {
	if start := StartGTIDText(task); start != "" {
		return start
	}
	for _, seg := range segs {
		if seg.HasPrevious && strings.TrimSpace(seg.Previous) != "" {
			return seg.Previous
		}
	}
	return ""
}

const restoreReadme = "Times are UTC. Save manifest and restore.sh, check each SHA-256, then run restore.sh and pipe its stdout to mysql in the same shell. Compare CHECKSUM TABLE and @@gtid_executed with the source at this point.\n时间是 UTC。保存 manifest 和 restore.sh，核对每段 SHA-256，再在同一个 shell 里把 restore.sh 的标准输出管道给 mysql。用 CHECKSUM TABLE 和 @@gtid_executed 与源库在该时间点的值对照。"

func shownSet(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(empty)"
	}
	return s
}

func namesOf(segs []assurance.CoverageSegment) []string {
	out := make([]string, len(segs))
	for i, s := range segs {
		out[i] = s.Name
	}
	return out
}

func downloadCommand(taskID string, names []string) string {
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "GET /api/tasks/%s/files/%s\n", url.PathEscape(taskID), url.PathEscape(name))
	}
	return b.String()
}

func verifyCommand(segs []assurance.CoverageSegment) string {
	var b strings.Builder
	b.WriteString("sha256sum -c <<'EOF'\n")
	n := 0
	for _, s := range segs {
		if s.SHA256 == "" {
			continue
		}
		name := s.Name
		if s.LocalPath != "" {
			name = s.LocalPath
		}
		fmt.Fprintf(&b, "%s  %s\n", s.SHA256, name)
		n++
	}
	if n == 0 {
		return ""
	}
	b.WriteString("EOF\n")
	return b.String()
}

func formatLatestCommand(client string, paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	tokens := make([]string, len(paths))
	for i, p := range paths {
		tokens[i] = shellToken(p)
	}
	return formatClientCommand(client, tokens)
}

func bundleURL(taskID string, q CoverageQuery) string {
	base := "/api/tasks/" + url.PathEscape(taskID) + "/replay/archive"
	if q.At == nil {
		return base
	}
	stop := q.At.UTC().Truncate(time.Second).Add(time.Second).Format(pitrClockLayout)
	return base + "?stop_datetime=" + url.QueryEscape(stop)
}
