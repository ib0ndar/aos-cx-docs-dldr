package library

import (
	"bytes"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/storage"
	"aos-cx-docs-dldr/internal/testutil"
)

func prepareReplacement(t *testing.T) *Run {
	t.Helper()
	base := t.TempDir()
	publishPDF(t, base, "previous")
	r, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	acceptPDF(t, r, "job", "replacement")
	return r
}

func readBytes(t *testing.T, filename string) []byte {
	t.Helper()
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertPreviousSnapshot(t *testing.T, r *Run, snapshot string) {
	t.Helper()
	data := readBytes(t, filepath.Join(r.Destination, filepath.FromSlash(snapshot), "job", "6300 - 10.10 - job Guide.pdf"))
	if !bytes.Equal(data, testutil.PDF("previous")) {
		t.Fatal("previous complete PDF was changed")
	}
}

func TestPublishRefusesReplacedLockWithIdenticalOwner(t *testing.T) {
	r := prepareReplacement(t)
	before := readBytes(t, filepath.Join(r.Target, "manifest.json"))
	owner := readBytes(t, filepath.Join(r.Destination, r.lock))
	if err := storage.Atomic(r.root, r.lock, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Publish(false); err == nil {
		t.Fatal("same token on a replacement inode was accepted as ownership")
	}
	if !bytes.Equal(before, readBytes(t, filepath.Join(r.Target, "manifest.json"))) {
		t.Fatal("target changed after lock inode replacement")
	}
}

func TestPublishRefusesExistingForeignJournal(t *testing.T) {
	r := prepareReplacement(t)
	before := readBytes(t, filepath.Join(r.Target, "manifest.json"))
	if err := storage.Atomic(r.root, r.journal, foreignJournal); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Publish(false); err == nil {
		t.Fatal("existing foreign journal was overwritten")
	}
	if !bytes.Equal(before, readBytes(t, filepath.Join(r.Target, "manifest.json"))) ||
		!bytes.Equal(foreignJournal, readBytes(t, filepath.Join(r.Destination, r.journal))) {
		t.Fatal("shared state changed despite foreign journal")
	}
}

func TestOwnershipLossAfterSnapshotStopsBeforeCommit(t *testing.T) {
	r := prepareReplacement(t)
	var snapshot string
	var foreignLock []byte
	r.rename = func(from, to string) error {
		if err := r.root.Rename(from, to); err != nil {
			return err
		}
		if from == r.target {
			snapshot = to
			foreignLock = replaceOwnership(t, r, false)
			if err := storage.Atomic(r.root, r.journal, foreignJournal); err != nil {
				return err
			}
			if err := os.Mkdir(r.Target, 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(r.Target, "peer.txt"), []byte("peer target"), 0o600)
		}
		return nil
	}
	_, err := r.Publish(false)
	if err == nil || !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("ownership loss at snapshot boundary was ignored: %v", err)
	}
	assertPreviousSnapshot(t, r, snapshot)
	if string(readBytes(t, filepath.Join(r.Target, "peer.txt"))) != "peer target" {
		t.Fatal("new owner's target was overwritten")
	}
	if _, err := os.Stat(r.Stage); err != nil {
		t.Fatal("uncommitted staging was not retained")
	}
	if err := r.Close(); err == nil {
		t.Fatal("foreign lock ownership was not reported on cleanup")
	}
	if !bytes.Equal(foreignLock, readBytes(t, filepath.Join(r.Destination, r.lock))) ||
		!bytes.Equal(foreignJournal, readBytes(t, filepath.Join(r.Destination, r.journal))) {
		t.Fatal("foreign transaction files were touched after snapshot boundary")
	}
}

func TestRollbackRequiresUnchangedLockAndJournal(t *testing.T) {
	for _, mode := range []string{"lock", "journal-inode", "journal-content", "foreign-target"} {
		t.Run(mode, func(t *testing.T) {
			r := prepareReplacement(t)
			var snapshot string
			var retainedJournal []byte
			r.rename = func(from, to string) error {
				if from != r.stage {
					if from == r.target {
						snapshot = to
					}
					return r.root.Rename(from, to)
				}
				retainedJournal = readBytes(t, filepath.Join(r.Destination, r.journal))
				switch mode {
				case "lock":
					replaceOwnership(t, r, true)
					retainedJournal = foreignJournal
					if err := storage.Atomic(r.root, r.journal, retainedJournal); err != nil {
						return err
					}
				case "journal-inode":
					if err := storage.Atomic(r.root, r.journal, retainedJournal); err != nil {
						return err
					}
				case "journal-content":
					retainedJournal = foreignJournal
					if err := os.WriteFile(filepath.Join(r.Destination, r.journal), retainedJournal, 0o600); err != nil {
						return err
					}
				case "foreign-target":
					if err := os.Mkdir(r.Target, 0o700); err != nil {
						return err
					}
					if err := os.WriteFile(filepath.Join(r.Target, "peer.txt"), []byte("keep"), 0o600); err != nil {
						return err
					}
				}
				return errors.New("injected commit failure")
			}
			if _, err := r.Publish(false); err == nil {
				t.Fatal("faulty transaction reported success")
			}
			assertPreviousSnapshot(t, r, snapshot)
			if !bytes.Equal(retainedJournal, readBytes(t, filepath.Join(r.Destination, r.journal))) {
				t.Fatal("rollback overwrote or removed the changed journal")
			}
			if mode == "foreign-target" {
				if string(readBytes(t, filepath.Join(r.Target, "peer.txt"))) != "keep" {
					t.Fatal("rollback replaced another target")
				}
			} else if _, err := os.Stat(r.Target); !os.IsNotExist(err) {
				t.Fatal("rollback ran after losing ownership")
			}
			if _, err := os.Stat(r.Stage); err != nil {
				t.Fatal("failed replacement staging lost")
			}
		})
	}
}

func TestOwnershipLossAfterCommitPreservesPeerJournal(t *testing.T) {
	r := prepareReplacement(t)
	var snapshot string
	r.rename = func(from, to string) error {
		if err := r.root.Rename(from, to); err != nil {
			return err
		}
		if from == r.target {
			snapshot = to
		}
		if from == r.stage {
			replaceOwnership(t, r, false)
			if err := storage.Atomic(r.root, r.journal, foreignJournal); err != nil {
				return err
			}
		}
		return nil
	}
	if _, err := r.Publish(false); err == nil {
		t.Fatal("ownership loss after commit was reported as owned success")
	}
	assertPreviousSnapshot(t, r, snapshot)
	current := readBytes(t, filepath.Join(r.Target, "job", "6300 - 10.10 - job Guide.pdf"))
	if !bytes.Equal(current, testutil.PDF("replacement")) {
		t.Fatal("committed target changed after ownership was lost")
	}
	r.Close()
	if !bytes.Equal(foreignJournal, readBytes(t, filepath.Join(r.Destination, r.journal))) {
		t.Fatal("post-commit cleanup removed another journal")
	}
}

func TestRecoveryStopsOnOwnershipOrJournalReplacement(t *testing.T) {
	for _, mode := range []string{"lock", "journal"} {
		t.Run(mode, func(t *testing.T) {
			r := prepareReplacement(t)
			r.rename = func(from, to string) error {
				if from == r.stage || strings.Contains(from, ".snapshots/") {
					return errors.New("injected interrupted publication")
				}
				return r.root.Rename(from, to)
			}
			if _, err := r.Publish(false); err == nil {
				t.Fatal("expected interrupted publication")
			}
			var tx journal
			if err := storage.ReadJSON(r.root, r.journal, &tx); err != nil {
				t.Fatal(err)
			}
			r.rename = func(from, to string) error {
				if err := r.root.Rename(from, to); err != nil {
					return err
				}
				if mode == "lock" {
					replaceOwnership(t, r, true)
				}
				return storage.Atomic(r.root, r.journal, foreignJournal)
			}
			if err := r.recover(); err == nil {
				t.Fatal("recovery did not detect ownership/journal change")
			}
			assertPreviousSnapshot(t, r, tx.PreviousSnapshot)
			if !bytes.Equal(foreignJournal, readBytes(t, filepath.Join(r.Destination, r.journal))) {
				t.Fatal("recovery removed a replacement journal")
			}
		})
	}
}

func TestJournalCreationNeverReplacesExistingFile(t *testing.T) {
	r := prepareReplacement(t)
	if err := storage.Atomic(r.root, r.journal, foreignJournal); err != nil {
		t.Fatal(err)
	}
	if _, err := r.createJournal(journal{RunID: r.stamp}); err == nil {
		t.Fatal("exclusive journal creation overwrote an existing file")
	}
	if !bytes.Equal(foreignJournal, readBytes(t, filepath.Join(r.Destination, filepath.FromSlash(path.Clean(r.journal))))) {
		t.Fatal("foreign journal bytes changed")
	}
}
