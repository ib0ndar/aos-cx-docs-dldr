package library

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/storage"
)

var foreignJournal = []byte(`{"owner":"foreign","do_not_change":true}`)

func replaceOwnership(t *testing.T, r *Run, sameInode bool) []byte {
	t.Helper()
	owner := r.owner
	owner.Token = "different-owner"
	data, err := jsonBytes(owner)
	if err != nil {
		t.Fatal(err)
	}
	if sameInode {
		err = os.WriteFile(filepath.Join(r.Destination, filepath.FromSlash(r.lock)), data, 0o600)
	} else {
		err = storage.Atomic(r.root, r.lock, data)
	}
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPublishRefusesLostLockBeforeSharedMutation(t *testing.T) {
	for _, sameInode := range []bool{false, true} {
		name := "replaced-inode"
		if sameInode {
			name = "changed-token"
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			target, _ := publishPDF(t, base, "previous")
			before, err := os.ReadFile(filepath.Join(target, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			r, err := Open(base, "6300", "10.10")
			if err != nil {
				t.Fatal(err)
			}
			acceptPDF(t, r, "job", "replacement")
			lock := replaceOwnership(t, r, sameInode)
			if err := storage.Atomic(r.root, r.journal, foreignJournal); err != nil {
				t.Fatal(err)
			}
			_, publishErr := r.Publish(false)
			closeErr := r.Close()
			if publishErr == nil || !strings.Contains(publishErr.Error(), "ownership") {
				t.Errorf("publication was not refused after ownership loss: publish=%v close=%v", publishErr, closeErr)
			}
			after, _ := os.ReadFile(filepath.Join(target, "manifest.json"))
			journal, _ := os.ReadFile(filepath.Join(base, "6300", ".10.10.transaction.json"))
			currentLock, _ := os.ReadFile(filepath.Join(base, "6300", ".10.10.lock"))
			if !bytes.Equal(before, after) || !bytes.Equal(journal, foreignJournal) || !bytes.Equal(lock, currentLock) {
				t.Error("shared target or foreign journal/lock changed")
			}
			if _, err := os.Stat(r.Stage); err != nil {
				t.Error("prepared staging was not retained")
			}
			snapshots, _ := filepath.Glob(filepath.Join(base, "6300", ".snapshots", "10.10", "*"))
			if len(snapshots) != 0 {
				t.Error("ownership-lost transaction changed the snapshots")
			}
		})
	}
}
