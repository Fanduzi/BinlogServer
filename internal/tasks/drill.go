// Package tasks provides module-level functionality for tasks.
// input: verify.drill_dsn / drill_every_sec / drill_check_sql, the task's source address and flavor, the retained coverage window, a RestorePlan, and an object store for covered files that are not local
// output: one restore-drill attempt per task per drill period on the task's lease holder. A point in the coverage window is picked once per period (persisted so a restart replays the same point), the plan's segments are materialized and sha256-checked, the replay client output is piped into the drill instance's mysql client, and @@gtid_executed (mysql) or @@gtid_binlog_pos (mariadb) is compared with the plan's applied set, plus the optional check SQL. A pass resolves DRILL_FAILED, stamps the last-success gauge and writes a RESTORE_DRILL event; any failure opens CRITICAL DRILL_FAILED. A drill DSN naming the task's own source host:port opens WARNING DRILL_REFUSED and never dials. An empty drill DSN means the whole path is off
// pos: ADR 0009 phase 3 §12 (docs/design/0009-phase3-verifier.md); the plan comes from Scheduler.RestorePlan (phase 5) and the client name from ReplayClient. Findings go through Scheduler.Report / ResolveWithReason like the verifier's unmanaged kinds; drill state rides .verify.json. The source DSN is never passed in and never dialed
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	driver "github.com/go-sql-driver/mysql"
	"go.uber.org/zap"
)

// DrillConfig is the restore drill's optional configuration on
// VerifierConfig. DSN is the drill instance's only address — empty leaves
// the drill off and never dialed (decision 8). Every is the attempt period
// (config default 7d); <= 0 also leaves the drill off. CheckSQL is the
// optional post-replay check statement.
type DrillConfig struct {
	DSN      string
	Every    time.Duration
	CheckSQL string
}

// FindingDrillFailed: the drill replay's executed set missed the plan's end
// set, or the check SQL failed (phase 3 §12). CRITICAL.
// FindingDrillRefused: the drill DSN names the task's own source host:port;
// the worker never dials it (decision 8). WARNING.
const (
	FindingDrillFailed  FindingKind = "DRILL_FAILED"
	FindingDrillRefused FindingKind = "DRILL_REFUSED"
)

// drillEventType is the task event every drill attempt leaves (design §12:
// a finding plus one RESTORE_DRILL event).
const drillEventType = "RESTORE_DRILL"

// drillTaskState is one task's drill record inside .verify.json. Period is
// now.Unix()/DrillEvery seconds. Point is the chosen restore target so a
// restart inside one period replays the same point instead of silently
// closing a failure with a new one. Done ends the period's attempt (pass or
// fail); a process that died mid-run retries the same point.
type drillTaskState struct {
	Period    int64     `json:"period,omitempty"`
	Point     time.Time `json:"point,omitempty"`
	Done      bool      `json:"done,omitempty"`
	SuccessAt time.Time `json:"success_at,omitempty"`
}

// drillDB is the drill instance side the worker dials. Tests stub it.
type drillDB interface {
	// ExecutedSet reads the instance's executed GTID set in the task's
	// dialect: @@global.gtid_executed for mysql, @@global.gtid_binlog_pos
	// for mariadb.
	ExecutedSet(ctx context.Context, flavor string) (string, error)
	ExecCheck(ctx context.Context, query string) error
	Close() error
}

// sqlDrillDB is the production drillDB over database/sql.
type sqlDrillDB struct{ db *sql.DB }

func (d sqlDrillDB) ExecutedSet(ctx context.Context, flavor string) (string, error) {
	col := "@@global.gtid_executed"
	if !mysqlFlavor(flavor) {
		col = "@@global.gtid_binlog_pos"
	}
	var set string
	if err := d.db.QueryRowContext(ctx, "SELECT "+col).Scan(&set); err != nil {
		return "", err
	}
	return set, nil
}

func (d sqlDrillDB) ExecCheck(ctx context.Context, query string) error {
	_, err := d.db.ExecContext(ctx, query)
	return err
}

func (d sqlDrillDB) Close() error { return d.db.Close() }

