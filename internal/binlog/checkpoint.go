package binlog

import "time"

type Checkpoint struct {
	File      string
	Pos       uint32
	GTIDSet   string
	UpdatedAt time.Time
}
