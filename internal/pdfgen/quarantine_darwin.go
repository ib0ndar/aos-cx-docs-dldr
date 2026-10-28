//go:build darwin

package pdfgen

import (
	"errors"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

const quarantineSupported = true

func openPinnedNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

func hasQuarantine(file *os.File) (bool, error) {
	if _, err := unix.Fgetxattr(int(file.Fd()), quarantineAttribute, nil); err != nil {
		if errors.Is(err, unix.ENOATTR) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// removeQuarantine removes the quarantine from the open file described by
// info. macOS refuses to change the attributes of a file without write
// permission, even for its owner, and qpdf's files ship read-only, so an
// owner's read-only file is made writable for the removal only and then
// given back its exact mode.
func removeQuarantine(file *os.File, info fs.FileInfo) error {
	fd := int(file.Fd())
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return unix.EACCES
	}
	mode := uint32(stat.Mode) & 0o7777
	writable := mode&0o200 != 0
	if !writable {
		if int(stat.Uid) != os.Getuid() {
			return unix.EACCES
		}
		if err := unix.Fchmod(fd, mode|0o200); err != nil {
			return err
		}
	}
	err := unix.Fremovexattr(fd, quarantineAttribute)
	if errors.Is(err, unix.ENOATTR) {
		err = nil
	}
	if !writable {
		err = errors.Join(err, unix.Fchmod(fd, mode))
	}
	return err
}

// attributesNeedPrivilege reports whether the xattr command needs sudo for
// the file described by info: macOS requires write permission to change its
// attributes, and the command does not grant it.
func attributesNeedPrivilege(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return !ok || int(stat.Uid) != os.Getuid() || info.Mode().Perm()&0o200 == 0
}