// drillOps abstracts the OS and dial sides of a drill so tests replay them,
// mirroring verifyFiles.
type drillOps struct {
	lookPath func(string) (string, error)
	// run executes name args in dir with env appended to the process env and
	// returns an error that includes trimmed combined output.
	run func(ctx context.Context, dir string, env []string, name string, args ...string) error
	// openDSN dials the drill instance. The drill DSN is the only address it
	// ever sees (design decision 8).
	openDSN func(dsn string) (drillDB, error)
	// openObj downloads a covered file that is not local.
	openObj func(ctx context.Context, key string) (io.ReadCloser, error)
}

var drillExec = drillOps{
	lookPath: exec.LookPath,
	run: func(ctx context.Context, dir string, env []string, name string, args ...string) error {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		msg := strings.TrimSpace(string(out))
		if len(msg) > 400 {
			msg = msg[len(msg)-400:]
		}
		if msg != "" {
			return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
		}
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	},
	openDSN: func(dsn string) (drillDB, error) {
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			return nil, err
		}
		return sqlDrillDB{db: db}, nil
	},
}

// splitDrillAddr returns the host and port of a go-sql-driver Addr
// ("host:port"). An empty host or a non-numeric port yields "",0.
func splitDrillAddr(addr string) (string, int) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0
	}
	return host, port
}

// drillSHA256 streams one file's digest for the manifest check.
func drillSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// drillFlavor normalizes the task's source flavor for go-mysql GTID parsing.
func drillFlavor(flavor string) string {
	if mysqlFlavor(flavor) {
		return "mysql"
	}
	return "mariadb"
}

