//go:build darwin

package pdfgen

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// quarantineValue is what Safari writes on a downloaded file.
const quarantineValue = "0083;6720f000;Safari;"

// quarantineBundle is a synthetic release bundle: an executable with Chrome
// and qpdf sidecars beside it, pinned by platform.
type quarantineBundle struct {
	executable string
	chromeDir  string
	qpdfDir    string
	platform   sidecarPlatform
	// code lists every pinned executable and library, data the other files.
	code []string
	data []string
}

func newQuarantineBundle(t *testing.T, parent string) quarantineBundle {
	t.Helper()
	root := filepath.Join(parent, "aos-cx-docs-dldr-test-macos-arm64")
	bundle := quarantineBundle{
		executable: filepath.Join(root, "aos-cx-docs-dldr"),
		chromeDir:  filepath.Join(root, "sidecars", "chrome-test"),
		qpdfDir:    filepath.Join(root, "sidecars", "qpdf-test"),
	}
	write := func(path, body string, mode os.FileMode) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(body))
		return hex.EncodeToString(sum[:])
	}
	write(bundle.executable, "main program", 0o755)
	var chromeFiles []sidecarFile
	for _, file := range []struct {
		relative, body string
		kind           pinnedFileKind
	}{
		{"chrome-headless-shell", "chrome executable", pinnedExecutable},
		{"libvk_swiftshader.dylib", "swiftshader library", pinnedExecutable},
		{"resources.pak", "chrome resources", pinnedData},
	} {
		mode := os.FileMode(0o644)
		path := filepath.Join(bundle.chromeDir, file.relative)
		if file.kind == pinnedExecutable {
			mode = 0o755
			bundle.code = append(bundle.code, path)
		} else {
			bundle.data = append(bundle.data, path)
		}
		chromeFiles = append(chromeFiles, sidecarFile{file.relative, int64(len(file.body)), write(path, file.body, mode), file.kind})
	}
	qpdf := filepath.Join(bundle.qpdfDir, "bin", "qpdf")
	library := filepath.Join(bundle.qpdfDir, "lib", "libqpdf.30.dylib")
	bundle.code = append(bundle.code, qpdf, library)
	bundle.platform = sidecarPlatform{
		name:                   "test",
		chromeDirectory:        filepath.Base(bundle.chromeDir),
		chromeExecutable:       "chrome-headless-shell",
		chromePlatform:         "test",
		chromeExecutableSHA256: chromeFiles[0].sha256,
		chromeFiles:            chromeFiles,
		chromeManifestSHA256:   sidecarManifestSHA256(chromeFiles),
		qpdfDirectory:          filepath.Base(bundle.qpdfDir),
		qpdfPlatform:           "test",
		// qpdf ships read-only, as in the release bundle.
		qpdfComponents: []sidecarComponent{
			{".", write(qpdf, "qpdf executable", 0o555), true, nil},
			{"../lib/libqpdf.30.dylib", write(library, "qpdf library", 0o444), false, nil},
		},
		finderMetadata: true,
		verifyObject:   func(*os.File, string) ([]string, error) { return nil, nil },
	}
	return bundle
}

func (b quarantineBundle) options(chromePath, qpdfPath string) (SidecarOptions, QPDFSidecarOptions) {
	none := func(string) string { return "" }
	return SidecarOptions{ExplicitPath: chromePath, ExecutablePath: b.executable, LookupEnv: none},
		QPDFSidecarOptions{ExplicitPath: qpdfPath, ExecutablePath: b.executable, LookupEnv: none}
}

func (b quarantineBundle) all() []string {
	return append(append([]string{b.executable}, b.code...), b.data...)
}

// quarantine marks paths as a browser download does, read-only files included.
func quarantine(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, info.Mode().Perm()|0o200); err != nil {
			t.Fatal(err)
		}
		err = unix.Setxattr(path, quarantineAttribute, []byte(quarantineValue), 0)
		if chmodErr := os.Chmod(path, info.Mode().Perm()); chmodErr != nil {
			t.Fatal(chmodErr)
		}
		if err != nil {
			t.Skipf("cannot set %s on %s: %v", quarantineAttribute, path, err)
		}
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func isQuarantined(t *testing.T, path string) bool {
	t.Helper()
	if _, err := unix.Getxattr(path, quarantineAttribute, nil); err != nil {
		if errors.Is(err, unix.ENOATTR) {
			return false
		}
		t.Fatal(err)
	}
	return true
}

