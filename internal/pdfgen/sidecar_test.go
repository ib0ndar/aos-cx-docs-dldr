package pdfgen

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveSidecarDoesNotSearchPATHOrDownload(t *testing.T) {
	root := t.TempDir()
	fake := filepath.Join(root, "chrome-headless-shell")
	if err := os.WriteFile(fake, []byte("not chrome"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	_, err := resolveSidecar(SidecarOptions{
		ExecutablePath: filepath.Join(root, "bin", "aos-cx-docs-dldr"),
		LookupEnv:      func(string) string { return "" },
	})
	if err == nil || !strings.Contains(err.Error(), "never downloads or searches") {
		t.Fatalf("implicit PATH browser was accepted: %v", err)
	}
}

func TestSidecarAcquisitionUsesAuthoritativePinHelper(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "scripts", "acquire-macos-arm64-sidecars.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"Chrome version pin":      `pin chrome-version`,
		"Chrome archive pin":      `pin chrome-archive-sha256`,
		"qpdf bottle pin":         `pin qpdf-bottle-sha256`,
		"qpdf input library pin":  `pin qpdf-library-input-sha256`,
		"complete checksums":      `checksums --root "$STAGED"`,
		"verified output":         `verify-source`,
		"token header creation":   `auth-header --output "$header"`,
		"private token use":       `--header "@$header"`,
		"safe archive extraction": `"$TOOL" extract`,
		"Chrome root allowlist":   `"$CHROME_ARCHIVE_SHA256" chrome-headless-shell-mac-arm64`,
		"qpdf root allowlist":     `"$QPDF_BOTTLE_SHA256" qpdf`,
		"jpeg root allowlist":     `"$JPEG_BOTTLE_SHA256" jpeg-turbo`,
		"OpenSSL root allowlist":  `"$OPENSSL_BOTTLE_SHA256" openssl@3`,
		"qpdf executable select":  `--select "qpdf/$QPDF_VERSION/bin/qpdf=qpdf/$QPDF_VERSION/bin/qpdf"`,
		"qpdf library select":     `--select "qpdf/$QPDF_VERSION/lib/libqpdf.30.4.1.dylib=qpdf/$QPDF_VERSION/lib/libqpdf.30.4.1.dylib"`,
		"qpdf license select":     `--select "qpdf/$QPDF_VERSION/LICENSE.txt=qpdf/$QPDF_VERSION/LICENSE.txt"`,
		"qpdf notice select":      `--select "qpdf/$QPDF_VERSION/NOTICE.md=qpdf/$QPDF_VERSION/NOTICE.md"`,
		"jpeg library select":     `--select "jpeg-turbo/$JPEG_VERSION/lib/libjpeg.8.3.2.dylib=jpeg-turbo/$JPEG_VERSION/lib/libjpeg.8.3.2.dylib"`,
		"jpeg license select":     `--select "jpeg-turbo/$JPEG_VERSION/LICENSE.md=jpeg-turbo/$JPEG_VERSION/LICENSE.md"`,
		"OpenSSL library select":  `--select "openssl@3/$OPENSSL_VERSION/lib/libcrypto.3.dylib=openssl@3/$OPENSSL_VERSION/lib/libcrypto.3.dylib"`,
		"OpenSSL license select":  `--select "openssl@3/$OPENSSL_VERSION/LICENSE.txt=openssl@3/$OPENSSL_VERSION/LICENSE.txt"`,
		"header removal":          `rm -f -- "$header"`,
	} {
		if !bytes.Contains(body, []byte(value)) {
			t.Fatalf("acquisition script does not contain %s %q", name, value)
		}
	}
	for _, forbidden := range []string{
		"set -x", "python3", "python ", `token=$(curl`, `echo "$token"`,
		`printf '%s' "$token"`, `Authorization: Bearer`, `-H "Authorization:`,
	} {
		if bytes.Contains(body, []byte(forbidden)) {
			t.Fatalf("acquisition script can expose the registry token through %q", forbidden)
		}
	}
}

func TestLocalStagingScriptsContainNoAcquisition(t *testing.T) {
	for _, name := range []string{"stage-chrome-sidecar.sh", "stage-qpdf-sidecar.sh"} {
		body, err := os.ReadFile(filepath.Join("..", "..", "scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"curl", "https://", "brew ", "command -v"} {
			if bytes.Contains(body, []byte(forbidden)) {
				t.Fatalf("%s contains forbidden acquisition/PATH behavior %q", name, forbidden)
			}
		}
		for _, required := range []string{"AOSCX_RELEASE_TOOL", "stage-tree", "VERIFIED_SOURCE_DIR"} {
			if !bytes.Contains(body, []byte(required)) {
				t.Fatalf("%s does not contain local staging contract %q", name, required)
			}
		}
	}
}

func TestPackageScriptUsesVerifiedLocalSidecarsAndCompleteSmokes(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "scripts", "package-macos-arm64.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"offline Go modules":           `GOPROXY=off GOSUMDB=off`,
		"source checksum verification": `verify-source`,
		"explicit Chrome source":       `"$CHROME_SOURCE" "$BUNDLE/sidecars"`,
		"explicit qpdf source":         `"$QPDF_SOURCE" "$BUNDLE/sidecars"`,
		"moved bundle smoke":           `"$MOVED/.package-smoke.test"`,
		"extracted ZIP smoke":          `"$EXTRACTED/.package-smoke.test"`,
		"Chrome override removal":      `-u AOSCX_DOCS_CHROME_PATH`,
		"qpdf override removal":        `-u AOSCX_DOCS_QPDF_PATH`,
		"local conversion smoke":       `TestPinnedSidecarsExecutableRelativeConversion`,
		"positive smoke sentinel":      `AOSCX_WP13_PACKAGE_SMOKE_SENTINEL`,
		"package manifest generation":  `"$RELEASE_TOOL" generate`,
		"deterministic ZIP generation": `"$RELEASE_TOOL" archive`,
		"detached provenance":          `"$RELEASE_TOOL" provenance`,
		"bundle/ZIP comparison":        `"$RELEASE_TOOL" compare`,
		"source identity":              `"$RELEASE_TOOL" source-identity`,
		"fixed umask":                  `umask 022`,
		"single release-set publish":   `mv "$RELEASE_SET" "$DESTINATION"`,
		"dangling-symlink refusal":     `[ -e "$DESTINATION" ] || [ -L "$DESTINATION" ]`,
	} {
		if !bytes.Contains(body, []byte(value)) {
			t.Fatalf("package script does not contain %s %q", name, value)
		}
	}
	for _, forbidden := range []string{"curl", "https://", "brew ", "/opt/homebrew"} {
		if bytes.Contains(body, []byte(forbidden)) {
			t.Fatalf("package script contains forbidden acquisition behavior %q", forbidden)
		}
	}
	for _, forbidden := range []string{
		`mv "$ZIP" "$DESTINATION.zip"`,
		`mv "$MOVED" "$DESTINATION"`,
	} {
		if bytes.Contains(body, []byte(forbidden)) {
			t.Fatalf("package script exposes partial publication step %q", forbidden)
		}
	}
}

func TestPackageScriptRejectsDanglingDestinationSymlink(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("package script is macOS arm64 only")
	}
	root := t.TempDir()
	chrome := filepath.Join(root, "chrome")
	qpdf := filepath.Join(root, "qpdf")
	if err := os.MkdirAll(chrome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(qpdf, 0o755); err != nil {
		t.Fatal(err)
	}
	checksums := filepath.Join(root, "SHA256SUMS")
	if err := os.WriteFile(checksums, []byte("unused\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "dangling-release")
	if err := os.Symlink(filepath.Join(root, "missing"), destination); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(
		"sh", filepath.Join("..", "..", "scripts", "package-macos-arm64.sh"),
		"--source-root", root,
		"--source-checksums", checksums,
		"--chrome-source", chrome,
		"--qpdf-source", qpdf,
		destination,
	)
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "Refusing to replace existing release-set destination") {
		t.Fatalf("dangling destination symlink was not rejected: err=%v output=%s", err, output)
	}
}

func TestResolveSidecarExplicitPathPrecedesEnvironment(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), "environment-chrome")
	explicitPath := filepath.Join(t.TempDir(), "explicit-chrome")
	_, err := resolveSidecar(SidecarOptions{
		ExplicitPath: explicitPath,
		LookupEnv: func(name string) string {
			if name == ChromePathEnvironment {
				return envPath
			}
			return ""
		},
	})
	if err == nil || !strings.Contains(err.Error(), explicitPath) ||
		strings.Contains(err.Error(), envPath) {
		t.Fatalf("explicit sidecar path did not take precedence: %v", err)
	}
}

