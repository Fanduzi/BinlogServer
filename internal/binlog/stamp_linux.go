//go:build linux

package binlog

import (
	"os"
	"syscall"
)

func changeTime(info os.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Ctim.Sec*1e9 + st.Ctim.Nsec
	}
	return 0
}
