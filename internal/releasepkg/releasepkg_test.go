package releasepkg

import (
	"archive/zip"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/model"
)

func TestVerifyChecksumsRejectsOmittedUnexpectedAndChangedFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "sidecars", "chrome", "binary"), "chrome", 0o700)
	writeTestFile(t, filepath.Join(root, "sidecars", "qpdf", "binary"), "qpdf", 0o700)
	checksums := filepath.Join(root, ChecksumsName)
	if err := WriteChecksums(root, checksums, nil); err != nil {
		t.Fatal(err)
	}
	result, err := VerifyChecksums(root, checksums, []string{
		filepath.Join(root, "sidecars", "chrome"),
		filepath.Join(root, "sidecars", "qpdf"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FileCount != 2 || len(result.ChecksumsSHA256) != 64 {
		t.Fatalf("unexpected verification: %+v", result)
	}

	writeTestFile(t, filepath.Join(root, "unexpected"), "extra", 0o600)
	if _, err := VerifyChecksums(root, checksums, nil); err == nil ||
		!strings.Contains(err.Error(), "file set") {
		t.Fatalf("unexpected file was accepted: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "unexpected")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sidecars", "qpdf", "binary"), []byte("changed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyChecksums(root, checksums, nil); err == nil ||
		!strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("changed file was accepted: %v", err)
	}
}

func TestVerifyChecksumsRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "target"), "target", 0o600)
	if err := os.Symlink("target", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	checksums := filepath.Join(root, ChecksumsName)
	if err := os.WriteFile(checksums, []byte(strings.Repeat("0", 64)+"  ./target\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyChecksums(root, checksums, nil); err == nil ||
		!strings.Contains(err.Error(), "symbolic links") {
		t.Fatalf("symlink was accepted: %v", err)
	}
}

func TestStageTreePreservesRegularFilesAndRejectsSymlink(t *testing.T) {
	source := t.TempDir()
	writeTestFile(t, filepath.Join(source, "bin", "tool"), "tool", 0o755)
	destination := filepath.Join(t.TempDir(), "copy")
	if err := StageTree(source, destination); err != nil {
		t.Fatal(err)
	}
	copied, err := os.ReadFile(filepath.Join(destination, "bin", "tool"))
	if err != nil || string(copied) != "tool" {
		t.Fatalf("unexpected copied file: %q err=%v", copied, err)
	}
	info, err := os.Stat(filepath.Join(destination, "bin", "tool"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("unexpected copied mode: %v err=%v", info.Mode(), err)
	}

	unsafe := t.TempDir()
	writeTestFile(t, filepath.Join(unsafe, "target"), "target", 0o600)
	if err := os.Symlink("target", filepath.Join(unsafe, "link")); err != nil {
		t.Fatal(err)
	}
	if err := StageTree(unsafe, filepath.Join(t.TempDir(), "unsafe")); err == nil ||
		!strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlink tree was accepted: %v", err)
	}
}

func TestHashSourceListBindsPathModeAndContent(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "a"), "one", 0o600)
	writeTestFile(t, filepath.Join(root, "sub", "b"), "two", 0o644)
	list := filepath.Join(t.TempDir(), "files.list")
	if err := os.WriteFile(list, []byte("a\x00sub/b\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, count, err := HashSourceList(root, list)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || len(first) != 64 {
		t.Fatalf("unexpected source identity: hash=%q count=%d", first, count)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "b"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, _, err := HashSourceList(root, list)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("source content change did not change identity")
	}
}

func TestCreateZIPUsesSortedSafePathsAndDeterministicTimestamp(t *testing.T) {
	bundle := t.TempDir()
	writeTestFile(t, filepath.Join(bundle, "z"), "z", 0o600)
	writeTestFile(t, filepath.Join(bundle, "a", "run"), "a", 0o755)
	zipPath := filepath.Join(t.TempDir(), "package.zip")
	const epoch = 1_800_000_000
	if err := CreateZIP(bundle, zipPath, "bundle", epoch); err != nil {
		t.Fatal(err)
	}
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var names []string
	for _, file := range reader.File {
		names = append(names, file.Name)
		if got := file.Modified.Unix(); got != epoch {
			t.Fatalf("timestamp=%d want=%d for %s", got, epoch, file.Name)
		}
		if !file.Mode().IsRegular() {
			t.Fatalf("nonregular ZIP entry: %s", file.Name)
		}
	}
	if want := []string{"bundle/a/run", "bundle/z"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("ZIP names=%v want=%v", names, want)
	}
}

func TestVerifyBundleRejectsUnexpectedPayload(t *testing.T) {
	bundle := t.TempDir()
	writeTestMachO(t, filepath.Join(bundle, model.ExecutableName))
	writeTestFile(t, filepath.Join(bundle, SBOMName), testSPDX(t), 0o644)
	writeTestFile(t, filepath.Join(bundle, NoticesName), "notices", 0o644)
	writeTestFile(t, filepath.Join(bundle, "LICENSE"), "application license", 0o644)
	payload, err := collectFiles(bundle, map[string]bool{ManifestName: true, ChecksumsName: true})
	if err != nil {
		t.Fatal(err)
	}
	sbom, _ := hashFile(filepath.Join(bundle, SBOMName))
	notices, _ := hashFile(filepath.Join(bundle, NoticesName))
	manifest := Manifest{
		SchemaVersion: ManifestSchema, Application: "aos-cx-docs-dldr",
		ApplicationVersion: model.Version,
		Source: SourceIdentity{
			Commit: strings.Repeat("a", 40), HeadTree: strings.Repeat("b", 40),
			ContentSHA256: strings.Repeat("c", 64), SourceFileCount: 3,
		},
		Build: BuildIdentity{
			GoVersion: "go1.27.1", GoCommandVersion: "go version go1.27.1 darwin/arm64",
			GOROOTVersionSHA256: strings.Repeat("d", 64),
			GOROOTLicenseSource: "GOROOT/LICENSE",
			GOROOTLicenseSHA256: strings.Repeat("e", 64),
			TargetOS:            TargetOS, TargetArch: TargetArchitecture,
			Flags: []string{"-trimpath"}, SourceDateEpoch: 1_800_000_000,
		},
		Dependencies: DependencyInventory{
			Scope:   "linked-application-binary-closure",
			Command: "go list -deps -json ./cmd/" + model.ExecutableName, ModuleCount: 1,
		},
		ModuleInputs: []FileIdentity{{Path: "go.mod"}, {Path: "go.sum"}, {Path: "third_party/json5/go.mod"}},
		Sidecars:     []SidecarIdentity{{Name: "chrome"}, {Name: "qpdf"}},
		SBOM:         sbom, Notices: notices,
		Assembly: AssemblyResults{
			OfflineInputsOnly: true, InputChecksumsSHA256: strings.Repeat("b", 64),
			InputChecksummedFiles: 2, TrustStatus: TrustStatus,
			Smoke: SmokeResults{MovedLayoutConversion: true, OfflineHelp: true, OfflineVersion: true},
		},
		ChecksumPolicy: ChecksumPolicy{
			ManifestExcludes:  []string{ManifestName, ChecksumsName},
			ChecksumsExcludes: []string{ChecksumsName},
			DetachedArtifacts: []string{
				PackageRootName + ".zip",
				PackageRootName + ".zip.sha256",
				PackageRootName + ".zip.provenance.json",
			},
		},
		PayloadFiles: payload,
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(bundle, ManifestName), string(data)+"\n", 0o644)
	if err := WriteChecksums(bundle, filepath.Join(bundle, ChecksumsName), nil); err != nil {
		t.Fatal(err)
	}
	fakeSidecars := func(string, string) ([]SidecarIdentity, error) {
		return manifest.Sidecars, nil
	}
	if err := verifyBundle(bundle, fakeSidecars); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(bundle, "unexpected"), "extra", 0o600)
	if err := verifyBundle(bundle, fakeSidecars); err == nil ||
		!strings.Contains(err.Error(), "payload file set") {
		t.Fatalf("unexpected payload was accepted: %v", err)
	}
}

func TestPackageRootTracksApplicationVersion(t *testing.T) {
	want := "aos-cx-docs-dldr-" + model.Version + "-macos-arm64"
	if PackageRootName != want {
		t.Fatalf("PackageRootName=%q want=%q", PackageRootName, want)
	}
}

func TestModuleLicenseEvidenceRejectsMissingLicense(t *testing.T) {
	module := Module{Path: "example.invalid/module", Version: "v1.0.0", Dir: t.TempDir()}
	if _, err := moduleLicenseEvidence(module); err == nil ||
		!strings.Contains(err.Error(), "no top-level LICENSE") {
		t.Fatalf("missing license evidence was accepted: %v", err)
	}
}

func TestGoToolchainLicenseEvidenceRejectsSymlink(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "libexec")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "real-license"), "license", 0o644)
	if err := os.Symlink("real-license", filepath.Join(root, "LICENSE")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(parent, "LICENSE"), "parent", 0o644)
	if _, err := goToolchainLicenseEvidence(root); err == nil ||
		!strings.Contains(err.Error(), "nonsymlink") {
		t.Fatalf("GOROOT license symlink was accepted: %v", err)
	}
}

