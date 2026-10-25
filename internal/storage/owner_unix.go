//go:build unix

package storage

import (
	"os"
	"syscall"
)

// Owner reports the numeric owner of info as the filesystem presents it.
func Owner(info os.FileInfo) (uid int, ok bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), true
}
