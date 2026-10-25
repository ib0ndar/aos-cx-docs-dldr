package pdfgen

import (
	"io/fs"
	"syscall"
)

// fileSnapshotOf extracts the identity snapshot from an Lstat or Stat result.
func fileSnapshotOf(info fs.FileInfo) (fileSnapshot, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return fileSnapshot{}, false
	}
	return fileSnapshot{
		device: uint64(stat.Dev), inode: uint64(stat.Ino), size: info.Size(), mode: info.Mode(),
		uid: stat.Uid, gid: stat.Gid,
		modified: stat.Mtim.Nano(), changed: stat.Ctim.Nano(),
	}, true
}