func TestGoToolchainLicenseEvidenceAcceptsHomebrewLayout(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "libexec")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(parent, "LICENSE"), "go license", 0o644)
	evidence, err := goToolchainLicenseEvidence(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 1 || evidence[0].Name != "GOROOT/../LICENSE (Homebrew layout)" {
		t.Fatalf("unexpected Homebrew license evidence: %+v", evidence)
	}
}

func TestSPDXDistinguishesToolchainAndModifiedLocalModule(t *testing.T) {
	module := Module{
		Path: "github.com/titanous/json5", Version: "v1.0.0", Sum: "h1:upstream",
		Replace: &Module{Path: "./third_party/json5", Dir: "/repo/third_party/json5"},
	}
	evidence := []licenseEvidence{{Name: "LICENSE", SHA256: strings.Repeat("a", 64)}}
	sidecars := map[string][]licenseEvidence{
		"Go toolchain and standard library": evidence,
		"Chrome for Testing headless shell": evidence,
		"qpdf":                              evidence, "jpeg-turbo": evidence, "OpenSSL": evidence,
	}
	document := buildSPDX(
		[]Module{module},
		map[string][]licenseEvidence{module.Path + "\x00" + module.Version: evidence},
		sidecars,
		strings.Repeat("b", 40), model.Version, "go1.27.1", 1_800_000_000,
	)
	if document.Packages[0].LicenseDeclared != "Apache-2.0" || document.Packages[0].LicenseConcluded != "Apache-2.0" {
		t.Fatal("application license must be Apache-2.0")
	}
	for _, pkg := range document.Packages[1:] {
		if pkg.LicenseDeclared != "NOASSERTION" || pkg.LicenseConcluded != "NOASSERTION" {
			t.Fatalf("application license incorrectly applied to third party %s", pkg.Name)
		}
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, required := range []string{
		`"name":"Go toolchain and standard library"`,
		`"relationshipType":"BUILD_TOOL_OF"`,
		`modified local replacement ./third_party/json5`,
		`Go module dirhash h1:upstream`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("SPDX output does not distinguish required evidence %q: %s", required, text)
		}
	}
}

