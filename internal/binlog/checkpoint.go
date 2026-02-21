package binlog

import "time"

// Checkpoint 记录 binlog 同步已安全落盘的位置。
type Checkpoint struct {
	File      string    `json:"file"`
	Pos       uint32    `json:"pos"`
	GTIDSet   string    `json:"gtid_set,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}
