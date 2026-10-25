package pdfgen

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestChromeDirectoryTablesAreCanonical(t *testing.T) {
	digest := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, platform := range []sidecarPlatform{darwinARM64Sidecars, linuxAMD64Sidecars} {
		if len(platform.chromeFiles) == 0 {
			t.Fatalf("%s does not pin its Chrome directory", platform.name)
		}
		if got := sidecarManifestSHA256(platform.chromeFiles); got != platform.chromeManifestSHA256 {
			t.Fatalf("%s Chrome table digest is %s, recorded %s", platform.name, got, platform.chromeManifestSHA256)
		}
		previous := ""
		executables := 0
		listed := map[string]bool{}
		for _, file := range platform.chromeFiles {
			if file.relative <= previous {
				t.Fatalf("%s Chrome table is not strictly sorted at %q", platform.name, file.relative)
			}
			previous = file.relative
			listed[file.relative] = true
			if file.relative == "" || path.Clean(file.relative) != file.relative || path.IsAbs(file.relative) ||
				strings.HasPrefix(file.relative, "../") || strings.Contains(file.relative, "\\") {
				t.Fatalf("%s Chrome table has an unsafe path %q", platform.name, file.relative)
			}
			if !digest.MatchString(file.sha256) || file.size <= 0 {
				t.Fatalf("%s Chrome table entry is incomplete: %+v", platform.name, file)
			}
			if file.relative == platform.chromeExecutable {
				executables++
				if file.kind != pinnedExecutable || file.sha256 != platform.chromeExecutableSHA256 {
					t.Fatalf("%s Chrome table disagrees with the executable pin: %+v", platform.name, file)
				}
			}
			if file.kind == pinnedOwnerOnlyMetadata && platform.name != linuxAMD64Sidecars.name {
				t.Fatalf("%s must not relax hashing for %s", platform.name, file.relative)
			}
		}
		if executables != 1 {
			t.Fatalf("%s Chrome table lists the executable %d times", platform.name, executables)
		}
		for _, notice := range []string{"ABOUT", "LICENSE.headless_shell"} {
			if !listed[notice] {
				t.Fatalf("%s Chrome table omits required notice %s", platform.name, notice)
			}
		}
	}
	if darwinARM64Sidecars.chromeManifestSHA256 != ChromeManifestSHA256 {
		t.Fatal("darwin/arm64 Chrome manifest drifted from the exported constant")
	}
	if !darwinARM64Sidecars.finderMetadata || linuxAMD64Sidecars.finderMetadata {
		t.Fatal("Finder metadata is tolerated only on macOS")
	}
	for _, file := range linuxAMD64Sidecars.chromeFiles {
		if file.kind == pinnedOwnerOnlyMetadata && file.relative != "deb.deps" && file.relative != "rpm.deps" {
			t.Fatalf("Linux Chrome table relaxes hashing for %s", file.relative)
		}
	}
}

// testChromeTree writes a synthetic Chrome directory and returns a platform
// table pinning exactly its contents. The object check accepts any file and
// counts its invocations so tests can observe re-hashing.
func testChromeTree(t *testing.T, finderMetadata bool) (string, sidecarPlatform, *int) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "chrome-headless-shell-test")
	files := []struct {
		relative string
		body     string
		kind     pinnedFileKind
	}{
		{"chrome-headless-shell", "#!/bin/sh\nexit 0\n", pinnedExecutable},
		{"libvk_swiftshader.dylib", "swiftshader library", pinnedExecutable},
		{"vk_swiftshader_icd.json", `{"ICD":{"library_path":"./libvk_swiftshader.dylib"}}`, pinnedData},
		{"icudtl.dat", "icu data", pinnedData},
		{"resources/accessibility/helper.js", "helper", pinnedData},
		{"deb.deps", "libc6", pinnedOwnerOnlyMetadata},
	}
	var table []sidecarFile
	for _, file := range files {
		target := filepath.Join(root, filepath.FromSlash(file.relative))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		switch file.kind {
		case pinnedExecutable:
			mode = 0o755
		case pinnedOwnerOnlyMetadata:
			mode = 0o600
		}
		if err := os.WriteFile(target, []byte(file.body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(target, mode); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(file.body))
		table = append(table, sidecarFile{file.relative, int64(len(file.body)), hex.EncodeToString(sum[:]), file.kind})
	}
	inspections := new(int)
	platform := sidecarPlatform{
		name:                   "test",
		chromeDirectory:        filepath.Base(root),
		chromeExecutable:       "chrome-headless-shell",
		chromePlatform:         "test",
		chromeExecutableSHA256: table[0].sha256,
		chromeFiles:            table,
		chromeManifestSHA256:   sidecarManifestSHA256(table),
		finderMetadata:         finderMetadata,
		verifyObject: func(*os.File, string) ([]string, error) {
			*inspections++
			return nil, nil
		},
	}
	return root, platform, inspections
}

