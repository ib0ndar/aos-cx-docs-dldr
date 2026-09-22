//go:build oldbinarygate

package library

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	oldBinarySHA256    = "1c1afb5ee37852354227b5685f6eb051a69702fd9997c71ad3cb4fc119fa4cf8"
	oldChecksumsSHA256 = "91d363412a1e37278aaf6b0f381a4ee1cf505f390bee649c460d80de6e2f06e0"
)

type gatePathIdentity struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Mode   string `json:"mode"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
	Target string `json:"target,omitempty"`
}

type oldBinaryGateCase struct {
	Name                        string             `json:"name"`
	Command                     []string           `json:"command"`
	ExitCode                    int                `json:"exit_code"`
	Stdout                      string             `json:"stdout"`
	Stderr                      string             `json:"stderr"`
	Before                      []gatePathIdentity `json:"before"`
	After                       []gatePathIdentity `json:"after"`
	Unchanged                   bool               `json:"unchanged"`
	RawCachePath                string             `json:"raw_cache_path"`
	RawCacheBefore              []gatePathIdentity `json:"raw_cache_before"`
	RawCacheAfter               []gatePathIdentity `json:"raw_cache_after"`
	RawCacheUnchanged           bool               `json:"raw_cache_unchanged"`
	RawCacheRemainedAbsent      bool               `json:"raw_cache_remained_absent"`
	LockRemainedAbsent          bool               `json:"lock_remained_absent"`
	ManagedPathsWithinStateTree bool               `json:"managed_paths_within_state_tree"`
	Requests                    []string           `json:"loopback_requests"`
	NoExternal                  bool               `json:"no_external_requests"`
}

type oldBinaryGateEvidence struct {
	SchemaVersion   int                 `json:"schema_version"`
	SourcePackage   string              `json:"source_package"`
	CopiedPackage   string              `json:"copied_package"`
	BinarySHA256    string              `json:"binary_sha256"`
	ChecksumsSHA256 string              `json:"checksums_sha256"`
	ChecksumsValid  bool                `json:"checksums_valid"`
	Cases           []oldBinaryGateCase `json:"cases"`
}

func TestRetainedZeroSixBinaryRejectsZeroSevenStateReadOnly(t *testing.T) {
	source := os.Getenv("AOSCX_OLD_PACKAGE_SOURCE")
	output := os.Getenv("AOSCX_OLD_GATE_OUTPUT")
	if source == "" || output == "" {
		t.Skip("set AOSCX_OLD_PACKAGE_SOURCE and AOSCX_OLD_GATE_OUTPUT")
	}
	entries, err := os.ReadDir(output)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(output, 0o700); err != nil {
			t.Fatal(err)
		}
	} else if err != nil || len(entries) != 0 {
		t.Fatalf("old-binary gate output must be new or empty: entries=%d err=%v", len(entries), err)
	}

	binaryHash := fileSHA256(t, filepath.Join(source, "aoscx-docs-go"))
	checksumsHash := fileSHA256(t, filepath.Join(source, "SHA256SUMS"))
	if binaryHash != oldBinarySHA256 || checksumsHash != oldChecksumsSHA256 {
		t.Fatalf("retained 0.6 identity mismatch: binary=%s checksums=%s", binaryHash, checksumsHash)
	}
	verifyChecksumFile(t, source)

	copied := filepath.Join(output, "aoscx-docs-0.6-macos-arm64")
	if err := os.CopyFS(copied, os.DirFS(source)); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(copied, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(name, 0o555)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := os.FileMode(0o444)
		if info.Mode()&0o111 != 0 {
			mode = 0o555
		}
		return os.Chmod(name, mode)
	}); err != nil {
		t.Fatal(err)
	}
	verifyChecksumFile(t, copied)

	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
			t.Errorf("non-loopback request reached authored server: host=%q err=%v", r.Host, err)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		switch r.URL.Path {
		case "/robots.txt":
			io.WriteString(w, "User-agent: *\nAllow: /\n")
		case "/portal":
			io.WriteString(w, `<select id="platform"><option>6300</option></select>
