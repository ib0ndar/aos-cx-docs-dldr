package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/model"
)

// This opt-in probe never runs in the normal suite and never changes transport
// identity to work around a denial. Its transcript distinguishes denial from a pass.
func TestLivePublicSupportFrontMatter(t *testing.T) {
	dir := os.Getenv("AOSCX_LIVE_PROBE_DIR")
	if dir == "" {
		t.Skip("live probe requires an explicit artifact directory")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil || !strings.HasPrefix(filepath.Base(absolute), "aoscx-go-m1-") {
		t.Fatal("live probe artifacts must be in an explicitly owned aoscx-go-m1-* directory")
	}
	target := "https://support.hpe.com/hpesc/public/api/document/sd00007433en_us?ignorePayload=true"
	cfg := DefaultConfig()
	cfg.Retries, cfg.Timeout, cfg.MaxBytes = 0, 20*time.Second, 1<<20
	var diagnostics []string
	c, err := New(cfg, func(message string) { diagnostics = append(diagnostics, message) })
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	record := map[string]any{"url": target, "started_at": time.Now().UTC().Format(time.RFC3339), "transport": "standard net/http"}
	var body []byte
	r, requestErr := c.Download(context.Background(), model.Request{URL: target, Refresh: true}, func(r model.Resource) error {
		var err error
		body, err = io.ReadAll(r.Body)
		return err
	})
	if requestErr != nil {
		record["outcome"], record["error"] = "failed", requestErr.Error()
		var status *StatusError
		if errors.As(requestErr, &status) {
			record["http_status"], record["failed_url"] = status.Status, status.URL
			if status.Status == 401 || status.Status == 403 {
				record["outcome"] = "access-denied"
			}
		}
	} else {
		record["final_url"], record["http_status"], record["headers"] = r.URL, r.Status, r.Headers
		record["bytes"] = len(body)
		if !strings.Contains(string(body), "ditasrc") || !strings.Contains(string(body), "Fundamentals") {
			record["outcome"], record["error"] = "invalid-payload", "expected HPE DITA Fundamentals front matter"
			requestErr = errors.New("response did not contain expected HPE front-matter shape and title")
		} else {
			record["outcome"] = "valid-front-matter"
		}
		if err := os.WriteFile(filepath.Join(absolute, "live-support-frontmatter.html"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	record["diagnostics"] = diagnostics
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(absolute, "live-support-result.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Log(string(data))
	if requestErr != nil {
		t.Skipf("LIVE ACCESS NOT VERIFIED: %s (recorded, not a functional pass)", requestErr)
	}
}
