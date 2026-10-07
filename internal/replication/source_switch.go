// Package replication provides module-level functionality for replication.
// input: the identity recorded for a task directory, the identity now reached at the task host:port, and that server's GTID executed and purged sets
// output: a plan that seals the old server's open segment and continues with COM_BINLOG_DUMP_GTID, or a permanent SOURCE_SWITCHOVER error naming both identities
// pos: VIP switch decision used by the replication runner before a byte from the new server is written
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"binlog_server/internal/binlog"
	"binlog_server/internal/tasks"

	sqlclient "github.com/go-mysql-org/go-mysql/client"
)

const (
	sourceChainFile   = ".source-chain"
	sourceResumeGTID  = ".source-resume-gtid"
	sourceSwitchEvent = "SOURCE_SWITCHOVER"
)

// errSwitchRestart tells the dump loop to open a GTID dump on the new server.
// The event that discovered the switch is not written.
var errSwitchRestart = fmt.Errorf("restart dump after source switch")

// switchGTIDProbe reads GTID_SUBSET / GTID_SUBTRACT on the server at host:port.
type switchGTIDProbe interface {
	ProbeSwitchGTID(ctx context.Context, source tasks.SourceConfig, ours string) (haveOurs bool, missing string, missingPurged bool, err error)
}

// sourceSession is the identity this dump is allowed to append to.
// original is the first server, whose files keep the source basename.
// active is the server the open segment belongs to. A later server's files
// are named {identity}.{source basename} so they cannot replace the earlier file.
type sourceSession struct {
	// dir is the directory that holds this task's segments. Takeover keeps
	// writing there, so the identity chain lives next to those files and
	// does not create a second task directory on the new worker.
	dir        string
	original   string
	active     string
	chain      []string
	seenConn   uint32
	resumeGTID string
}

func (s *sourceSession) diskName(serverFile string) string {
	return diskSourceName(s.original, s.active, serverFile)
}

func (s *sourceSession) serverName(name string) string {
	return serverBinlogName(name, s.original, s.chain)
}

func diskSourceName(original, active, serverFile string) string {
	serverFile = strings.TrimSpace(serverFile)
	if serverFile == "" || active == "" || original == "" || active == original {
		return serverFile
	}
	prefix := active + "."
	if strings.HasPrefix(serverFile, prefix) {
		return serverFile
	}
	return prefix + serverFile
}

func serverBinlogName(name, original string, ids []string) string {
	name = strings.TrimSpace(name)
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || id == original {
			continue
		}
		prefix := id + "."
		if strings.HasPrefix(name, prefix) {
			return strings.TrimPrefix(name, prefix)
		}
	}
	return name
}

func nameBelongs(name, original, active string) bool {
	name = strings.TrimSpace(name)
	if name == "" || active == "" {
		return false
	}
	if active == original || original == "" {
		return serverBinlogName(name, original, []string{active}) == name
	}
	return strings.HasPrefix(name, active+".")
}

type switchAction int

const (
	switchSame switchAction = iota
	switchAdopt
	switchContinue
	switchStop
)

type switchPlan struct {
	action  switchAction
	message string
	detail  string
}

// planSourceSwitch decides whether a VIP that now reaches a different server
// can continue. captured means this task already has a checkpoint or a local
// event. ours is the executed GTID set. haveOurs, missing, and missingPurged
// come from the new server. probed is false when those three were not read.
func planSourceSwitch(flavor, oldID, newID, ours string, captured, probed, haveOurs bool, missing string, missingPurged bool, file string, pos uint32) switchPlan {
	oldID = strings.TrimSpace(oldID)
	newID = strings.TrimSpace(newID)
	ours = strings.TrimSpace(ours)
	if oldID == "" || oldID == newID {
		return switchPlan{action: switchSame}
	}
	detail := switchDetail(oldID, newID, ours, file, pos)
	if !captured && ours == "" {
		return switchPlan{
			action:  switchAdopt,
			message: fmt.Sprintf("source switched from %s to %s before any transaction was stored; continuing with the configured start", oldID, newID),
			detail:  detail,
		}
	}
	if !strings.EqualFold(strings.TrimSpace(flavor), "mysql") {
		return switchPlan{
			action:  switchStop,
			message: switchStopMessage(oldID, newID, "MariaDB has no usable GTID path for a source switch"),
			detail:  detail,
		}
	}
	if ours == "" {
		return switchPlan{
			action:  switchStop,
			message: switchStopMessage(oldID, newID, "This backup has no GTID set, so the old source file and position cannot be applied to the new source"),
			detail:  detail,
		}
	}
	if !probed {
		return switchPlan{
			action:  switchStop,
			message: switchStopMessage(oldID, newID, "The new source GTID state could not be read"),
			detail:  detail,
		}
	}
	if !haveOurs {
		return switchPlan{
			action:  switchStop,
			message: switchStopMessage(oldID, newID, "The new primary is missing transactions this backup already has"),
			detail:  detail,
		}
	}
	if strings.TrimSpace(missing) != "" && missingPurged {
		return switchPlan{
			action:  switchStop,
			message: switchStopMessage(oldID, newID, "The new primary has purged transactions this backup does not have yet, so they cannot be copied"),
			detail:  detail,
		}
	}
	at := ours
	if file != "" && pos != 0 {
		at = fmt.Sprintf("%s:%d gtid_set=%s", file, pos, ours)
	}
	return switchPlan{
		action:  switchContinue,
		message: fmt.Sprintf("source switched from %s to %s at %s; continuing from the executed GTID set", oldID, newID, at),
		detail:  detail,
	}
}