<select id="ver"><option>10.10</option></select><div id="menu1"><table><tr>
<td id="job" onclick="openFile('PDF','job','aoscx')">Job Scheduler</td></tr></table></div>`)
		case "/json/aoscx/job.json":
			fmt.Fprintf(w, `{"10.10":{"6300":%q}}`, serverURL(r)+"/job.pdf")
		case "/job.pdf":
			if r.Method != http.MethodHead {
				t.Errorf("old binary reached PDF body after rejected preflight")
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/pdf")
			w.Header().Set("Content-Length", "1024")
		default:
			t.Errorf("unexpected authored request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	states := filepath.Join(output, "states")
	if err := os.MkdirAll(states, 0o700); err != nil {
		t.Fatal(err)
	}
	committed := filepath.Join(states, "committed-current")
	createCurrentGateLibrary(t, committed)
	inflight := filepath.Join(states, "schema-2-inflight")
	createInflightGateLibrary(t, inflight)

	evidence := oldBinaryGateEvidence{
		SchemaVersion:   1,
		SourcePackage:   source,
		CopiedPackage:   copied,
		BinarySHA256:    binaryHash,
		ChecksumsSHA256: checksumsHash,
		ChecksumsValid:  true,
	}
	for _, fixture := range []struct {
		name string
		base string
	}{
		{name: "schema-2-inflight", base: inflight},
		{name: "committed-current", base: committed},
	} {
		mu.Lock()
		requests = nil
		mu.Unlock()
		before := gateTree(t, fixture.base)
		rawCache := filepath.Join(output, "raw-"+fixture.name)
		rawBefore, rawBeforeExists := gateTreeIfExists(t, rawCache)
		args := []string{
			"--transport", "http",
			"--platform", "6300",
			"--version", "10.10",
			"--guides", "job",
			"--destination", fixture.base,
			"--raw-cache", rawCache,
			"--portal-url", server.URL + "/portal",
			"--delay", "0",
			"--retries", "0",
			"--timeout", "15",
			"--attempt-timeout", "5",
			"--json",
		}
		command := exec.Command(filepath.Join(copied, "aoscx-docs-go"), args...)
		command.Env = loopbackOnlyEnvironment()
		stdout, runErr := command.Output()
		exitCode := 0
		var stderr string
		if runErr != nil {
			var exitErr *exec.ExitError
			if !errors.As(runErr, &exitErr) {
				t.Fatal(runErr)
			}
			exitCode = exitErr.ExitCode()
			stderr = string(exitErr.Stderr)
		}
		if exitCode == 0 {
			t.Fatalf("retained 0.6 binary accepted %s state: stdout=%s", fixture.name, stdout)
		}
		if !strings.Contains(stderr, "unsupported") &&
			!strings.Contains(stderr, "incompatible") &&
			!strings.Contains(stderr, "manual inspection") {
			t.Fatalf("%s rejection was not actionable: %s", fixture.name, stderr)
		}
		after := gateTree(t, fixture.base)
		if !equalGateTrees(before, after) {
			t.Fatalf("retained 0.6 binary mutated %s state", fixture.name)
		}
		rawAfter, rawAfterExists := gateTreeIfExists(t, rawCache)
		rawUnchanged := rawBeforeExists == rawAfterExists && equalGateTrees(rawBefore, rawAfter)
		if !rawUnchanged {
			t.Fatalf("retained 0.6 binary mutated raw-cache path for %s", fixture.name)
		}
		if _, err := os.Lstat(filepath.Join(fixture.base, "6300", ".10.10.lock")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retained 0.6 binary created a lock for %s: %v", fixture.name, err)
		}
		mu.Lock()
		observed := append([]string(nil), requests...)
		mu.Unlock()
		evidence.Cases = append(evidence.Cases, oldBinaryGateCase{
			Name:                        fixture.name,
			Command:                     append([]string{filepath.Join(copied, "aoscx-docs-go")}, args...),
			ExitCode:                    exitCode,
			Stdout:                      string(stdout),
			Stderr:                      stderr,
			Before:                      before,
			After:                       after,
			Unchanged:                   true,
			RawCachePath:                rawCache,
			RawCacheBefore:              rawBefore,
			RawCacheAfter:               rawAfter,
			RawCacheUnchanged:           true,
			RawCacheRemainedAbsent:      !rawBeforeExists && !rawAfterExists,
			LockRemainedAbsent:          true,
			ManagedPathsWithinStateTree: true,
			Requests:                    observed,
			NoExternal:                  true,
		})
	}
	body, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "old-binary-gate.json"), append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func createCurrentGateLibrary(t *testing.T, base string) {
	t.Helper()
	run, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.Publish(true); err != nil {
		run.Close()
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

func createInflightGateLibrary(t *testing.T, base string) {
	t.Helper()
	createCurrentGateLibrary(t, base)
	target := filepath.Join(base, "6300", "10.10")
	rewritePublishedApplicationVersion(t, target, "0.6")
	_, err := openContextWithPolicy(
		context.Background(),
		base,
		"6300",
		"10.10",
		upgradePolicy(t),
		func(point string) error {
			if point == "target-to-snapshot" {
				return errors.New("retain valid schema-2 journal")
			}
			return nil
		},
	)
	if err == nil {
		t.Fatal("schema-2 fixture did not retain its journal")
	}
}

func serverURL(request *http.Request) string {
	return "http://" + request.Host
}

func loopbackOnlyEnvironment() []string {
	blocked := map[string]bool{
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true,
		"http_proxy": true, "https_proxy": true, "all_proxy": true,
		"NO_PROXY": true, "no_proxy": true,
	}
	environment := make([]string, 0, len(os.Environ())+8)
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if !blocked[name] {
			environment = append(environment, value)
		}
	}
	return append(environment,
		"HTTP_PROXY=http://127.0.0.1:1",
		"HTTPS_PROXY=http://127.0.0.1:1",
		"ALL_PROXY=http://127.0.0.1:1",
		"NO_PROXY=127.0.0.1,localhost",
	)
}

func verifyChecksumFile(t *testing.T, root string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	for index, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		hash, relative, ok := strings.Cut(line, "  ")
		if !ok || len(hash) != 64 {
			t.Fatalf("invalid checksum line %d", index+1)
		}
		relative = strings.TrimPrefix(relative, "./")
		if got := fileSHA256(t, filepath.Join(root, filepath.FromSlash(relative))); got != hash {
			t.Fatalf("checksum mismatch for %s: %s", relative, got)
		}
	}
}

func fileSHA256(t *testing.T, name string) string {
	t.Helper()
	file, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func gateTree(t *testing.T, root string) []gatePathIdentity {
	t.Helper()
	var identities []gatePathIdentity
	if err := filepath.WalkDir(root, func(name string, entry os.DirEntry, err error) error {
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
		identity := gatePathIdentity{
			Path: filepath.ToSlash(relative),
			Mode: fmt.Sprintf("%#o", uint32(info.Mode())),
			Size: info.Size(),
		}
		switch {
		case info.Mode().IsRegular():
			identity.Type = "file"
			identity.SHA256 = fileSHA256(t, name)
		case info.IsDir():
			identity.Type = "directory"
		case info.Mode()&os.ModeSymlink != 0:
			identity.Type = "symlink"
			identity.Target, err = os.Readlink(name)
			if err != nil {
				return err
			}
		default:
			identity.Type = "special"
		}
		identities = append(identities, identity)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return identities
}

func gateTreeIfExists(t *testing.T, root string) ([]gatePathIdentity, bool) {
	t.Helper()
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return []gatePathIdentity{}, false
	} else if err != nil {
		t.Fatal(err)
	}
	return gateTree(t, root), true
}

func equalGateTrees(first, second []gatePathIdentity) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func TestOldBinaryGateEvidenceShape(t *testing.T) {
	evidence := oldBinaryGateEvidence{
		SchemaVersion: 1,
		Cases: []oldBinaryGateCase{{
			Name: "fixture", ExitCode: 1, Unchanged: true,
			RawCacheUnchanged: true, RawCacheRemainedAbsent: true,
			LockRemainedAbsent: true, ManagedPathsWithinStateTree: true, NoExternal: true,
			Before:         []gatePathIdentity{{Path: ".", Type: "directory", Mode: "020000000700"}},
			After:          []gatePathIdentity{{Path: ".", Type: "directory", Mode: "020000000700"}},
			RawCacheBefore: []gatePathIdentity{},
			RawCacheAfter:  []gatePathIdentity{},
		}},
	}
	body, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), strconv.Quote("no_external_requests")) {
		t.Fatal("gate evidence omitted network boundary")
	}
}
