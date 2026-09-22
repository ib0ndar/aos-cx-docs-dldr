//go:build darwin

package library

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestFinderMetadataFIFOIsRejected(t *testing.T) {
	base, target := publishFinderHTMLFixture(t)
	name := filepath.Join(target, "html", "assets", ".DS_Store")
	if err := syscall.Mkfifo(name, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(base, "6300", "10.10"); err == nil {
		t.Fatal("Finder metadata FIFO was accepted")
	}
	if info, err := os.Lstat(name); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("rejected FIFO was modified: info=%v err=%v", info, err)
	}
}
