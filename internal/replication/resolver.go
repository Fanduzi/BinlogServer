// Package replication provides module-level functionality for replication.
// input: task start strategy, MasterStatusFetcher, dump file/pos
// output: resolved FILE_POS/GTID start, conservative dump-vs-master file/pos comparison
// pos: start-point resolver and idle at-tip comparison helper
// note: if this file changes, update this header and module README.md.
package replication

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"binlog_server/internal/tasks"
)

// MasterStatus 对应源库 SHOW MASTER STATUS 返回的位点。
type MasterStatus struct {
	// File/Pos 对应 SHOW MASTER STATUS 的当前位点。
	File string
	Pos  uint32
}

// MasterStatusFetcher 定义读取主库位点的能力。
type MasterStatusFetcher interface {
	// FetchMasterStatus 拉取主库当前 file/pos。
	FetchMasterStatus(ctx context.Context, source tasks.SourceConfig) (MasterStatus, error)
}

// ResolveStart 将任务 start 策略解析为可直接用于拉流的起点配置。
func ResolveStart(ctx context.Context, task tasks.Task, fetcher MasterStatusFetcher) (tasks.StartConfig, error) {
	start := task.Start
	if start.Mode == "" {
		start.Mode = tasks.StartModeLatest
	}

	switch start.Mode {
	case tasks.StartModeLatest:
		if fetcher == nil {
			return tasks.StartConfig{}, errors.New("latest mode requires master status fetcher")
		}
		status, err := fetcher.FetchMasterStatus(ctx, task.Source)
		if err != nil {
			return tasks.StartConfig{}, err
		}
		if status.File == "" || status.Pos == 0 {
			return tasks.StartConfig{}, fmt.Errorf("invalid master status: file=%q pos=%d", status.File, status.Pos)
		}
		return tasks.StartConfig{
			Mode: tasks.StartModeFilePos,
			File: status.File,
			Pos:  status.Pos,
		}, nil
	case tasks.StartModeFilePos:
		if start.File == "" || start.Pos == 0 {
			return tasks.StartConfig{}, errors.New("file_pos requires file and pos")
		}
		return start, nil
	case tasks.StartModeGTID:
		if start.GTIDSet == "" {
			return tasks.StartConfig{}, errors.New("gtid requires gtid_set")
		}
		return start, nil
	default:
		return tasks.StartConfig{}, fmt.Errorf("unsupported start mode: %s", start.Mode)
	}
}

// dumpAtOrBeyondMaster reports whether dump file/pos is at or past SHOW MASTER STATUS.
// Comparison is file then pos. Typical names are mysql-bin.NNNNNN; unparseable or
// mismatched prefixes are treated as not at tip.
func dumpAtOrBeyondMaster(file string, pos uint32, master MasterStatus) bool {
	if file == "" || master.File == "" || master.Pos == 0 {
		return false
	}
	cmp, ok := compareBinlogFiles(file, master.File)
	if !ok {
		return false
	}
	if cmp != 0 {
		return cmp > 0
	}
	return pos >= master.Pos
}

func compareBinlogFiles(a, b string) (int, bool) {
	if a == b {
		return 0, true
	}
	aPrefix, aSeq, aOK := splitBinlogName(a)
	bPrefix, bSeq, bOK := splitBinlogName(b)
	if !aOK || !bOK || aPrefix != bPrefix {
		return 0, false
	}
	switch {
	case aSeq > bSeq:
		return 1, true
	case aSeq < bSeq:
		return -1, true
	default:
		return 0, true
	}
}

func splitBinlogName(name string) (prefix string, seq uint64, ok bool) {
	i := strings.LastIndex(name, ".")
	if i <= 0 || i == len(name)-1 {
		return "", 0, false
	}
	seq, err := strconv.ParseUint(name[i+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return name[:i], seq, true
}
