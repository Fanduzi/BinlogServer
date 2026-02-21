package replication

import (
	"context"
	"errors"
	"fmt"

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