func TestClearSidecarQuarantineReleasesOnlyPinnedBundledCode(t *testing.T) {
	bundle := newQuarantineBundle(t, t.TempDir())
	quarantine(t, bundle.all()...)
	modes := map[string]os.FileMode{}
	for _, path := range bundle.code {
		modes[path] = mode(t, path)
	}
	chrome, qpdf := bundle.options("", "")
	clearance, err := clearSidecarQuarantine(bundle.platform, chrome, qpdf)
	if err != nil {
		t.Fatalf("bundled sidecars were not released: %v", err)
	}
	// Read-only files are made writable only for the removal.
	for path, want := range modes {
		if got := mode(t, path); got != want {
			t.Fatalf("%s mode is %v after release, want %v", path, got, want)
		}
	}
	if modes[bundle.code[len(bundle.code)-1]] != 0o444 {
		t.Fatal("fixture no longer holds a read-only library")
	}
	cleared := slices.Clone(clearance.Files)
	slices.Sort(cleared)
	want := slices.Clone(bundle.code)
	slices.Sort(want)
	if !slices.Equal(cleared, want) {
		t.Fatalf("released %v, want exactly the pinned code %v", cleared, want)
	}
	for _, path := range bundle.code {
		if isQuarantined(t, path) {
			t.Fatalf("%s is still quarantined", path)
		}
	}
	// The main program was approved by the user and data files are never run.
	for _, path := range append([]string{bundle.executable}, bundle.data...) {
		if !isQuarantined(t, path) {
			t.Fatalf("%s lost its quarantine although it is not sidecar code", path)
		}
	}
	// Removing the attribute moved ctime; conversion must still verify them.
	if _, err := resolveSidecarFor(bundle.platform, chrome); err != nil {
		t.Fatalf("released Chrome no longer verifies: %v", err)
	}
	if _, err := resolveQPDFSidecarFor(bundle.platform, qpdf); err != nil {
		t.Fatalf("released qpdf no longer verifies: %v", err)
	}
	again, err := clearSidecarQuarantine(bundle.platform, chrome, qpdf)
	if err != nil || len(again.Files) != 0 {
		t.Fatalf("a released bundle was changed again: %+v %v", again, err)
	}
}

func TestClearSidecarQuarantineNeverChangesOverriddenSidecars(t *testing.T) {
	bundle := newQuarantineBundle(t, t.TempDir())
	quarantine(t, bundle.code...)
	// The xattr command cannot change a read-only file without sudo.
	if err := os.Chmod(bundle.code[1], 0o555); err != nil {
		t.Fatal(err)
	}
	chrome, qpdf := bundle.options(filepath.Join(bundle.chromeDir, "chrome-headless-shell"), "")
	clearance, err := clearSidecarQuarantine(bundle.platform, chrome, qpdf)
	var failure *QuarantineError
	if !errors.As(err, &failure) || !failure.Overridden || failure.Cause != nil {
		t.Fatalf("a quarantined --chrome-path sidecar was not reported: %v", err)
	}
	if len(failure.Files) != 2 || len(clearance.Files) != 2 {
		t.Fatalf("expected 2 kept Chrome files and 2 released qpdf files: kept %v released %v", failure.Files, clearance.Files)
	}
	for _, path := range failure.Files {
		if filepath.Dir(path) != bundle.chromeDir || !isQuarantined(t, path) {
			t.Fatalf("overridden Chrome file %s was changed", path)
		}
	}
	if want := "sudo xattr -dr com.apple.quarantine " + shellQuote(bundle.chromeDir); failure.Command != want {
		t.Fatalf("fallback command is %q, want %q", failure.Command, want)
	}
	message := err.Error()
	if strings.Contains(message, "\n") || !strings.HasSuffix(message, failure.Command) {
		t.Fatalf("error is not one line ending in the command: %q", message)
	}
	for _, expected := range []string{
		"macOS keeps the download quarantine on 2 PDF tool files",
		"--chrome-path, --qpdf-path, AOSCX_DOCS_CHROME_PATH or AOSCX_DOCS_QPDF_PATH are never changed",
		"nothing has been downloaded",
		"run this command, then start the download again: " + failure.Command,
	} {
		if !strings.Contains(message, expected) {
			t.Fatalf("error %q lacks %q", message, expected)
		}
	}
}