func switchStopMessage(oldID, newID, why string) string {
	return fmt.Sprintf("source switched from %s to %s. %s. Start a new task against the new primary and keep this backup. This task will not mix the two servers.", oldID, newID, why)
}

func switchDetail(oldID, newID, gtid, file string, pos uint32) string {
	return fmt.Sprintf("old=%s new=%s gtid_set=%s file=%s pos=%d", oldID, newID, gtid, file, pos)
}

func (r *MySQLRunner) emitSwitch(taskID string, plan switchPlan) {
	if r == nil || plan.message == "" {
		return
	}
	r.dumpMu.Lock()
	fn := r.onSourceSwitch
	r.dumpMu.Unlock()
	if fn == nil {
		return
	}
	fn(taskID, tasks.SourceSwitchNotice{
		Message:   plan.message,
		Detail:    plan.detail,
		Continued: plan.action == switchContinue || plan.action == switchAdopt,
	})
}

func (r *MySQLRunner) sourceDir(taskID string) string {
	if r == nil || strings.TrimSpace(r.dataDir) == "" || strings.TrimSpace(taskID) == "" {
		return ""
	}
	return filepath.Join(r.dataDir, taskID)
}

func loadSourceChain(dir string) []string {
	if dir == "" {
		return nil
	}
	body, err := os.ReadFile(filepath.Join(dir, sourceChainFile))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func loadResumeGTID(dir string) string {
	if dir == "" {
		return ""
	}
	body, err := os.ReadFile(filepath.Join(dir, sourceResumeGTID))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}

func writeSourceChain(dir string, chain []string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, sourceChainFile), strings.Join(chain, "\n")+"\n")
}

func writeResumeGTID(dir, gtid string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, sourceResumeGTID), strings.TrimSpace(gtid)+"\n")
}

func clearResumeGTID(dir string) {
	if dir == "" {
		return
	}
	_ = os.Remove(filepath.Join(dir, sourceResumeGTID))
}

