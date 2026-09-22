package releasepkg

import (
	"debug/buildinfo"
	"debug/macho"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"aos-cx-docs-dldr/internal/model"
)

func GenerateMetadata(options MetadataOptions) (Manifest, error) {
	if options.Commit == "" || options.GoVersion == "" || options.SourceDateEpoch <= 0 {
		return Manifest{}, fmt.Errorf("commit, Go version, and positive source date epoch are required")
	}
	bundle, err := filepath.Abs(options.Bundle)
	if err != nil {
		return Manifest{}, err
	}
	license, err := readLicenseEvidence(filepath.Join(options.Repository, "LICENSE"), "LICENSE")
	if err != nil {
		return Manifest{}, fmt.Errorf("application license: %w", err)
	}
	if err := writeFile(filepath.Join(bundle, "LICENSE"), []byte(license.Text), 0o644); err != nil {
		return Manifest{}, err
	}
	binaryInfo, err := buildinfo.ReadFile(filepath.Join(bundle, model.ExecutableName))
	if err != nil {
		return Manifest{}, fmt.Errorf("read application build information: %w", err)
	}
	if !strings.Contains(options.GoVersion, binaryInfo.GoVersion) {
		return Manifest{}, fmt.Errorf(
			"application compiler %q differs from Go command %q", binaryInfo.GoVersion, options.GoVersion,
		)
	}
	goRootVersion, err := hashFile(filepath.Join(options.GOROOT, "VERSION"))
	if err != nil {
		return Manifest{}, fmt.Errorf("GOROOT VERSION: %w", err)
	}
	goRootLicensePath, goRootLicenseSource, err := goToolchainLicensePath(options.GOROOT)
	if err != nil {
		return Manifest{}, err
	}
	goRootLicense, err := hashFile(goRootLicensePath)
	if err != nil {
		return Manifest{}, fmt.Errorf("GOROOT LICENSE: %w", err)
	}
	modules, err := generateSBOMAndNotices(
		bundle, options.DependenciesJSON, options.Commit, model.Version,
		binaryInfo.GoVersion, options.GOROOT, options.SourceDateEpoch,
	)
	if err != nil {
		return Manifest{}, err
	}
	sidecars, err := VerifySidecars(
		filepath.Join(bundle, "sidecars", "chrome-headless-shell-mac-arm64"),
		filepath.Join(bundle, "sidecars", "qpdf-mac-arm64"),
	)
	if err != nil {
		return Manifest{}, err
	}
	moduleInputs := make([]FileIdentity, 0, 4)
	for _, relative := range []string{
		"go.mod",
		"go.sum",
		"third_party/json5/go.mod",
		"third_party/json5/LICENSE",
		"third_party/json5/UPSTREAM.md",
	} {
		path := filepath.Join(options.Repository, filepath.FromSlash(relative))
		identity, err := fileIdentity(options.Repository, path)
		if err != nil {
			return Manifest{}, fmt.Errorf("module input %s: %w", relative, err)
		}
		moduleInputs = append(moduleInputs, identity)
	}
	sbom, err := hashFile(filepath.Join(bundle, SBOMName))
	if err != nil {
		return Manifest{}, err
	}
	notices, err := hashFile(filepath.Join(bundle, NoticesName))
	if err != nil {
		return Manifest{}, err
	}
	payload, err := collectFiles(bundle, map[string]bool{
		ManifestName:  true,
		ChecksumsName: true,
	})
	if err != nil {
		return Manifest{}, err
	}
	manifest := Manifest{
		SchemaVersion:      ManifestSchema,
		Application:        "aos-cx-docs-dldr",
		ApplicationVersion: model.Version,
		Source: SourceIdentity{
			Commit: options.Commit, HeadTree: options.HeadTree,
			Dirty: options.Dirty, ContentSHA256: options.SourceContentSHA256,
			SourceFileCount: options.SourceFileCount,
		},
		Build: BuildIdentity{
			GoVersion: binaryInfo.GoVersion, GoCommandVersion: options.GoVersion,
			GOROOTVersionSHA256: goRootVersion.SHA256,
			GOROOTLicenseSource: goRootLicenseSource,
			GOROOTLicenseSHA256: goRootLicense.SHA256,
			TargetOS:            TargetOS,
			TargetArch:          TargetArchitecture, CGOEnabled: false,
			Flags:           []string{"-trimpath", "-buildvcs=false", "-ldflags=-buildid="},
			SourceDateEpoch: options.SourceDateEpoch,
			TimestampUTC:    deterministicTime(options.SourceDateEpoch).Format("2006-01-02T15:04:05Z"),
		},
		Dependencies: DependencyInventory{
			Scope:       "linked-application-binary-closure",
			Command:     "go list -deps -json ./cmd/" + model.ExecutableName,
			ModuleCount: len(modules),
		},
		ModuleInputs: moduleInputs,
		Sidecars:     sidecars,
		SBOM:         sbom,
		Notices:      notices,
		Assembly: AssemblyResults{
			OfflineInputsOnly: true, NetworkToolsInvoked: false,
			InputChecksumsSHA256:  options.InputChecksums,
			InputChecksummedFiles: options.InputFileCount,
			Smoke: SmokeResults{
				MovedLayoutConversion: options.MovedSmokeComplete,
				OfflineHelp:           options.HelpSmokeComplete,
				OfflineVersion:        options.VersionSmokeComplete,
			},
			TrustStatus: TrustStatus, Signed: false, Notarized: false,
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
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	encoded = append(encoded, '\n')
	if err := writeFile(filepath.Join(bundle, ManifestName), encoded, 0o644); err != nil {
		return Manifest{}, err
	}
	if err := WriteChecksums(bundle, filepath.Join(bundle, ChecksumsName), nil); err != nil {
		return Manifest{}, err
	}
	if err := VerifyBundle(bundle); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func VerifyBundle(bundle string) error {
	return verifyBundle(bundle, VerifySidecars)
}

func verifyBundle(
	bundle string,
	sidecarVerifier func(string, string) ([]SidecarIdentity, error),
) error {
	data, err := os.ReadFile(filepath.Join(bundle, ManifestName))
	if err != nil {
		return err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("decode package manifest: %w", err)
	}
	if manifest.SchemaVersion != ManifestSchema ||
		manifest.Application != "aos-cx-docs-dldr" ||
		manifest.ApplicationVersion != model.Version {
		return fmt.Errorf("incompatible package manifest identity")
	}
	if len(manifest.Source.Commit) != 40 || len(manifest.Source.HeadTree) != 40 ||
		len(manifest.Source.ContentSHA256) != 64 || manifest.Source.SourceFileCount <= 0 ||
		len(manifest.ModuleInputs) < 3 ||
		len(manifest.Sidecars) != 2 || manifest.Assembly.InputChecksummedFiles <= 0 ||
		len(manifest.Assembly.InputChecksumsSHA256) != 64 {
		return fmt.Errorf("package manifest provenance is incomplete")
	}
	if manifest.Build.GoVersion == "" || manifest.Build.GoCommandVersion == "" ||
		manifest.Build.GOROOTLicenseSource == "" ||
		manifest.Dependencies.Scope != "linked-application-binary-closure" ||
		manifest.Dependencies.Command != "go list -deps -json ./cmd/"+model.ExecutableName ||
		manifest.Dependencies.ModuleCount <= 0 {
		return fmt.Errorf("package toolchain or dependency scope is incomplete")
	}
	if err := verifyHexIdentity("GOROOT VERSION", manifest.Build.GOROOTVersionSHA256, 32); err != nil {
		return err
	}
	if err := verifyHexIdentity("GOROOT LICENSE", manifest.Build.GOROOTLicenseSHA256, 32); err != nil {
		return err
	}
	if err := verifyHexIdentity("source commit", manifest.Source.Commit, 20); err != nil {
		return err
	}
	if err := verifyHexIdentity("source HEAD tree", manifest.Source.HeadTree, 20); err != nil {
		return err
	}
	if err := verifyHexIdentity("source content", manifest.Source.ContentSHA256, 32); err != nil {
		return err
	}
	if err := verifyHexIdentity("input checksums", manifest.Assembly.InputChecksumsSHA256, 32); err != nil {
		return err
	}
	expectedPolicy := ChecksumPolicy{
		ManifestExcludes:  []string{ManifestName, ChecksumsName},
		ChecksumsExcludes: []string{ChecksumsName},
		DetachedArtifacts: []string{
			PackageRootName + ".zip",
			PackageRootName + ".zip.sha256",
			PackageRootName + ".zip.provenance.json",
		},
	}
	if !reflect.DeepEqual(manifest.ChecksumPolicy, expectedPolicy) {
		return fmt.Errorf("package checksum exclusion policy differs from schema 1")
	}
	if manifest.Build.TargetOS != TargetOS || manifest.Build.TargetArch != TargetArchitecture ||
		manifest.Build.CGOEnabled || manifest.Assembly.NetworkToolsInvoked ||
		!manifest.Assembly.OfflineInputsOnly || manifest.Assembly.Signed ||
		manifest.Assembly.Notarized || manifest.Assembly.TrustStatus != TrustStatus ||
		!manifest.Assembly.Smoke.MovedLayoutConversion ||
		!manifest.Assembly.Smoke.OfflineHelp || !manifest.Assembly.Smoke.OfflineVersion {
		return fmt.Errorf("package manifest build, trust, or smoke contract is incomplete")
	}
	actualPayload, err := collectFiles(bundle, map[string]bool{
		ManifestName: true, ChecksumsName: true,
	})
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actualPayload, manifest.PayloadFiles) {
		return fmt.Errorf("package payload file set or identity differs from manifest")
	}
	if err := verifyApplicationBinary(filepath.Join(bundle, model.ExecutableName)); err != nil {
		return err
	}
	if _, err := readLicenseEvidence(filepath.Join(bundle, "LICENSE"), "LICENSE"); err != nil {
		return fmt.Errorf("application license: %w", err)
	}
	if err := verifyArtifact(bundle, manifest.SBOM, SBOMName); err != nil {
		return err
	}
	if err := verifyArtifact(bundle, manifest.Notices, NoticesName); err != nil {
		return err
	}
	checksumEntries, err := parseChecksums(filepath.Join(bundle, ChecksumsName))
	if err != nil {
		return err
	}
	actualChecksummed, err := collectFiles(bundle, map[string]bool{ChecksumsName: true})
	if err != nil {
		return err
	}
	if len(checksumEntries) != len(actualChecksummed) {
		return fmt.Errorf("in-package checksum set is incomplete")
	}
	for _, file := range actualChecksummed {
		if checksumEntries[file.Path] != file.SHA256 {
			return fmt.Errorf("in-package checksum mismatch or omission: %s", file.Path)
		}
	}
	if _, err := sidecarVerifier(
		filepath.Join(bundle, "sidecars", "chrome-headless-shell-mac-arm64"),
		filepath.Join(bundle, "sidecars", "qpdf-mac-arm64"),
	); err != nil {
		return err
	}
	var spdx spdxDocument
	sbomData, err := os.ReadFile(filepath.Join(bundle, SBOMName))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(sbomData, &spdx); err != nil {
		return fmt.Errorf("decode SPDX SBOM: %w", err)
	}
	if spdx.SPDXVersion != SPDXVersion || spdx.DataLicense != DataLicense ||
		len(spdx.Packages) == 0 || spdx.Packages[0].LicenseDeclared != ProductLicense {
		return fmt.Errorf("SPDX SBOM identity or product license contract is invalid")
	}
	return nil
}

func verifyHexIdentity(name, value string, size int) error {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != size {
		return fmt.Errorf("%s is not a %d-byte hexadecimal identity", name, size)
	}
	return nil
}

func verifyApplicationBinary(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("application binary: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("application binary must be an executable nonsymlink regular file")
	}
	file, err := macho.Open(path)
	if err != nil {
		return fmt.Errorf("application binary is not Mach-O: %w", err)
	}
	defer file.Close()
	if file.Cpu != macho.CpuArm64 {
		return fmt.Errorf("application binary architecture is %s, expected arm64", file.Cpu)
	}
	return nil
}

func verifyArtifact(bundle string, expected ArtifactIdentity, requiredPath string) error {
	if expected.Path != requiredPath {
		return fmt.Errorf("artifact path is %q, expected %q", expected.Path, requiredPath)
	}
	actual, err := hashFile(filepath.Join(bundle, requiredPath))
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("artifact identity differs for %s", requiredPath)
	}
	return nil
}

func loadManifest(bundle string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(bundle, ManifestName))
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func sortedPaths(files []FileIdentity) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	sort.Strings(paths)
	return paths
}
