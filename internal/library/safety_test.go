package library

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"aos-cx-docs-dldr/internal/storage"
	"aos-cx-docs-dldr/internal/testutil"
)

func TestUnsafeJournalAndChangedLockOwnershipArePreserved(t *testing.T) {
	base := t.TempDir()
	r, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	tx := journal{NativeSchemaVersion: 1, RunID: r.stamp, Stage: "../outside", PreviousSnapshot: "../outside"}
	if err := storage.WriteJSON(r.root, r.journal, tx); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if _, err := Open(base, "6300", "10.10"); err == nil {
		t.Fatal("unsafe recovery path accepted")
	}
	if err := os.Remove(filepath.Join(base, "6300", ".10.10.transaction.json")); err != nil {
		t.Fatal(err)
	}
	r, err = Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Atomic(r.root, r.lock, []byte(`{"pid":123,"host":"fixture","token":"different"}`)); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err == nil {
		t.Fatal("changed lock ownership not reported")
	}
	if _, err := os.Stat(filepath.Join(base, "6300", ".10.10.lock")); err != nil {
		t.Fatal("another owner's lock was removed")
	}
}

func TestManagedPDFSymlinkIsNotFollowed(t *testing.T) {
	base := t.TempDir()
	target, m := publishPDF(t, base, "original")
	file := filepath.Join(target, "job", m.PDFGuides["job"].PDF.Path)
	outside := filepath.Join(t.TempDir(), "keep.bin")
	body := testutil.PDF("outside")
	os.WriteFile(outside, body, 0o600)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, file); err != nil {
		t.Skip(err)
	}
	if _, err := Open(base, "6300", "10.10"); err == nil {
		t.Fatal("managed PDF symlink accepted")
	}
	unchanged, _ := os.ReadFile(outside)
	if !bytes.Equal(unchanged, body) {
		t.Fatal("symlink target modified")
	}
}

func TestNewUnrelatedTargetDuringTransactionIsNotReplaced(t *testing.T) {
	base := t.TempDir()
	r, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	acceptPDF(t, r, "job", "staged")
	if err := os.Mkdir(r.Target, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Publish(false); err == nil {
		t.Fatal("new unrelated destination was replaced")
	}
	entries, err := os.ReadDir(r.Target)
	if err != nil || len(entries) != 0 {
		t.Fatal("new directory was changed")
	}
}
