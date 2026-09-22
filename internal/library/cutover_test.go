package library

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/model"
)

func writeCutoverFixture(t *testing.T, base, relative string, data []byte) {
	t.Helper()
	name := filepath.Join(base, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, data, 0o640); err != nil {
		t.Fatal(err)
	}
}

func cutoverSnapshot(t *testing.T, root string) []byte {
	t.Helper()
	var result bytes.Buffer
	err := filepath.WalkDir(root, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		info, err := os.Lstat(name)
		if err != nil {
			return err
		}
		result.WriteString(relative + "|" + info.Mode().String() + "|")
		switch {
		case info.Mode().IsRegular():
			data, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			result.Write(data)
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(name)
			if err != nil {
				return err
			}
			result.WriteString(target)
		}
		result.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}

func assertNoCutoverArtifacts(t *testing.T, base string, allowJournal bool) {
	t.Helper()
	parent := filepath.Join(base, "6300")
	for _, name := range []string{".10.16.lock", ".staging", ".snapshots", ".incomplete"} {
		if _, err := os.Lstat(filepath.Join(parent, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rejected state left managed artifact %s: %v", name, err)
		}
	}
	if !allowJournal {
		if _, err := os.Lstat(filepath.Join(parent, ".10.16.transaction.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rejected state created a journal: %v", err)
		}
	}
}

func TestPythonLikeAndIncompatibleLibrariesAreRejectedReadOnlyBeforeLock(t *testing.T) {
	for _, fixture := range []struct {
		name     string
		manifest []byte
		symlink  bool
	}{
		{"python-complete", []byte(`{"application":"aoscx-docs","application_version":"0.5","schema_version":1,"status":"complete"}`), false},
		{"python-incomplete", []byte(`{"application":"aoscx-docs","application_version":"0.5","schema_version":1,"status":"incomplete"}`), false},
		{"malformed", []byte("{"), false},
		{"newer-native", []byte(`{"application":"aos-cx-docs-dldr","native_schema_version":2}`), false},
		{"wrong-application", []byte(`{"application":"other","native_schema_version":1}`), false},
		{"old-product-current-version", []byte(`{"application":"aoscx-docs","application_version":"` + model.Version + `","schema_version":1,"native_schema_version":1,"status":"complete"}`), false},
		{"symlink-target", nil, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			base := t.TempDir()
			target := filepath.Join(base, "6300", "10.16")
			if fixture.symlink {
				outside := t.TempDir()
				writeCutoverFixture(t, outside, "manifest.json", []byte(`{"application":"aoscx-docs","native_schema_version":1}`))
				if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, target); err != nil {
					t.Skip(err)
				}
			} else {
				writeCutoverFixture(t, base, "6300/10.16/manifest.json", fixture.manifest)
				writeCutoverFixture(t, base, "6300/10.16/user-data.bin", []byte{0, 1, 2, 3})
			}
			before := cutoverSnapshot(t, base)
			run, err := Open(base, "6300", "10.16")
			if run != nil {
				run.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "the existing state is preserved") ||
				!strings.Contains(err.Error(), "only version "+model.Version+" libraries") ||
				!strings.Contains(err.Error(), "choose a new base") {
				t.Fatalf("library rejection is not actionable: %v", err)
			}
			if after := cutoverSnapshot(t, base); !bytes.Equal(before, after) {
				t.Fatalf("rejected library changed:\nbefore=%s\nafter=%s", before, after)
			}
			assertNoCutoverArtifacts(t, base, false)
		})
	}
}

func TestPythonLikeJournalsWithAbsentOrPresentTargetAreRejectedReadOnly(t *testing.T) {
	for _, targetPresent := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent-target", true: "present-target"}[targetPresent], func(t *testing.T) {
			base := t.TempDir()
			writeCutoverFixture(t, base, "6300/.10.16.transaction.json",
				[]byte(`{"previous_snapshot":".snapshots/10.16/interrupted"}`))
			if targetPresent {
				writeCutoverFixture(t, base, "6300/10.16/manifest.json",
					[]byte(`{"application":"aoscx-docs","schema_version":1,"status":"complete"}`))
			}
			before := cutoverSnapshot(t, base)
			run, err := Open(base, "6300", "10.16")
			if run != nil {
				run.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "manual inspection with its original tool") {
				t.Fatalf("journal rejection is not actionable: %v", err)
			}
			if after := cutoverSnapshot(t, base); !bytes.Equal(before, after) {
				t.Fatal("rejected Python journal or target changed")
			}
			assertNoCutoverArtifacts(t, base, true)
		})
	}
}

func TestMalformedNativeJournalStillUsesOwnedRecoveryChecks(t *testing.T) {
	base := t.TempDir()
	tx := journal{
		Application: Application, NativeSchemaVersion: NativeSchema,
		RunID: "20260914T120000.000000000Z-native", Stage: "../outside",
	}
	data, err := json.Marshal(tx)
	if err != nil {
		t.Fatal(err)
	}
	writeCutoverFixture(t, base, "6300/.10.16.transaction.json", data)
	before := cutoverSnapshot(t, base)
	run, err := Open(base, "6300", "10.16")
	if run != nil {
		run.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "manual inspection required") {
		t.Fatalf("unsafe native journal did not reach normal recovery validation: %v", err)
	}
	if after := cutoverSnapshot(t, base); !bytes.Equal(before, after) {
		t.Fatal("malformed native journal changed")
	}
	assertNoCutoverArtifacts(t, base, true)
}

func TestPublicationRechecksForIncompatibleTargetUnderOwnedLock(t *testing.T) {
	base := t.TempDir()
	run, err := Open(base, "6300", "10.16")
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	writeCutoverFixture(t, base, "6300/10.16/manifest.json",
		[]byte(`{"application":"aoscx-docs","schema_version":1,"status":"complete"}`))
	writeCutoverFixture(t, base, "6300/10.16/keep.txt", []byte("python state"))
	before := cutoverSnapshot(t, run.Target)
	if _, err := run.Publish(false); err == nil || !strings.Contains(err.Error(), "the existing state is preserved") {
		t.Fatalf("publication did not reject incompatible target: %v", err)
	}
	if after := cutoverSnapshot(t, run.Target); !bytes.Equal(before, after) {
		t.Fatal("publication recheck changed incompatible target")
	}
}
