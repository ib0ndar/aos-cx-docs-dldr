package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func markerBytes(t *testing.T, marker rootMarker) []byte {
	t.Helper()
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func TestEmptyRootInitializationReopenAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "native-cache")
	if err := Prepare(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	rootInfo, err := os.Stat(dir)
	if err != nil || rootInfo.Mode().Perm() != 0o700 {
		t.Fatalf("cache root permissions are not private: mode=%v err=%v", rootInfo.Mode(), err)
	}
	markerPath := filepath.Join(dir, RootMarkerName)
	info, err := os.Stat(markerPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cache marker permissions are not private: mode=%v err=%v", info.Mode(), err)
	}
	before, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, markerBytes(t, expectedRootMarker)) {
		t.Fatalf("wrong cache marker: %s", before)
	}
	if err := Prepare(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(markerPath)
	if !bytes.Equal(before, after) {
		t.Fatal("reopening rewrote the native cache marker")
	}
}

func TestConcurrentEmptyRootInitialization(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "concurrent-cache")
	const count = 16
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- Prepare(context.Background(), dir)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != RootMarkerName {
		t.Fatalf("concurrent admission left unexpected files: %v err=%v", entries, err)
	}
}

func TestNonemptyUnmarkedRootsAreRejectedUnchanged(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		files map[string][]byte
	}{
		{"arbitrary", map[string][]byte{"keep.txt": []byte("user data")}},
		{"python-like", map[string][]byte{
			"metadata.json": []byte(`{"url":"https://docs.example/guide","sha256":"python"}`),
			"body.bin":      []byte("python cache bytes"),
		}},
		{"old-raw-v1", map[string][]byte{
			strings.Repeat("a", 64) + ".json": []byte(`{"schema":1}`),
			strings.Repeat("b", 64) + ".bin":  []byte("old native bytes"),
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, data := range fixture.files {
				if err := os.WriteFile(filepath.Join(dir, name), data, 0o640); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotDirectory(t, dir)
			err := Prepare(context.Background(), dir)
			if err == nil || !strings.Contains(err.Error(), "existing state is preserved") ||
				!strings.Contains(err.Error(), "new empty cache path") {
				t.Fatalf("unmarked root error is not actionable: %v", err)
			}
			if after := snapshotDirectory(t, dir); !bytes.Equal(before, after) {
				t.Fatalf("unmarked cache changed:\nbefore=%s\nafter=%s", before, after)
			}
		})
	}
}

func TestWrongNewerAndMalformedMarkersAreRejectedUnchanged(t *testing.T) {
	for _, fixture := range []struct {
		name string
		data []byte
	}{
		{"wrong-application", markerBytes(t, rootMarker{Application: "other", CacheSchemaVersion: RootSchema})},
		{"older-schema", markerBytes(t, rootMarker{Application: Application, CacheSchemaVersion: RootSchema - 1})},
		{"newer-schema", markerBytes(t, rootMarker{Application: Application, CacheSchemaVersion: RootSchema + 1})},
		{"malformed", []byte("{")},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, RootMarkerName)
			if err := os.WriteFile(path, fixture.data, 0o600); err != nil {
				t.Fatal(err)
			}
			before := snapshotDirectory(t, dir)
			if err := Prepare(context.Background(), dir); err == nil {
				t.Fatal("incompatible marker was accepted")
			}
			if after := snapshotDirectory(t, dir); !bytes.Equal(before, after) {
				t.Fatal("incompatible marker or neighboring contents changed")
			}
		})
	}
}

func TestNonprivateMarkerIsRejectedWithoutPermissionRepair(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, RootMarkerName)
	if err := os.WriteFile(marker, markerBytes(t, expectedRootMarker), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshotDirectory(t, dir)
	if err := Prepare(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "expected private 0600") {
		t.Fatalf("nonprivate marker was accepted: %v", err)
	}
	if after := snapshotDirectory(t, dir); !bytes.Equal(before, after) {
		t.Fatal("nonprivate marker was silently repaired")
	}
}