func TestVerifySidecarRejectsSymlinkNonExecutableAndWrongBinary(t *testing.T) {
	// The wrong-architecture fixture below is a Mach-O header, so this test
	// exercises the darwin/arm64 platform table explicitly and runs anywhere.
	platform := darwinARM64Sidecars
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("not chrome"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySidecar(platform, link); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("sidecar symlink was accepted: %v", err)
	}
	nonExecutable := filepath.Join(root, "non-executable")
	if err := os.WriteFile(nonExecutable, []byte("not chrome"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySidecar(platform, nonExecutable); err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("non-executable sidecar was accepted: %v", err)
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		// The test binary is a genuine arm64 Mach-O, so it passes the format
		// and architecture checks and must fail only on its hash.
		current, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := verifySidecar(platform, current); err == nil || !strings.Contains(err.Error(), "SHA-256") {
			t.Fatalf("wrong executable hash was accepted: %v", err)
		}
	}
	x86 := filepath.Join(root, "x86_64")
	header := make([]byte, 32)
	binary.LittleEndian.PutUint32(header[0:4], 0xfeedfacf)
	binary.LittleEndian.PutUint32(header[4:8], 0x01000007)
	binary.LittleEndian.PutUint32(header[8:12], 3)
	binary.LittleEndian.PutUint32(header[12:16], 2)
	if err := os.WriteFile(x86, header, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySidecar(platform, x86); err == nil || !strings.Contains(err.Error(), "expected arm64") {
		t.Fatalf("wrong-architecture executable was accepted: %v", err)
	}
}

func TestAllowRenderRequestRestrictsFilesToRegularGuideEntries(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, ".print.html")
	asset := filepath.Join(root, "assets", "image.png")
	if err := os.Mkdir(filepath.Dir(asset), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{input, asset} {
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if allowed, _, reason := allowRenderRequest(fileURL(input), root, input); !allowed || reason != "" {
		t.Fatalf("transient input rejected: %s", reason)
	}
	if allowed, _, reason := allowRenderRequest(fileURL(asset), root, input); !allowed || reason != "" {
		t.Fatalf("local asset rejected: %s", reason)
	}
	if allowed, _, _ := allowRenderRequest("https://example.test/image.png", root, input); allowed {
		t.Fatal("network request was accepted")
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if allowed, _, _ := allowRenderRequest(fileURL(outside), root, input); allowed {
		t.Fatal("foreign file request was accepted")
	}
	link := filepath.Join(root, "assets", "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if allowed, _, reason := allowRenderRequest(fileURL(link), root, input); allowed || !strings.Contains(reason, "symbolic link") {
		t.Fatalf("symlink request was not rejected: allowed=%v reason=%q", allowed, reason)
	}
}

func fileURL(name string) string {
	return "file://" + filepath.ToSlash(name)
}
