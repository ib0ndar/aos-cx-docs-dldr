package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/model"
)

func TestFixtureModeOnlyForLoopbackPortalOverride(t *testing.T) {
	for _, tc := range []struct {
		portal   string
		fixtures bool
	}{
		{model.PortalURL, false},
		{"http://127.0.0.1:8080/portal", true},
		{"https://[::1]:8443/portal", true},
		{"http://localhost:8080/portal", true},
		{"http://192.168.1.5/portal", false},
		{"https://10.0.0.1/portal", false},
		{"https://docs.example/portal", false},
	} {
		o := options{list: true, portal: tc.portal, transport: "compatible", delay: 0.1, timeout: 150,
			attemptTimeout: 45, retries: 2, workers: 4, maxMB: 256, maxArchiveMB: 1024}
		config, err := o.validate()
		if err != nil || config.AllowLoopbackFixtures != tc.fixtures {
			t.Errorf("portal %s: fixtures=%v want %v err=%v", tc.portal, config.AllowLoopbackFixtures, tc.fixtures, err)
		}
	}
}

func TestPrivatePortalOverrideIsRefusedBeforeNetwork(t *testing.T) {
	for _, transport := range []string{"http", "compatible"} {
		var out, stderr bytes.Buffer
		portal := "http://10.0.0.1/portal.html"
		args := []string{"--list", "--portal-url", portal, "--transport", transport,
			"--raw-cache", filepath.Join(t.TempDir(), "raw"), "--retries", "2", "--timeout", "5", "--attempt-timeout", "5"}
		if code := runContract(context.Background(), args, &out, &stderr); code != 1 ||
			!strings.Contains(stderr.String(), "refusing destination "+portal) ||
			!strings.Contains(stderr.String(), "non-public IP address") {
			t.Fatalf("%s: private portal was not refused: code=%d stdout=%s stderr=%s", transport, code, &out, &stderr)
		}
	}
}