func TestMarkerAndRootSymlinksOrSpecialFilesAreRejected(t *testing.T) {
	t.Run("root-file", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "cache")
		if err := os.WriteFile(root, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := Prepare(context.Background(), root); err == nil {
			t.Fatal("regular-file cache root accepted")
		}
	})
	t.Run("root-symlink", func(t *testing.T) {
		parent := t.TempDir()
		target := filepath.Join(parent, "target")
		link := filepath.Join(parent, "cache")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Skip(err)
		}
		if err := Prepare(context.Background(), link); err == nil {
			t.Fatal("symlink cache root accepted")
		}
	})
	for _, kind := range []string{"symlink", "fifo"} {
		t.Run("marker-"+kind, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, RootMarkerName)
			var err error
			if kind == "symlink" {
				target := filepath.Join(dir, "keep")
				err = os.WriteFile(target, []byte("keep"), 0o600)
				if err == nil {
					err = os.Symlink(target, marker)
				}
			} else {
				err = syscall.Mkfifo(marker, 0o600)
			}
			if err != nil {
				t.Skip(err)
			}
			before := snapshotDirectory(t, dir)
			if err := Prepare(context.Background(), dir); err == nil {
				t.Fatal("unsafe cache marker accepted")
			}
			if after := snapshotDirectory(t, dir); !bytes.Equal(before, after) {
				t.Fatal("unsafe marker root changed")
			}
		})
	}
}

func TestCancelledInitializationLeavesNoMarkerOrTemporaryFile(t *testing.T) {
	t.Run("before-create", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "cache")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := Prepare(ctx, dir); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cancelled preparation created cache root: %v", err)
		}
	})
	t.Run("while-peer-initializes", func(t *testing.T) {
		dir := t.TempDir()
		part := filepath.Join(dir, "."+RootMarkerName+"-peer.part")
		if err := os.WriteFile(part, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer cancel()
		before := snapshotDirectory(t, dir)
		if err := Prepare(ctx, dir); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("initialization wait cancellation lost: %v", err)
		}
		if after := snapshotDirectory(t, dir); !bytes.Equal(before, after) {
			t.Fatal("cancelled marker wait altered peer state")
		}
	})
}

func TestMarkerWriteFailureAndCancellationLeaveEmptyRoot(t *testing.T) {
	for _, mode := range []string{"failure", "cancellation"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			writeErr := errors.New("injected marker write failure")
			err = initializeRootMarkerWithWriter(ctx, root, func(ctx context.Context, file *os.File, data []byte) error {
				if _, err := file.Write(data[:len(data)/2]); err != nil {
					return err
				}
				if mode == "cancellation" {
					cancel()
					return ctx.Err()
				}
				return writeErr
			})
			closeErr := root.Close()
			if mode == "failure" && !errors.Is(err, writeErr) {
				t.Fatalf("marker write failure lost: %v", err)
			}
			if mode == "cancellation" && !errors.Is(err, context.Canceled) {
				t.Fatalf("marker write cancellation lost: %v", err)
			}
			if closeErr != nil {
				t.Fatal(closeErr)
			}
			entries, readErr := os.ReadDir(dir)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("failed marker write left output: entries=%v err=%v", entries, readErr)
			}
		})
	}
}

func TestRawV1RemainsUntouchedWhenRawV2IsInitialized(t *testing.T) {
	parent := t.TempDir()
	rawV1 := filepath.Join(parent, "raw-v1")
	rawV2 := filepath.Join(parent, "raw-v2")
	if err := os.Mkdir(rawV1, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rawV1, "legacy.bin"), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotDirectory(t, rawV1)
	if err := Prepare(context.Background(), rawV2); err != nil {
		t.Fatal(err)
	}
	if after := snapshotDirectory(t, rawV1); !bytes.Equal(before, after) {
		t.Fatal("raw-v1 data changed while initializing raw-v2")
	}
}

func snapshotDirectory(t *testing.T, root string) []byte {
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
		result.WriteString(relative)
		result.WriteString("|")
		result.WriteString(info.Mode().String())
		result.WriteString("|")
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			result.Write(data)
		} else if info.Mode()&os.ModeSymlink != 0 {
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
