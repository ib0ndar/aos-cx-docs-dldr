package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHTMLSearchCodeIsCoveredByLibraryIntegrity(t *testing.T) {
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
	script := filepath.Join(base, "6300", "10.10", "html", "search.js")
	if err := os.WriteFile(script, []byte(`alert("tampered")`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(base, "6300", "10.10"); err == nil || !strings.Contains(err.Error(), "search.js") {
		t.Fatalf("tampered executable guide asset was accepted: %v", err)
	}
}
