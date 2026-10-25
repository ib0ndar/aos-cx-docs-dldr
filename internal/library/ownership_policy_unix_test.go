//go:build unix

package library

import (
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

type ownedDirectory struct {
	mode os.FileMode
	uid  int
}

func (d ownedDirectory) Name() string       { return "6300" }
func (d ownedDirectory) Size() int64        { return 0 }
func (d ownedDirectory) Mode() fs.FileMode  { return fs.ModeDir | d.mode }
func (d ownedDirectory) ModTime() time.Time { return time.Time{} }
func (d ownedDirectory) IsDir() bool        { return true }
func (d ownedDirectory) Sys() any           { return &syscall.Stat_t{Uid: uint32(d.uid)} }

func TestManagedDirectoryPolicy(t *testing.T) {
	const user, baseOwner, other = 501, 777, 4242
	for _, test := range []struct {
		name      string
		directory ownedDirectory
		modesKept bool
		refusal   string
	}{
		{"this user", ownedDirectory{0o700, user}, true, ""},
		{"root, as some bind mounts present every file", ownedDirectory{0o755, 0}, true, ""},
		{"destination owner", ownedDirectory{0o700, baseOwner}, true, ""},
		{"another account", ownedDirectory{0o700, other}, true, "owned by UID 4242"},
		{"another account where modes are synthesised", ownedDirectory{0o777, other}, false, "owned by UID 4242"},
		{"world writable", ownedDirectory{0o777, user}, true, "writable by other accounts"},
		{"group writable", ownedDirectory{0o770, user}, true, "writable by other accounts"},
		{"world writable with sticky bit", ownedDirectory{0o777 | os.ModeSticky, 0}, true, "writable by other accounts"},
		{"synthesised 0777 on a bind mount", ownedDirectory{0o777, user}, false, ""},
		{"synthesised 0777 root-owned bind mount", ownedDirectory{0o777, 0}, false, ""},
		{"read-only for others", ownedDirectory{0o755, user}, true, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := managedDirectoryPolicy("/library/6300", test.directory, user, baseOwner, test.modesKept)
			if test.refusal == "" && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if test.refusal != "" && (err == nil || !strings.Contains(err.Error(), test.refusal)) {
				t.Fatalf("expected refusal %q, got %v", test.refusal, err)
			}
		})
	}
}