func TestVerifySidecarRequiresTheCompletePinnedChromeDirectory(t *testing.T) {
	executable := func(root string) string { return filepath.Join(root, "chrome-headless-shell") }
	write := func(t *testing.T, name string, body []byte, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, body, mode); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("complete", func(t *testing.T) {
		root, platform, _ := testChromeTree(t, false)
		sidecar, err := verifySidecar(platform, executable(root))
		if err != nil {
			t.Fatal(err)
		}
		if sidecar.ManifestSHA256 != platform.chromeManifestSHA256 || sidecar.ExecutableSHA256 != platform.chromeExecutableSHA256 {
			t.Fatalf("unexpected sidecar identity: %+v", sidecar)
		}
	})
	for name, test := range map[string]struct {
		mutate func(t *testing.T, root string)
		want   string
	}{
		"unexpected library beside Chrome": {func(t *testing.T, root string) {
			write(t, filepath.Join(root, "libvulkan.dylib"), []byte("planted"), 0o755)
		}, "unexpected file: libvulkan.dylib"},
		"unexpected nested file": {func(t *testing.T, root string) {
			write(t, filepath.Join(root, "resources", "accessibility", "extra.js"), []byte("x"), 0o644)
		}, "unexpected file: resources/accessibility/extra.js"},
		"unexpected directory": {func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, "locales"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "unexpected directory: locales"},
		"symlinked data file": {func(t *testing.T, root string) {
			target := filepath.Join(t.TempDir(), "icudtl.dat")
			write(t, target, []byte("icu data"), 0o644)
			if err := os.Remove(filepath.Join(root, "icudtl.dat")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(root, "icudtl.dat")); err != nil {
				t.Fatal(err)
			}
		}, "symbolic links: icudtl.dat"},
		"symlinked directory": {func(t *testing.T, root string) {
			if err := os.Symlink(t.TempDir(), filepath.Join(root, "swiftshader")); err != nil {
				t.Fatal(err)
			}
		}, "symbolic links: swiftshader"},
		"special file": {func(t *testing.T, root string) {
			if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "special file: fifo"},
		"changed library with the same size": {func(t *testing.T, root string) {
			write(t, filepath.Join(root, "libvk_swiftshader.dylib"), []byte("swiftshader LIBRARY"), 0o755)
		}, "SHA-256"},
		"changed ICD manifest size": {func(t *testing.T, root string) {
			write(t, filepath.Join(root, "vk_swiftshader_icd.json"), []byte(`{"ICD":{"library_path":"/tmp/evil.dylib"}}`), 0o644)
		}, "size is"},
		"executable data file": {func(t *testing.T, root string) {
			if err := os.Chmod(filepath.Join(root, "icudtl.dat"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "must not be executable"},
		"nonexecutable library": {func(t *testing.T, root string) {
			if err := os.Chmod(filepath.Join(root, "libvk_swiftshader.dylib"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "is not executable"},
		"Finder metadata without Finder": {func(t *testing.T, root string) {
			write(t, filepath.Join(root, ".DS_Store"), []byte("finder"), 0o644)
		}, "unexpected file: .DS_Store"},
	} {
		t.Run(name, func(t *testing.T) {
			root, platform, _ := testChromeTree(t, false)
			test.mutate(t, root)
			_, err := verifySidecar(platform, executable(root))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want error containing %q", err, test.want)
			}
		})
	}
	t.Run("missing pinned file is not a missing sidecar", func(t *testing.T) {
		root, platform, _ := testChromeTree(t, false)
		if err := os.Remove(filepath.Join(root, "resources", "accessibility", "helper.js")); err != nil {
			t.Fatal(err)
		}
		_, err := verifySidecar(platform, executable(root))
		if err == nil || !strings.Contains(err.Error(), "missing pinned file resources/accessibility/helper.js") ||
			errors.Is(err, os.ErrNotExist) {
			t.Fatalf("incomplete Chrome directory was not reported as such: %v", err)
		}
	})
	t.Run("executable outside its pinned name", func(t *testing.T) {
		root, platform, _ := testChromeTree(t, false)
		body, err := os.ReadFile(executable(root))
		if err != nil {
			t.Fatal(err)
		}
		renamed := filepath.Join(root, "chrome")
		write(t, renamed, body, 0o755)
		if _, err := verifySidecar(platform, renamed); err == nil || !strings.Contains(err.Error(), "must be named") {
			t.Fatalf("renamed Chrome executable was accepted: %v", err)
		}
	})
}

func TestVerifySidecarToleratesOnlyBoundedFinderMetadataOnMacOS(t *testing.T) {
	root, platform, _ := testChromeTree(t, true)
	for _, dir := range []string{".", "resources", filepath.Join("resources", "accessibility")} {
		if err := os.WriteFile(filepath.Join(root, dir, ".DS_Store"), []byte("finder"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := verifySidecar(platform, filepath.Join(root, "chrome-headless-shell")); err != nil {
		t.Fatalf("bounded Finder metadata was refused: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".DS_Store"), make([]byte, finderMetadataMaxBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySidecar(platform, filepath.Join(root, "chrome-headless-shell")); err == nil ||
		!strings.Contains(err.Error(), "unexpected file: .DS_Store") {
		t.Fatalf("oversized Finder metadata was accepted: %v", err)
	}
	if err := os.Remove(filepath.Join(root, ".DS_Store")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "icudtl.dat"), filepath.Join(root, ".DS_Store")); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySidecar(platform, filepath.Join(root, "chrome-headless-shell")); err == nil ||
		!strings.Contains(err.Error(), "symbolic links: .DS_Store") {
		t.Fatalf("symlinked Finder metadata was accepted: %v", err)
	}
}

func TestOwnerOnlyMetadataIsHashedWhenReadableAndOnlyItMayBeUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read owner-only files, so permission denial cannot be exercised")
	}
	root, platform, _ := testChromeTree(t, false)
	metadata := filepath.Join(root, "deb.deps")
	if err := os.WriteFile(metadata, []byte("libc7"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySidecar(platform, filepath.Join(root, "chrome-headless-shell")); err == nil ||
		!strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("readable changed owner-only metadata was accepted: %v", err)
	}
	if err := os.Chmod(metadata, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(metadata, 0o600) })
	if _, err := verifySidecar(platform, filepath.Join(root, "chrome-headless-shell")); err != nil {
		t.Fatalf("unreadable owner-only metadata of the pinned size was refused: %v", err)
	}
	if err := os.Chmod(metadata, 0o040); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySidecar(platform, filepath.Join(root, "chrome-headless-shell")); err == nil ||
		!errors.Is(err, os.ErrPermission) {
		t.Fatalf("unreadable metadata with group access was accepted: %v", err)
	}
	if err := os.Chmod(metadata, 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "icudtl.dat")
	if err := os.Chmod(data, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(data, 0o644) })
	if _, err := verifySidecar(platform, filepath.Join(root, "chrome-headless-shell")); err == nil ||
		!errors.Is(err, os.ErrPermission) {
		t.Fatalf("unreadable pinned data file was accepted: %v", err)
	}
}

func TestSidecarReverificationRehashesOnlyChangedFiles(t *testing.T) {
	root, platform, inspections := testChromeTree(t, false)
	chrome := filepath.Join(root, "chrome-headless-shell")
	if _, err := verifySidecar(platform, chrome); err != nil {
		t.Fatal(err)
	}
	if *inspections != 1 {
		t.Fatalf("first verification inspected the executable %d times", *inspections)
	}
	for _, file := range platform.chromeFiles {
		target := filepath.Join(root, filepath.FromSlash(file.relative))
		info, err := os.Lstat(target)
		if err != nil {
			t.Fatal(err)
		}
		if !pinnedContentCurrent(target, info, file.sha256) {
			t.Fatalf("verified file was not recorded: %s", file.relative)
		}
	}
	if _, err := verifySidecar(platform, chrome); err != nil || *inspections != 1 {
		t.Fatalf("unchanged executable was re-hashed: inspections=%d err=%v", *inspections, err)
	}

	// Rewrite a library with same-size content and put its mtime back: the
	// inode, size and mtime match, but ctime moves, so it must be re-hashed.
	library := filepath.Join(root, "libvk_swiftshader.dylib")
	before, err := os.Lstat(library)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	file, err := os.OpenFile(library, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteAt([]byte("S"), 0)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(library, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySidecar(platform, chrome); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("same-size change with restored mtime was not detected: %v", err)
	}

	// A touched executable is inspected and hashed again.
	if err := os.WriteFile(library, []byte("swiftshader library"), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(chrome, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySidecar(platform, chrome); err != nil || *inspections != 2 {
		t.Fatalf("changed executable identity was not re-verified: inspections=%d err=%v", *inspections, err)
	}
}

func TestConfirmSidecarsRefuseUnpinnedExecutables(t *testing.T) {
	stand := writeExecutable(t, "chrome-headless-shell", []byte("#!/bin/sh\n"))
	if err := confirmChromeSidecar(Sidecar{Path: stand}); err == nil ||
		!strings.Contains(err.Error(), "immediately before launch") {
		t.Fatalf("unpinned Chrome was confirmed: %v", err)
	}
	if err := confirmQPDFSidecar(QPDFSidecar{Path: stand}); err == nil ||
		!strings.Contains(err.Error(), "immediately before launch") {
		t.Fatalf("unpinned qpdf was confirmed: %v", err)
	}
}

func TestQPDFSearchDirectoriesHoldOnlyPinnedComponents(t *testing.T) {
	root := filepath.Join(t.TempDir(), "qpdf-test")
	bodies := map[string]string{"bin/qpdf": "qpdf executable", "lib/libqpdf.so.30": "qpdf library"}
	sha := map[string]string{}
	for relative, body := range bodies {
		target := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(body))
		sha[relative] = hex.EncodeToString(sum[:])
	}
	if err := os.MkdirAll(filepath.Join(root, "licenses", "qpdf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "licenses", "qpdf", "LICENSE.txt"), []byte("license"), 0o644); err != nil {
		t.Fatal(err)
	}
	platform := sidecarPlatform{
		name: "test",
		qpdfComponents: []sidecarComponent{
			{".", sha["bin/qpdf"], true, []string{"libqpdf.so.30"}},
			{"../lib/libqpdf.so.30", sha["lib/libqpdf.so.30"], false, []string{"libqpdf.so.30"}},
		},
		verifyObject: func(*os.File, string) ([]string, error) { return []string{"libqpdf.so.30"}, nil },
	}
	qpdf := filepath.Join(root, "bin", "qpdf")
	if _, err := verifyQPDFSidecar(platform, qpdf); err != nil {
		t.Fatalf("pinned qpdf closure was refused: %v", err)
	}
	for _, planted := range []string{"lib/libz.so.1", "bin/zlib-flate", "lib/.DS_Store"} {
		target := filepath.Join(root, filepath.FromSlash(planted))
		if err := os.WriteFile(target, []byte("planted"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := verifyQPDFSidecar(platform, qpdf); err == nil || !strings.Contains(err.Error(), "unpinned entry") {
			t.Fatalf("planted %s on the qpdf search path was accepted: %v", planted, err)
		}
		if planted == "lib/.DS_Store" {
			finder := platform
			finder.finderMetadata = true
			if _, err := verifyQPDFSidecar(finder, qpdf); err != nil {
				t.Fatalf("bounded Finder metadata was refused on macOS: %v", err)
			}
		}
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "lib", "private"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyQPDFSidecar(platform, qpdf); err == nil || !strings.Contains(err.Error(), "unpinned entry") {
		t.Fatalf("unexpected directory on the qpdf search path was accepted: %v", err)
	}
}

func TestPinnedFileIsReadAgainAfterAMetadataOnlyChange(t *testing.T) {
	target := filepath.Join(t.TempDir(), "icudtl.dat")
	if err := os.WriteFile(target, []byte("pinned bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("pinned bytes"))
	want := hex.EncodeToString(sum[:])
	observe := func() fs.FileInfo {
		t.Helper()
		info, err := os.Lstat(target)
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	// A sync client writing an extended attribute moves only ctime; a chmod
	// to the same mode does the same on every platform.
	touchMetadata := func() error {
		time.Sleep(5 * time.Millisecond)
		return os.Chmod(target, 0o644)
	}

	reads := 0
	digest, stable, err := hashPinnedFile("Chrome sidecar", target, observe(), func(*os.File) error {
		reads++
		if reads == 1 {
			return touchMetadata()
		}
		return nil
	})
	if err != nil || digest != want || reads != 2 {
		t.Fatalf("metadata-only change: digest=%s reads=%d err=%v", digest, reads, err)
	}
	if !sameFileIdentity(stable, observe()) {
		t.Fatal("the returned observation is not the one the accepted read was stable against")
	}

	reads = 0
	_, _, err = hashPinnedFile("Chrome sidecar", target, observe(), func(*os.File) error {
		reads++
		return touchMetadata()
	})
	if err == nil || !strings.Contains(err.Error(), "identity changed during verification") || reads != maxPinnedFileReads {
		t.Fatalf("metadata that never settles: reads=%d err=%v", reads, err)
	}

	reads = 0
	_, _, err = hashPinnedFile("Chrome sidecar", target, observe(), func(*os.File) error {
		reads++
		time.Sleep(5 * time.Millisecond)
		return os.WriteFile(target, []byte("other bytes!"), 0o644)
	})
	if err == nil || !strings.Contains(err.Error(), "identity changed during verification") || reads != 1 {
		t.Fatalf("a write during the read was not refused at once: reads=%d err=%v", reads, err)
	}
}
