package storage

import (
	"bytes"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// managedFixture creates base/published/g/x, the file a redirected operation
// would damage, and base/stage/g/x, the file it should reach.
func managedFixture(t *testing.T) (string, *os.Root) {
	t.Helper()
	base := t.TempDir()
	for _, tree := range []string{"published", "stage"} {
		if err := os.MkdirAll(filepath.Join(base, tree, "g", "pages"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, tree, "g", "x"), []byte(tree), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return base, root
}

func assertPublishedIntact(t *testing.T, base string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(base, "published", "g", "x"))
	if err != nil || !bytes.Equal(data, []byte("published")) {
		t.Fatalf("published file changed through a redirected path: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(base, "published", "g", "pages")); err != nil {
		t.Fatalf("published directory removed through a redirected path: %v", err)
	}
}

// swapForSymlink moves name aside and replaces it with a symlink to target,
// as another writer could between a check and its use.
func swapForSymlink(t *testing.T, base, name, target string) {
	t.Helper()
	full := filepath.Join(base, filepath.FromSlash(name))
	if err := os.Rename(full, full+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		t.Skip(err)
	}
}

func TestManagedMutationRefusesInRootSymlinkComponents(t *testing.T) {
	base, root := managedFixture(t)
	if err := os.Symlink("published", filepath.Join(base, "link")); err != nil {
		t.Skip(err)
	}
	for name, run := range map[string]func() error{
		"OpenDir":     func() error { dir, err := OpenDir(root, "link/g"); closeIfOpened(dir); return err },
		"EnsureDir":   func() error { return EnsureDir(root, "link/g/new") },
		"Atomic":      func() error { return Atomic(root, "link/g/x", []byte("replaced")) },
		"RemoveTree":  func() error { return RemoveTree(root, "link/g") },
		"Remove":      func() error { return Remove(root, "link/g/x") },
		"CopyTree":    func() error { return CopyTree(root, "link/g", "copy") },
		"CopyTreeOut": func() error { return CopyTree(root, "stage/g", "link/g/copy") },
	} {
		if err := run(); err == nil {
			t.Errorf("%s followed a symlink that resolves inside the root", name)
		}
	}
	assertPublishedIntact(t, base)
	if _, err := os.Lstat(filepath.Join(base, "published", "g", "copy")); err == nil {
		t.Fatal("copy was written through a symlinked target component")
	}
}

func closeIfOpened(dir *os.Root) {
	if dir != nil {
		dir.Close()
	}
}

func TestOpenedManagedDirectoryIgnoresLaterComponentSwap(t *testing.T) {
	base, root := managedFixture(t)
	guide, err := EnsureDirRoot(root, "stage/g")
	if err != nil {
		t.Fatal(err)
	}
	defer guide.Close()
	swapForSymlink(t, base, "stage", "published")
	if err := RemoveTree(guide, "pages"); err != nil {
		t.Fatal(err)
	}
	if err := Atomic(guide, "x", []byte("rewritten")); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(guide, "assets"); err != nil {
		t.Fatal(err)
	}
	assertPublishedIntact(t, base)
	moved := filepath.Join(base, "stage-moved", "g")
	if _, err := os.Stat(filepath.Join(moved, "pages")); !os.IsNotExist(err) {
		t.Fatalf("removal did not reach the opened directory: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(moved, "x")); err != nil || string(data) != "rewritten" {
		t.Fatalf("write did not reach the opened directory: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(moved, "assets")); err != nil {
		t.Fatalf("directory was not created in the opened directory: %v", err)
	}
}

func TestOpenVerifiedDirRefusesSwapAfterLstat(t *testing.T) {
	base, root := managedFixture(t)
	info, err := root.Lstat("stage")
	if err != nil {
		t.Fatal(err)
	}
	swapForSymlink(t, base, "stage", "published")
	dir, err := openVerifiedDir(root, "stage", "stage", info)
	closeIfOpened(dir)
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("directory replaced after Lstat was opened: %v", err)
	}
	if err := removeTreeEntry(root, "stage", "stage"); err == nil {
		t.Fatal("symlink replacing a managed directory was removed as a tree")
	}
	assertPublishedIntact(t, base)
}

func TestCopyTreeRefusesEntriesSwappedDuringCopy(t *testing.T) {
	for _, swappedName := range []string{"stage/g/x", "stage/g/pages"} {
		t.Run(path.Base(swappedName), func(t *testing.T) {
			base, root := managedFixture(t)
			if err := os.WriteFile(filepath.Join(base, "published", "g", "pages", "secret"), []byte("published"), 0o600); err != nil {
				t.Fatal(err)
			}
			swapped := false
			err := CopyTreeFiltered(root, "stage", "copy", func(name string, info fs.FileInfo) (bool, error) {
				if name == swappedName && !swapped {
					swapped = true
					swapForSymlink(t, base, swappedName, "../../published/g/"+path.Base(swappedName))
				}
				return true, nil
			})
			if !swapped {
				t.Fatal("copy filter never reached the swapped entry")
			}
			if err == nil {
				t.Fatal("copy continued through an entry swapped for a symlink")
			}
			for _, copied := range []string{"x", "pages/secret"} {
				if data, readErr := os.ReadFile(filepath.Join(base, "copy", "g", filepath.FromSlash(copied))); readErr == nil &&
					bytes.Equal(data, []byte("published")) {
					t.Fatalf("copy read %s through a symlink swapped in after inspection", copied)
				}
			}
			assertPublishedIntact(t, base)
		})
	}
}

func TestCopyAndRemoveTreeKeepManagedContent(t *testing.T) {
	base, root := managedFixture(t)
	if err := os.WriteFile(filepath.Join(base, "stage", "g", "pages", "a.html"), []byte("page"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CopyTreeFiltered(root, "stage", "copy", func(name string, info fs.FileInfo) (bool, error) {
		return name != "stage/g/x", nil
	}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(base, "copy", "g", "pages", "a.html")); err != nil || string(data) != "page" {
		t.Fatalf("nested file not copied: %q %v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(base, "copy", "g", "x")); !os.IsNotExist(err) {
		t.Fatalf("filtered file was copied: %v", err)
	}
	if err := RemoveTree(root, "copy"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(base, "copy")); !os.IsNotExist(err) {
		t.Fatalf("tree not removed: %v", err)
	}
	if err := RemoveTree(root, "missing/child"); err != nil {
		t.Fatalf("removing a missing tree failed: %v", err)
	}
	assertPublishedIntact(t, base)
}