func TestClearSidecarQuarantineFallsBackToAnExactCommand(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "it's my apps")
	bundle := newQuarantineBundle(t, parent)
	quarantine(t, bundle.code...)
	// An immutable file refuses every change, as one of another account would.
	locked := bundle.code[1]
	if err := unix.Chflags(locked, unix.UF_IMMUTABLE); err != nil {
		t.Skipf("cannot make %s immutable: %v", locked, err)
	}
	t.Cleanup(func() { _ = unix.Chflags(locked, 0) })
	chrome, qpdf := bundle.options("", "")
	clearance, err := clearSidecarQuarantine(bundle.platform, chrome, qpdf)
	var failure *QuarantineError
	if !errors.As(err, &failure) || failure.Overridden || failure.Cause == nil {
		t.Fatalf("an unchangeable bundled file was not reported with its cause: %v", err)
	}
	if !slices.Equal(failure.Files, []string{locked}) || !isQuarantined(t, locked) {
		t.Fatalf("kept %v, want only %s", failure.Files, locked)
	}
	if len(clearance.Files) != len(bundle.code)-1 {
		t.Fatalf("the other pinned files were not released: %v", clearance.Files)
	}
	want := `xattr -dr com.apple.quarantine '` +
		strings.ReplaceAll(bundle.chromeDir, "'", `'\''`) + `'`
	if failure.Command != want {
		t.Fatalf("fallback command is %q, want %q", failure.Command, want)
	}
	if !strings.Contains(err.Error(), "removing it failed: "+locked+": operation not permitted.") {
		t.Fatalf("error does not name the file that failed: %v", err)
	}
}

func TestClearSidecarQuarantineLeavesUnverifiedSidecarsToConversion(t *testing.T) {
	bundle := newQuarantineBundle(t, t.TempDir())
	quarantine(t, bundle.code...)
	// Chrome no longer matches its pins; qpdf is incomplete.
	if err := os.WriteFile(bundle.code[1], []byte("swiftshader librarx"), 0o755); err != nil {
		t.Fatal(err)
	}
	quarantine(t, bundle.code[1])
	if err := os.Remove(bundle.code[3]); err != nil {
		t.Fatal(err)
	}
	chrome, qpdf := bundle.options("", "")
	clearance, err := clearSidecarQuarantine(bundle.platform, chrome, qpdf)
	if err != nil || len(clearance.Files) != 0 {
		t.Fatalf("unverified sidecars were handled before conversion: %+v %v", clearance, err)
	}
	for _, path := range bundle.code[:3] {
		if !isQuarantined(t, path) {
			t.Fatalf("unverified sidecar file %s was released", path)
		}
	}
}

func TestClearPinnedQuarantineRefusesAFileChangedAfterVerification(t *testing.T) {
	bundle := newQuarantineBundle(t, t.TempDir())
	chrome, _ := bundle.options("", "")
	if _, err := resolveSidecarFor(bundle.platform, chrome); err != nil {
		t.Fatal(err)
	}
	target := bundle.code[1]
	if err := os.WriteFile(target, []byte("swiftshader librarx"), 0o755); err != nil {
		t.Fatal(err)
	}
	quarantine(t, target)
	pin := bundle.platform.chromeFiles[1]
	cleared, still, _, err := clearPinnedQuarantine(target, pin.sha256, true)
	if cleared || !still || err == nil || !strings.Contains(err.Error(), "changed after verification") {
		t.Fatalf("a file changed after verification was released: cleared=%v still=%v err=%v", cleared, still, err)
	}
	if !isQuarantined(t, target) {
		t.Fatal("the changed file lost its quarantine")
	}
}
