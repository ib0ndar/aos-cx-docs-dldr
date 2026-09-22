package storage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestTreeDigestIsStableForIdenticalTrees(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "source")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "plain.bin"), []byte{0, 1, 2, 3}, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "empty"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	first, err := TreeDigest(context.Background(), root, "source")
	if err != nil {
		t.Fatal(err)
	}
	second, err := TreeDigest(context.Background(), root, "source")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("digest is not stable: first=%s second=%s", first, second)
	}
	if first == "" {
		t.Fatal("digest is empty")
	}
}

func TestTreeDigestBindsPathModeSizeAndContent(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "tree"), 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(base, "tree", "value")
	if err := os.WriteFile(file, []byte("same-size"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	initial, err := TreeDigest(context.Background(), root, "tree")
	if err != nil {
		t.Fatal(err)
	}
	mutations := []func() error{
		func() error { return os.WriteFile(file, []byte("diff-size"), 0o600) },
		func() error { return os.Chmod(file, 0o640) },
		func() error { return os.Rename(file, filepath.Join(base, "tree", "renamed")) },
	}
	previous := initial
	for index, mutate := range mutations {
		if err := mutate(); err != nil {
			t.Fatal(err)
		}
		digest, err := TreeDigest(context.Background(), root, "tree")
		if err != nil {
			t.Fatal(err)
		}
		if digest == previous {
			t.Fatalf("mutation %d did not change digest", index)
		}
		previous = digest
	}
}

func TestTreeDigestRejectsUnsafeEntriesAndCancellation(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			tree := filepath.Join(base, "tree")
			if err := os.Mkdir(tree, 0o700); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(tree, "unsafe")
			var err error
			if kind == "symlink" {
				err = os.Symlink(filepath.Join(base, "outside"), name)
			} else {
				err = syscall.Mkfifo(name, 0o600)
			}
			if err != nil {
				t.Skip(err)
			}
			root, err := os.OpenRoot(base)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if _, err := TreeDigest(context.Background(), root, "tree"); err == nil {
				t.Fatal("unsafe entry was hashed")
			}
		})
	}

	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "tree"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "tree", "large"), bytes.Repeat([]byte("x"), 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := TreeDigest(ctx, root, "tree"); !errors.Is(err, context.Canceled) {
		t.Fatalf("digest cancellation lost: %v", err)
	}
}

func TestTreeDigestRejectsCaseFoldAndNormalizationCollisions(t *testing.T) {
	for _, names := range [][]string{
		{"Guide", "guide"},
		{"é", "e\u0301"},
		{"σ", "ς"},
	} {
		t.Run(strings.Join(names, "-"), func(t *testing.T) {
			base := t.TempDir()
			tree := filepath.Join(base, "tree")
			if err := os.Mkdir(tree, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, name := range names {
				if err := os.WriteFile(filepath.Join(tree, name), []byte(name), 0o600); err != nil {
					t.Skipf("filesystem does not permit collision fixture: %v", err)
				}
			}
			entries, err := os.ReadDir(tree)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != len(names) {
				t.Skip("filesystem normalizes or folds the collision fixture")
			}
			root, err := os.OpenRoot(base)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if _, err := TreeDigest(context.Background(), root, "tree"); err == nil ||
				!strings.Contains(err.Error(), "collision") {
				t.Fatalf("collision was not rejected: %v", err)
			}
		})
	}
}

func TestFoldedManagedPathUsesFullUnicodeCaseFolding(t *testing.T) {
	for _, pair := range [][2]string{
		{"σ", "ς"},
		{"Straße", "STRASSE"},
	} {
		if foldedManagedPath(pair[0]) != foldedManagedPath(pair[1]) {
			t.Fatalf("%q and %q did not share a full case-fold key", pair[0], pair[1])
		}
	}
}

func TestTreeDigestRejectsFileReplacementBetweenLstatAndOpen(t *testing.T) {
	base := t.TempDir()
	tree := filepath.Join(base, "tree")
	if err := os.Mkdir(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(tree, "value")
	if err := os.WriteFile(source, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	replaced := false
	_, err = treeDigestWithHook(context.Background(), root, "tree", func(point, name string) error {
		if point != "digest-file-before-open" || name != "tree/value" {
			return nil
		}
		replacement := filepath.Join(tree, "replacement")
		if err := os.WriteFile(replacement, []byte("new-data"), 0o600); err != nil {
			return err
		}
		replaced = true
		return os.Rename(replacement, source)
	})
	if !replaced || err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("replacement inode was not rejected: replaced=%t err=%v", replaced, err)
	}
}

func TestTreeDigestRejectsDirectoryReplacementAfterRead(t *testing.T) {
	base := t.TempDir()
	tree := filepath.Join(base, "tree")
	if err := os.Mkdir(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "value"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	replaced := false
	_, err = treeDigestWithHook(context.Background(), root, "tree", func(point, name string) error {
		if point != "digest-directory-after-read" || name != "tree" {
			return nil
		}
		if err := os.Rename(tree, filepath.Join(base, "old-tree")); err != nil {
			return err
		}
		replaced = true
		return os.Mkdir(tree, 0o700)
	})
	if !replaced || err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("directory replacement was not rejected: replaced=%t err=%v", replaced, err)
	}
}