// drillTask runs the restore drill for one task when its period is due. It is
// called from the verifier pass so it inherits the Allows gate: only the
// lease holder reads the local chain and may dial the drill instance.
func (v *Verifier) drillTask(ctx context.Context, task Task, now time.Time) {
	dsn := strings.TrimSpace(v.cfg.Drill.DSN)
	if dsn == "" || v.cfg.Drill.Every <= 0 {
		return
	}
	if v.cfg.Allows != nil {
		ok, err := v.cfg.Allows(ctx, task.ID)
		if err != nil || !ok {
			return
		}
	}
	// The DSN has to parse before the host check and the dial.
	dc, err := driver.ParseDSN(dsn)
	if err != nil {
		v.drillFail(task.ID, fmt.Sprintf("verify.drill_dsn does not parse: %v", err))
		return
	}
	period := now.Unix() / int64(v.cfg.Drill.Every.Seconds())
	st := v.drillState(task.ID)
	if st != nil && st.Period == period && st.Done {
		return
	}

	// Decision 8: never replay into the task's own source.
	host, port := splitDrillAddr(dc.Addr)
	if host != "" && int(task.Source.Port) == port && SameSourceHost(task.Source.Host, host) {
		v.sched.Report(Finding{TaskID: task.ID, Kind: FindingDrillRefused, Severity: FindingWarning,
			Detail: "verify.drill_dsn host:port equals the task source; the drill never dials its own source"})
		v.markDrill(task.ID, period, time.Time{}, true, time.Time{})
		return
	}
	v.sched.ResolveWithReason(task.ID, FindingDrillRefused, "", "drill DSN does not name the task source")

	// One stable point per period: reuse the persisted one or pick and save
	// before any side effect, so a crash replays the same target.
	point := time.Time{}
	if st != nil && st.Period == period {
		point = st.Point
	}
	if point.IsZero() {
		var perr error
		point, perr = v.drillPoint(task.ID)
		if perr != nil {
			zap.L().Info(fmt.Sprintf("drill: task %s no point this pass: %v", task.ID, perr))
			return
		}
		v.markDrill(task.ID, period, point, false, time.Time{})
		v.save()
	}

	fail := func(format string, args ...any) {
		detail := fmt.Sprintf(format, args...)
		v.sched.Report(Finding{TaskID: task.ID, Kind: FindingDrillFailed, Severity: FindingCritical, Detail: detail})
		v.sched.noteEvent(task.ID, drillEventType, "restore drill failed", detail)
		v.markDrill(task.ID, period, point, true, time.Time{})
	}
	defer func() {
		if r := recover(); r != nil {
			fail("restore drill panicked: %v", r)
		}
	}()

	plan, err := v.sched.RestorePlan(task.ID, CoverageQuery{At: &point}, "")
	if err != nil {
		fail("restore plan at %s: %v", point.UTC().Format(time.RFC3339), err)
		return
	}
	if !plan.Coverage.Covered {
		fail("restore plan at %s not covered: %s", point.UTC().Format(time.RFC3339), plan.Coverage.Reason)
		return
	}
	if len(plan.Manifest) == 0 {
		fail("restore plan at %s covers no files", point.UTC().Format(time.RFC3339))
		return
	}

	dir, err := os.MkdirTemp(v.cfg.DataDir, ".drill-"+task.ID+"-")
	if err != nil {
		fail("drill staging dir: %v", err)
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()

	paths, err := v.drillStage(ctx, plan, dir)
	if err != nil {
		fail("%v", err)
		return
	}
	if err := v.drillReplay(ctx, task, dc, host, port, paths, point, dir); err != nil {
		fail("%v", err)
		return
	}
	if err := v.drillCheck(ctx, task, plan, dsn); err != nil {
		fail("%v", err)
		return
	}

	v.sched.ResolveWithReason(task.ID, FindingDrillFailed, "", "restore drill passed")
	v.sched.noteEvent(task.ID, drillEventType, "restore drill passed",
		fmt.Sprintf("restored %d files to %s on %s:%d", len(paths), point.UTC().Format(time.RFC3339), host, port))
	v.markDrill(task.ID, period, point, true, now)
}

// drillStage materializes every planned file under dir — covered files that
// are not local come down through the object store — and checks each digest
// against the manifest.
func (v *Verifier) drillStage(ctx context.Context, plan RestorePlan, dir string) ([]string, error) {
	paths := make([]string, 0, len(plan.Manifest))
	for _, f := range plan.Manifest {
		p := f.LocalPath
		if p == "" {
			var derr error
			p, derr = v.drillFetch(ctx, dir, f)
			if derr != nil {
				return nil, fmt.Errorf("fetch %s: %w", f.Name, derr)
			}
		}
		if f.SHA256 != "" {
			sum, herr := drillSHA256(p)
			if herr != nil {
				return nil, fmt.Errorf("hash %s: %w", f.Name, herr)
			}
			if !strings.EqualFold(sum, f.SHA256) {
				return nil, fmt.Errorf("sha256 mismatch on %s: plan %s file %s", f.Name, f.SHA256, sum)
			}
		}
		paths = append(paths, p)
	}
	return paths, nil
}

// drillReplay pipes the phase-5 replay client's binlog stream into the drill
// instance's mysql client. The password rides MYSQL_PWD so it never sits on
// a command line (drill DSN secrets stay out of ps and logs).
func (v *Verifier) drillReplay(ctx context.Context, task Task, dc *driver.Config, host string, port int, paths []string, point time.Time, dir string) error {
	client, _ := ReplayClient(task.Source.Flavor)
	if client == "" && mysqlFlavor(task.Source.Flavor) {
		client = "mysqlbinlog"
	}
	if client == "" {
		return fmt.Errorf("no replay client for source flavor %q", task.Source.Flavor)
	}
	if _, err := drillExec.lookPath(client); err != nil {
		return fmt.Errorf("replay client %s not on PATH: %w", client, err)
	}
	mysqlBin := "mysql"
	if !mysqlFlavor(task.Source.Flavor) {
		mysqlBin = "mariadb"
	}
	if _, err := drillExec.lookPath(mysqlBin); err != nil {
		return fmt.Errorf("client %s not on PATH: %w", mysqlBin, err)
	}
	apply := FormatPITRCommand(client, paths, nil, point.UTC().Truncate(time.Second).Add(time.Second))
	if apply == "" {
		return fmt.Errorf("empty apply command at %s", point.UTC().Format(time.RFC3339))
	}
	pipe := fmt.Sprintf("%s | %s --protocol=tcp -h%s -P%s -u%s", apply, mysqlBin,
		shellToken(host), shellToken(strconv.Itoa(port)), shellToken(dc.User))
	if err := drillExec.run(ctx, dir, []string{"MYSQL_PWD=" + dc.Passwd}, "sh", "-c", pipe); err != nil {
		return fmt.Errorf("replay at %s: %w", point.UTC().Format(time.RFC3339), err)
	}
	return nil
}

// drillCheck dials the drill instance and requires its executed GTID set to
// contain the plan's applied set, then runs the optional check SQL.
func (v *Verifier) drillCheck(ctx context.Context, task Task, plan RestorePlan, dsn string) error {
	db, err := drillExec.openDSN(dsn)
	if err != nil {
		return fmt.Errorf("dial drill instance: %w", err)
	}
	defer func() { _ = db.Close() }()

	if applied := strings.TrimSpace(plan.Coverage.Applied); applied != "" {
		executed, derr := db.ExecutedSet(ctx, task.Source.Flavor)
		if derr != nil {
			return fmt.Errorf("read executed set on the drill instance: %w", derr)
		}
		have, herr := gomysql.ParseGTIDSet(drillFlavor(task.Source.Flavor), executed)
		if herr != nil {
			return fmt.Errorf("parse executed set %q: %w", executed, herr)
		}
		want, werr := gomysql.ParseGTIDSet(drillFlavor(task.Source.Flavor), applied)
		if werr != nil {
			return fmt.Errorf("parse plan applied set %q: %w", applied, werr)
		}
		if !have.Contain(want) {
			return fmt.Errorf("drill executed set %s does not contain plan end set %s", executed, applied)
		}
	}
	if q := strings.TrimSpace(v.cfg.Drill.CheckSQL); q != "" {
		if err := db.ExecCheck(ctx, q); err != nil {
			return fmt.Errorf("verify.drill_check_sql: %w", err)
		}
	}
	return nil
}

// drillState returns the persisted drill record for taskID (nil if none).
func (v *Verifier) drillState(taskID string) *drillTaskState {
	v.mu.Lock()
	defer v.mu.Unlock()
	st, ok := v.state.Tasks[taskID]
	if !ok || st.Drill == nil {
		return nil
	}
	d := *st.Drill
	return &d
}

// markDrill writes the task's drill record and sets dirty.
func (v *Verifier) markDrill(taskID string, period int64, point time.Time, done bool, successAt time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	st := v.state.Tasks[taskID]
	if st.Drill == nil {
		st.Drill = &drillTaskState{}
	}
	st.Drill.Period = period
	if !point.IsZero() {
		st.Drill.Point = point
	}
	st.Drill.Done = done
	if !successAt.IsZero() {
		st.Drill.SuccessAt = successAt
	}
	v.state.Tasks[taskID] = st
	v.dirty = true
	if successAt.IsZero() {
		return
	}
	m := v.metrics[taskID]
	m.LastDrillSuccessUnix = successAt.Unix()
	m.HaveDrillSuccess = true
	v.metrics[taskID] = m
}

// drillFail records a failure that happens before the period machinery runs
// (an unparseable DSN). It does not consume the period.
func (v *Verifier) drillFail(taskID string, detail string) {
	v.sched.Report(Finding{TaskID: taskID, Kind: FindingDrillFailed, Severity: FindingCritical, Detail: detail})
	v.sched.noteEvent(taskID, drillEventType, "restore drill failed", detail)
}

// drillPoint picks this task's restore target: the latest event time in the
// retained recovery window. Persisted by the caller before the drill runs.
func (v *Verifier) drillPoint(taskID string) (time.Time, error) {
	win, err := v.sched.RecoveryWindow(taskID)
	if err != nil {
		return time.Time{}, err
	}
	if win.Latest == nil {
		return time.Time{}, errors.New("recovery window is empty")
	}
	return *win.Latest, nil
}

// drillFetch downloads one covered-but-not-local manifest file into dir.
func (v *Verifier) drillFetch(ctx context.Context, dir string, f RestoreFile) (string, error) {
	open := drillExec.openObj
	if open == nil {
		if v.cfg.Objects == nil {
			return "", fmt.Errorf("covered file %s is not local and the object store is off", f.Name)
		}
		open = v.cfg.Objects.Open
	}
	key := f.ObjectKey
	if key == "" {
		key = f.Name
	}
	rc, err := open(ctx, key)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	dst := filepath.Join(dir, filepath.Base(f.Name))
	out, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	_, copyErr := io.Copy(out, rc)
	closeErr := out.Close()
	if copyErr != nil {
		return "", copyErr
	}
	return dst, closeErr
}
