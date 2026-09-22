package library

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func publishFinderHTMLFixture(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	run, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	acceptHTML(t, run, "html")
	if _, err := run.Publish(false); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "6300", "10.10")
	if err := os.MkdirAll(filepath.Join(target, "html", "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	return base, target
}

func writeFinderMetadata(t *testing.T, directory string, body []byte) string {
	t.Helper()
	name := filepath.Join(directory, ".DS_Store")
	if err := os.WriteFile(name, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestFinderMetadataIsIgnoredOmittedFromStageAndRetainedInSnapshot(t *testing.T) {
	base, target := publishFinderHTMLFixture(t)
	body := bytes.Repeat([]byte("finder-metadata"), 128)
	modTime := time.Unix(1_700_000_000, 0)
	var metadata []string
	for _, relative := range []string{"", "html", "html/pages", "html/assets"} {
		name := writeFinderMetadata(t, filepath.Join(target, filepath.FromSlash(relative)), body)
		if err := os.Chtimes(name, modTime, modTime); err != nil {
			t.Fatal(err)
		}
		metadata = append(metadata, name)
	}

	zipPath, err := ExportZIP(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range reader.File {
		if strings.Contains(member.Name, ".DS_Store") {
			reader.Close()
			t.Fatalf("Finder metadata entered ZIP: %s", member.Name)
		}
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}

	run, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range metadata {
		got, err := os.ReadFile(source)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("source Finder metadata changed before publication: %s err=%v", source, err)
		}
		info, err := os.Stat(source)
		if err != nil || !info.ModTime().Equal(modTime) || info.Mode().Perm() != 0o600 {
			t.Fatalf("source Finder metadata attributes changed: %s info=%v err=%v", source, info, err)
		}
		relative, err := filepath.Rel(target, source)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(run.Stage, relative)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Finder metadata propagated into stage: %s err=%v", relative, err)
		}
	}
	if _, err := run.Publish(false); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(run.Destination, filepath.FromSlash(path.Join(run.parent, ".snapshots", path.Base(run.target), run.stamp)))
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	for _, source := range metadata {
		relative, err := filepath.Rel(target, source)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("normal atomic replacement retained Finder metadata: %s err=%v", source, err)
		}
		got, err := os.ReadFile(filepath.Join(snapshot, relative))
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("snapshot did not preserve Finder metadata: %s err=%v", relative, err)
		}
	}
}

func TestFinderMetadataPublicationRollbackRestoresOriginal(t *testing.T) {
	base := t.TempDir()
	target, _ := publishPDF(t, base, "original")
	body := []byte("original Finder metadata")
	name := writeFinderMetadata(t, target, body)
	run, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(run.Stage, ".DS_Store")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Finder metadata propagated into stage: %v", err)
	}
	run.rename = func(from, to string) error {
		if from == run.stage {
			return errors.New("injected publication failure")
		}
		return run.root.Rename(from, to)
	}
	if _, err := run.Publish(false); err == nil {
		t.Fatal("injected publication failure was hidden")
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(name)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("rollback did not restore Finder metadata: got=%q err=%v", got, err)
	}
}

func TestUnsafeOrUnexpectedFinderLikeEntriesRemainRejected(t *testing.T) {
	tests := []struct {
		name   string
		create func(*testing.T, string) string
	}{
		{"directory", func(t *testing.T, root string) string {
			name := filepath.Join(root, ".DS_Store")
			if err := os.Mkdir(name, 0o700); err != nil {
				t.Fatal(err)
			}
			return name
		}},
		{"oversized", func(t *testing.T, root string) string {
			name := writeFinderMetadata(t, root, nil)
			if err := os.Truncate(name, finderMetadataMaxBytes+1); err != nil {
				t.Fatal(err)
			}
			return name
		}},
		{"wrong-case", func(t *testing.T, root string) string {
			name := filepath.Join(root, ".ds_store")
			if err := os.WriteFile(name, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			return name
		}},
		{"arbitrary-hidden", func(t *testing.T, root string) string {
			name := filepath.Join(root, ".hidden")
			if err := os.WriteFile(name, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			return name
		}},
		{"apple-double", func(t *testing.T, root string) string {
			name := filepath.Join(root, "._manifest.json")
			if err := os.WriteFile(name, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			return name
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base, target := publishFinderHTMLFixture(t)
			name := test.create(t, filepath.Join(target, "html", "pages"))
			if _, err := Open(base, "6300", "10.10"); err == nil {
				t.Fatal("unsafe or unexpected Finder-like entry was accepted")
			}
			if _, err := os.Lstat(name); err != nil {
				t.Fatalf("rejected source entry was modified: %v", err)
			}
		})
	}
}

func TestFinderMetadataSymlinkAndMissingExpectedFileRemainRejected(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		base, target := publishFinderHTMLFixture(t)
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(target, "html", ".DS_Store")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(base, "6300", "10.10"); err == nil {
			t.Fatal("Finder metadata symlink was accepted")
		}
		got, err := os.ReadFile(outside)
		if err != nil || string(got) != "outside" {
			t.Fatalf("symlink target changed: got=%q err=%v", got, err)
		}
	})
	t.Run("missing expected file", func(t *testing.T) {
		base, target := publishFinderHTMLFixture(t)
		writeFinderMetadata(t, target, []byte("finder"))
		if err := os.Remove(filepath.Join(target, "search.js")); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(base, "6300", "10.10"); err == nil {
			t.Fatal("Finder metadata masked a missing expected file")
		}
	})
	t.Run("case collision", func(t *testing.T) {
		base, target := publishFinderHTMLFixture(t)
		writeFinderMetadata(t, target, []byte("finder"))
		if err := os.Rename(filepath.Join(target, "index.html"), filepath.Join(target, "Index.html")); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(base, "6300", "10.10"); err == nil {
			t.Fatal("Finder metadata masked an expected-file case collision")
		}
	})
}
