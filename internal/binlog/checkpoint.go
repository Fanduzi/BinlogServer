package binlog

import "time"

type Checkpoint struct {
	File      string    `json:"file"`
	Pos       uint32    `json:"pos"`
	GTIDSet   string    `json:"gtid_set,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}
