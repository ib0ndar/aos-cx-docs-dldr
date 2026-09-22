package cache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOldProductCacheIsRejectedWithoutMutation(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, ".aoscx-docs-cache-root")
	body := []byte(`{"application":"aoscx-docs","cache_schema_version":2}`)
	if err := os.WriteFile(marker, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Prepare(context.Background(), root); err == nil {
		t.Fatal("old-product cache was accepted")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(marker) {
		t.Fatalf("old cache file set changed: %v %v", entries, err)
	}
	after, err := os.ReadFile(marker)
	if err != nil || string(after) != string(body) {
		t.Fatalf("old cache marker changed: %v", err)
	}
}