func TestLoadModulesReadsPackageModuleObjectsAndDeduplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deps.json")
	data := `{"ImportPath":"example/one","Module":{"Path":"example.com/dep","Version":"v1.2.3","Sum":"h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","Dir":"/cache/dep"}}` + "\n" +
		`{"ImportPath":"example/two","Module":{"Path":"example.com/dep","Version":"v1.2.3","Sum":"h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","Dir":"/cache/dep"}}` + "\n" +
		`{"ImportPath":"example/main","Module":{"Path":"example.com/main","Main":true,"Dir":"/repo"}}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	modules, err := loadModules(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(modules) != 1 || modules[0].Path != "example.com/dep" || modules[0].Version != "v1.2.3" {
		t.Fatalf("unexpected modules: %+v", modules)
	}
}

func writeTestFile(t *testing.T, path, value string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), mode); err != nil {
		t.Fatal(err)
	}
}

func testSPDX(t *testing.T) string {
	t.Helper()
	data, err := json.Marshal(spdxDocument{
		SPDXVersion: SPDXVersion,
		DataLicense: DataLicense,
		Packages: []spdxPackage{{
			Name: "aos-cx-docs-dldr", SPDXID: "SPDXRef-Package-aos-cx-docs-dldr",
			DownloadLocation: "NOASSERTION", LicenseConcluded: ProductLicense,
			LicenseDeclared: ProductLicense, CopyrightText: "NOASSERTION",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeTestMachO(t *testing.T, path string) {
	t.Helper()
	header := make([]byte, 32)
	binary.LittleEndian.PutUint32(header[0:4], 0xfeedfacf)
	binary.LittleEndian.PutUint32(header[4:8], 0x0100000c)
	binary.LittleEndian.PutUint32(header[8:12], 0)
	binary.LittleEndian.PutUint32(header[12:16], 2)
	if err := os.WriteFile(path, header, 0o755); err != nil {
		t.Fatal(err)
	}
}
