//go:build !linux

package binlog

import "os"

func changeTime(os.FileInfo) int64 { return 0 }