func writeAtomic(path, body string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func appendIdentity(chain []string, id string) []string {
	id = strings.TrimSpace(id)
	if id == "" {
		return chain
	}
	if len(chain) > 0 && chain[len(chain)-1] == id {
		return chain
	}
	for _, existing := range chain {
		if existing == id {
			return chain
		}
	}
	return append(chain, id)
}

// prepareSourceIdentity records the first server, or decides a switch before
// the writer opens. A continue result replaces start with the executed GTID set.
// A stop result seals an open segment from the old server and returns SOURCE_SWITCHOVER.
func (r *MySQLRunner) prepareSourceIdentity(ctx context.Context, task tasks.Task, connected string, start tasks.StartConfig, stored binlog.Checkpoint, checkpointExists bool, gtidFallback string, freshLatest bool, segmentDir string) (*sourceSession, tasks.StartConfig, error) {
	connected = strings.TrimSpace(connected)
	dir := segmentDir
	if dir == "" {
		dir = r.sourceDir(task.ID)
	}
	chain := loadSourceChain(dir)
	session := &sourceSession{dir: dir, active: connected, chain: chain}
	if len(chain) > 0 {
		session.original = chain[0]
	}
	if len(chain) == 0 {
		session.original = connected
		session.chain = []string{connected}
		if err := writeSourceChain(dir, session.chain); err != nil {
			return nil, start, err
		}
		return session, start, nil
	}
	recorded := chain[len(chain)-1]
	session.active = recorded
	if recorded == connected {
		marker := loadResumeGTID(dir)
		session.resumeGTID = marker
		if marker != "" && !nameBelongs(start.File, session.original, recorded) {
			if err := r.sealForeignOpens(ctx, task, dir, session.original, recorded); err != nil {
				return nil, start, err
			}
			start = tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: marker}
			return session, start, nil
		}
		if marker != "" && nameBelongs(start.File, session.original, recorded) {
			clearResumeGTID(dir)
			session.resumeGTID = ""
		}
		session.active = recorded
		return session, start, nil
	}

	_, _, local := binlog.DurableResume(r.dataDir, task.ID)
	captured := local || (checkpointExists && strings.TrimSpace(stored.File) != "")
	ours := strings.TrimSpace(gtidFallback)
	if ours == "" && task.Start.Mode == tasks.StartModeGTID {
		ours = strings.TrimSpace(task.Start.GTIDSet)
	}
	file, pos := stored.File, stored.Pos
	if file == "" {
		file, pos = start.File, start.Pos
	}
	if !captured && ours == "" && freshLatest {
		plan := planSourceSwitch(task.Source.Flavor, recorded, connected, "", false, false, false, "", false, file, pos)
		session.chain = appendIdentity(chain, connected)
		session.original = session.chain[0]
		session.active = connected
		if err := writeSourceChain(dir, session.chain); err != nil {
			return nil, start, err
		}
		r.emitSwitch(task.ID, plan)
		return session, start, nil
	}
	plan, err := r.decideSwitch(ctx, task, recorded, connected, ours, true, file, pos)
	if err != nil {
		return nil, start, err
	}
	if plan.action == switchStop {
		if sealErr := r.sealForeignOpens(ctx, task, dir, session.original, recorded); sealErr != nil {
			return nil, start, sealErr
		}
		r.emitSwitch(task.ID, plan)
		return nil, start, tasks.NewPermanentError(tasks.CodeSourceSwitchover, plan.message)
	}
	if err := r.sealForeignOpens(ctx, task, dir, session.original, recorded); err != nil {
		return nil, start, err
	}
	session.chain = appendIdentity(chain, connected)
	session.original = session.chain[0]
	session.active = connected
	session.resumeGTID = ours
	if err := writeSourceChain(dir, session.chain); err != nil {
		return nil, start, err
	}
	if err := writeResumeGTID(dir, ours); err != nil {
		return nil, start, err
	}
	r.emitSwitch(task.ID, plan)
	return session, tasks.StartConfig{Mode: tasks.StartModeGTID, GTIDSet: ours}, nil
}

func (r *MySQLRunner) decideSwitch(ctx context.Context, task tasks.Task, oldID, newID, ours string, captured bool, file string, pos uint32) (switchPlan, error) {
	flavor := strings.TrimSpace(task.Source.Flavor)
	if flavor == "" {
		flavor = "mysql"
	}
	probed, haveOurs, missing, missingPurged := false, false, "", false
	if strings.EqualFold(flavor, "mysql") && strings.TrimSpace(ours) != "" {
		var err error
		haveOurs, missing, missingPurged, err = r.probeSwitch(ctx, task.Source, ours)
		if err != nil {
			return switchPlan{}, err
		}
		probed = true
	}
	return planSourceSwitch(flavor, oldID, newID, ours, captured, probed, haveOurs, missing, missingPurged, file, pos), nil
}

func (r *MySQLRunner) probeSwitch(ctx context.Context, source tasks.SourceConfig, ours string) (bool, string, bool, error) {
	prober, ok := r.fetcher.(switchGTIDProbe)
	if !ok || r.fetcher == nil {
		return false, "", false, fmt.Errorf("source does not report GTID state")
	}
	have, missing, purged, err := prober.ProbeSwitchGTID(ctx, source, ours)
	if err != nil {
		return false, "", false, classifySourceError(err)
	}
	return have, missing, purged, nil
}

