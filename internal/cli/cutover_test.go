package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/fetch"
)

func TestInvalidExplicitCacheFailsBeforeTransportOrPublisherTraffic(t *testing.T) {
	root := t.TempDir()
	cachePath := filepath.Join(root, "python-cache")
	if err := os.Mkdir(cachePath, 0o700); err != nil {
		t.Fatal(err)
	}
	metadata := []byte(`{"url":"https://publisher.example/topic","sha256":"python"}`)
	metadataPath := filepath.Join(cachePath, "metadata.json")
	if err := os.WriteFile(metadataPath, metadata, 0o640); err != nil {
		t.Fatal(err)
	}
	called := false
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"--platform", "6300", "--release", "10.16", "--guides", "job",
		"--destination", filepath.Join(root, "library"), "--raw-cache", cachePath,
	}, &stdout, &stderr, func(string, fetch.Config, func(string)) (*fetch.Client, error) {
		called = true
		return nil, errors.New("transport must not be created")
	})
	if code != 1 || called || !strings.Contains(stderr.String(), "existing state is preserved") ||
		!strings.Contains(stderr.String(), "new empty cache path") {
		t.Fatalf("invalid cache was not rejected before transport: code=%d called=%t stderr=%s", code, called, &stderr)
	}
	after, err := os.ReadFile(metadataPath)
	if err != nil || !bytes.Equal(after, metadata) {
		t.Fatalf("invalid explicit cache changed: %v", err)
	}
}

func TestIncompatibleExplicitLibraryFailsBeforeTransportOrPublisherTraffic(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "library", "6300", "10.16")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`{"application":"aoscx-docs","schema_version":1,"status":"complete"}`)
	if err := os.WriteFile(filepath.Join(target, "manifest.json"), manifest, 0o640); err != nil {
		t.Fatal(err)
	}
	called := false
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"--platform", "6300", "--release", "10.16", "--guides", "job",
		"--destination", filepath.Join(root, "library"), "--raw-cache", filepath.Join(root, "cache"),
	}, &stdout, &stderr, func(string, fetch.Config, func(string)) (*fetch.Client, error) {
		called = true
		return nil, errors.New("transport must not be created")
	})
	if code != 1 || called || !strings.Contains(stderr.String(), "choose a new base") {
		t.Fatalf("incompatible library was not rejected before transport: code=%d called=%t stderr=%s", code, called, &stderr)
	}
	after, err := os.ReadFile(filepath.Join(target, "manifest.json"))
	if err != nil || !bytes.Equal(after, manifest) {
		t.Fatalf("incompatible library changed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "cache")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("library rejection created cache: %v", err)
	}
}

func TestDefaultCacheUsesMarkedRawV2AndMigrationFlagsStayAbsent(t *testing.T) {
	cachePath, err := defaultRawCache()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(cachePath) != "raw-v2" || strings.Contains(cachePath, "raw-v1") {
		t.Fatalf("default cache path is not the clean raw-v2 cutover: %s", cachePath)
	}
	for _, flag := range []string{"--migrate", "--import-python-cache", "--reuse-unmarked-cache"} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{flag}, &stdout, &stderr,
			func(string, fetch.Config, func(string)) (*fetch.Client, error) {
				t.Fatal("unknown migration flag created transport")
				return nil, nil
			})
		if code != 1 || !strings.Contains(stderr.String(), "unknown flag") {
			t.Fatalf("migration flag unexpectedly exists: flag=%s code=%d stderr=%s", flag, code, &stderr)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--help"}, &stdout, &stderr,
		func(string, fetch.Config, func(string)) (*fetch.Client, error) {
			t.Fatal("help created transport")
			return nil, nil
		}); code != 0 {
		t.Fatalf("help failed: %d %s", code, &stderr)
	}
	help := stdout.String()
	if !strings.Contains(help, "marked aos-cx-docs-dldr raw-v2") || strings.Contains(help, "migration") {
		t.Fatalf("help does not describe clean cache separation: %s", help)
	}
}