// noteDumpIdentity runs before an event is written.
// A new connection to the same server continues.
// A different server seals the open segment, then either restarts a GTID dump
// or returns SOURCE_SWITCHOVER. The event itself is left unwritten.
func (r *MySQLRunner) noteDumpIdentity(ctx context.Context, task tasks.Task, session *sourceSession, syncer binlogSyncer, ours, file string, pos uint32, seal func(owner string) error) error {
	if session == nil || syncer == nil {
		return nil
	}
	id := dumpConnectionID(syncer)
	if id == 0 || id == session.seenConn {
		return nil
	}
	if r.fetcher == nil {
		return nil
	}
	next, err := r.fetcher.FetchServerUUID(ctx, task.Source)
	if err != nil {
		return classifySourceError(err)
	}
	next = strings.TrimSpace(next)
	if next == "" || next == session.active {
		session.seenConn = id
		return nil
	}
	plan, err := r.decideSwitch(ctx, task, session.active, next, ours, true, file, pos)
	if err != nil {
		return err
	}
	owner := session.active
	if owner == "" {
		owner = session.original
	}
	if seal != nil {
		if err := seal(owner); err != nil {
			return err
		}
	}
	if plan.action == switchStop {
		r.emitSwitch(task.ID, plan)
		return tasks.NewPermanentError(tasks.CodeSourceSwitchover, plan.message)
	}
	dir := session.dir
	if dir == "" {
		dir = r.sourceDir(task.ID)
	}
	session.chain = appendIdentity(session.chain, next)
	if session.original == "" && len(session.chain) > 0 {
		session.original = session.chain[0]
	}
	session.active = next
	session.resumeGTID = ours
	session.seenConn = 0
	if err := writeSourceChain(dir, session.chain); err != nil {
		return err
	}
	if err := writeResumeGTID(dir, ours); err != nil {
		return err
	}
	r.emitSwitch(task.ID, plan)
	return errSwitchRestart
}

// sealForeignOpens seals open segments that do not belong to active.
// The object key uses the identity that wrote the file.
func (r *MySQLRunner) sealForeignOpens(ctx context.Context, task tasks.Task, dir, original, active string) error {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		named, ok := binlog.ClassifySegment(entry.Name())
		if !ok || !named.Open {
			continue
		}
		if nameBelongs(named.Source, original, active) {
			continue
		}
		owner := original
		if prefix := identityPrefix(named.Source, loadSourceChain(dir)); prefix != "" {
			owner = prefix
		}
		if err := r.sealOpenSegmentFile(ctx, task, filepath.Join(dir, entry.Name()), owner); err != nil {
			return err
		}
	}
	return nil
}

func identityPrefix(name string, ids []string) string {
	var best string
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if strings.HasPrefix(name, id+".") && len(id) > len(best) {
			best = id
		}
	}
	return best
}

func (r *MySQLRunner) sealOpenSegmentFile(ctx context.Context, task tasks.Task, path, owner string) error {
	if path == "" {
		return nil
	}
	if !segmentHasEvents(path) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	start, end, ok := binlog.SegmentPositions(path)
	if !ok {
		start, end = 4, 4
	}
	created := time.Now()
	if info, err := os.Stat(path); err == nil {
		created = info.ModTime()
	}
	meta, err := r.sealLocalFile(ctx, task, owner, path, start, end, created, time.Now())
	if err != nil {
		return err
	}
	return r.uploadSealed(ctx, meta)
}

// ProbeSwitchGTID reports whether ours is contained in @@gtid_executed and
// whether every transaction the server has that ours does not is still in the binlogs.
func (f *mysqlStatusFetcher) ProbeSwitchGTID(_ context.Context, source tasks.SourceConfig, ours string) (bool, string, bool, error) {
	ours = strings.TrimSpace(ours)
	if !safeGTIDText(ours) {
		return false, "", false, fmt.Errorf("invalid gtid set")
	}
	addr := fmt.Sprintf("%s:%d", source.Host, source.Port)
	conn, err := sqlclient.Connect(addr, source.User, source.Password, "")
	if err != nil {
		return false, "", false, classifySourceError(err)
	}
	defer conn.Close()
	query := fmt.Sprintf(
		"SELECT GTID_SUBSET('%s', @@global.gtid_executed), GTID_SUBTRACT(@@global.gtid_executed, '%s'), GTID_SUBSET(GTID_SUBTRACT(@@global.gtid_executed, '%s'), @@global.gtid_purged)",
		ours, ours, ours,
	)
	result, err := conn.Execute(query)
	if err != nil {
		return false, "", false, classifySourceError(err)
	}
	if result == nil || result.Resultset == nil || result.Resultset.RowNumber() == 0 {
		return false, "", false, fmt.Errorf("empty gtid switch probe")
	}
	haveText, err := result.GetString(0, 0)
	if err != nil {
		return false, "", false, err
	}
	missing, err := result.GetString(0, 1)
	if err != nil {
		return false, "", false, err
	}
	purgedText, err := result.GetString(0, 2)
	if err != nil {
		return false, "", false, err
	}
	return strings.TrimSpace(haveText) == "1", strings.TrimSpace(missing), strings.TrimSpace(purgedText) == "1", nil
}

func safeGTIDText(s string) bool {
	if s == "" || len(s) > 1024*1024 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		case r == ':' || r == '-' || r == ',' || r == '\n' || r == ' ':
		default:
			return false
		}
	}
	return true
}
