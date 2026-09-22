package releasepkg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"aos-cx-docs-dldr/internal/pdfgen"
)

type moduleJSON struct {
	Path    string      `json:"Path"`
	Version string      `json:"Version"`
	Sum     string      `json:"Sum"`
	Dir     string      `json:"Dir"`
	Main    bool        `json:"Main"`
	Replace *moduleJSON `json:"Replace"`
}

type packageJSON struct {
	Module *moduleJSON `json:"Module"`
}

type spdxDocument struct {
	SPDXVersion       string             `json:"spdxVersion"`
	DataLicense       string             `json:"dataLicense"`
	SPDXID            string             `json:"SPDXID"`
	Name              string             `json:"name"`
	DocumentNamespace string             `json:"documentNamespace"`
	CreationInfo      spdxCreationInfo   `json:"creationInfo"`
	DocumentDescribes []string           `json:"documentDescribes"`
	Packages          []spdxPackage      `json:"packages"`
	Relationships     []spdxRelationship `json:"relationships"`
	Annotations       []spdxAnnotation   `json:"annotations"`
}

type spdxCreationInfo struct {
	Created  string   `json:"created"`
	Creators []string `json:"creators"`
	Comment  string   `json:"comment"`
}

type spdxPackage struct {
	Name             string         `json:"name"`
	SPDXID           string         `json:"SPDXID"`
	VersionInfo      string         `json:"versionInfo,omitempty"`
	DownloadLocation string         `json:"downloadLocation"`
	FilesAnalyzed    bool           `json:"filesAnalyzed"`
	LicenseConcluded string         `json:"licenseConcluded"`
	LicenseDeclared  string         `json:"licenseDeclared"`
	CopyrightText    string         `json:"copyrightText"`
	Checksums        []spdxChecksum `json:"checksums,omitempty"`
	ExternalRefs     []spdxRef      `json:"externalRefs,omitempty"`
	Comment          string         `json:"comment,omitempty"`
}

type spdxChecksum struct {
	Algorithm     string `json:"algorithm"`
	ChecksumValue string `json:"checksumValue"`
}

type spdxRef struct {
	ReferenceCategory string `json:"referenceCategory"`
	ReferenceType     string `json:"referenceType"`
	ReferenceLocator  string `json:"referenceLocator"`
}

type spdxRelationship struct {
	SPDXElementID      string `json:"spdxElementId"`
	RelationshipType   string `json:"relationshipType"`
	RelatedSPDXElement string `json:"relatedSpdxElement"`
	Comment            string `json:"comment,omitempty"`
}

type spdxAnnotation struct {
	AnnotationDate string `json:"annotationDate"`
	AnnotationType string `json:"annotationType"`
	Annotator      string `json:"annotator"`
	Comment        string `json:"comment"`
}

type licenseEvidence struct {
	Name   string
	Path   string
	SHA256 string
	Text   string
}

func loadModules(path string) ([]Module, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 128<<20))
	seen := make(map[string]Module)
	for {
		var pkg packageJSON
		err := decoder.Decode(&pkg)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode Go dependency inventory: %w", err)
		}
		if pkg.Module == nil {
			continue
		}
		raw := *pkg.Module
		if raw.Main || raw.Path == "" {
			continue
		}
		module := Module{
			Path:    raw.Path,
			Version: raw.Version,
			Sum:     raw.Sum,
			Dir:     raw.Dir,
			Main:    raw.Main,
		}
		if raw.Replace != nil {
			module.Replace = &Module{
				Path: raw.Replace.Path, Version: raw.Replace.Version,
				Sum: raw.Replace.Sum, Dir: raw.Replace.Dir,
			}
		}
		key := module.Path + "\x00" + module.Version
		seen[key] = module
	}
	modules := make([]Module, 0, len(seen))
	for _, module := range seen {
		modules = append(modules, module)
	}
	sort.Slice(modules, func(i, j int) bool {
		if modules[i].Path != modules[j].Path {
			return modules[i].Path < modules[j].Path
		}
		return modules[i].Version < modules[j].Version
	})
	if len(modules) == 0 {
		return nil, fmt.Errorf("Go dependency inventory contains no linked modules")
	}
	return modules, nil
}

func moduleLicenseEvidence(module Module) ([]licenseEvidence, error) {
	root := module.Dir
	if module.Replace != nil && module.Replace.Dir != "" {
		root = module.Replace.Dir
	}
	if root == "" {
		return nil, fmt.Errorf("linked module %s %s has no local source directory", module.Path, module.Version)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read linked module %s %s: %w", module.Path, module.Version, err)
	}
	var names []string
	for _, entry := range entries {
		upper := strings.ToUpper(entry.Name())
		if entry.Type().IsRegular() &&
			(strings.HasPrefix(upper, "LICENSE") || strings.HasPrefix(upper, "COPYING") ||
				strings.HasPrefix(upper, "NOTICE")) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf(
			"linked module %s %s has no top-level LICENSE, COPYING, or NOTICE evidence", module.Path, module.Version,
		)
	}
	evidence := make([]licenseEvidence, 0, len(names))
	for _, name := range names {
		item, err := readLicenseEvidence(filepath.Join(root, name), name)
		if err != nil {
			return nil, fmt.Errorf("linked module %s %s: %w", module.Path, module.Version, err)
		}
		evidence = append(evidence, item)
	}
	return evidence, nil
}

func readLicenseEvidence(path, name string) (licenseEvidence, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return licenseEvidence{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return licenseEvidence{}, fmt.Errorf("license evidence is not a nonsymlink regular file: %s", path)
	}
	if info.Size() <= 0 || info.Size() > 8<<20 {
		return licenseEvidence{}, fmt.Errorf("license evidence size is invalid at %s: %d", path, info.Size())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return licenseEvidence{}, err
	}
	if strings.IndexByte(string(data), 0) >= 0 {
		return licenseEvidence{}, fmt.Errorf("license evidence contains NUL bytes: %s", path)
	}
	sum := sha256.Sum256(data)
	return licenseEvidence{
		Name: name, Path: path, SHA256: hex.EncodeToString(sum[:]), Text: string(data),
	}, nil
}

func generateSBOMAndNotices(
	bundle, dependenciesJSON, commit, version, goVersion, goRoot string, epoch int64,
) ([]Module, error) {
	modules, err := loadModules(dependenciesJSON)
	if err != nil {
		return nil, err
	}
	moduleEvidence := make(map[string][]licenseEvidence, len(modules))
	for _, module := range modules {
		evidence, err := moduleLicenseEvidence(module)
		if err != nil {
			return nil, err
		}
		moduleEvidence[module.Path+"\x00"+module.Version] = evidence
	}
	sidecarEvidence, err := sidecarLicenseEvidence(bundle)
	if err != nil {
		return nil, err
	}
	goEvidence, err := goToolchainLicenseEvidence(goRoot)
	if err != nil {
		return nil, err
	}
	sidecarEvidence["Go toolchain and standard library"] = goEvidence
	sbom := buildSPDX(modules, moduleEvidence, sidecarEvidence, commit, version, goVersion, epoch)
	encoded, err := json.MarshalIndent(sbom, "", "  ")
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, '\n')
	if err := writeFile(filepath.Join(bundle, SBOMName), encoded, 0o644); err != nil {
		return nil, err
	}
	notices := buildNotices(modules, moduleEvidence, sidecarEvidence)
	if err := writeFile(filepath.Join(bundle, NoticesName), []byte(notices), 0o644); err != nil {
		return nil, err
	}
	return modules, nil
}

func goToolchainLicenseEvidence(goRoot string) ([]licenseEvidence, error) {
	if goRoot == "" {
		return nil, fmt.Errorf("GOROOT is required")
	}
	path, label, err := goToolchainLicensePath(goRoot)
	if err != nil {
		return nil, err
	}
	evidence, err := readLicenseEvidence(path, label)
	if err != nil {
		return nil, fmt.Errorf("Go toolchain license evidence: %w", err)
	}
	return []licenseEvidence{evidence}, nil
}

func goToolchainLicensePath(goRoot string) (string, string, error) {
	candidates := []struct {
		path  string
		label string
	}{
		{filepath.Join(goRoot, "LICENSE"), "GOROOT/LICENSE"},
		{filepath.Join(filepath.Dir(goRoot), "LICENSE"), "GOROOT/../LICENSE (Homebrew layout)"},
	}
	for _, candidate := range candidates {
		info, err := os.Lstat(candidate.path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() == 0 {
			return "", "", fmt.Errorf(
				"Go toolchain license evidence is not a nonempty nonsymlink regular file: %s", candidate.label,
			)
		}
		return candidate.path, candidate.label, nil
	}
	return "", "", fmt.Errorf("Go toolchain license evidence is missing at GOROOT/LICENSE and the Homebrew GOROOT/../LICENSE location")
}

func sidecarLicenseEvidence(bundle string) (map[string][]licenseEvidence, error) {
	roots := map[string]string{
		"Chrome for Testing headless shell": filepath.Join(
			bundle, "sidecars", "chrome-headless-shell-mac-arm64",
		),
		"qpdf": filepath.Join(bundle, "sidecars", "qpdf-mac-arm64"),
	}
	result := make(map[string][]licenseEvidence, len(roots)+2)
	for product, root := range roots {
		kind := "qpdf"
		if strings.HasPrefix(product, "Chrome") {
			kind = "chrome"
		}
		for _, relative := range requiredSidecarLicenses[kind] {
			evidence, err := readLicenseEvidence(
				filepath.Join(root, filepath.FromSlash(relative)), relative,
			)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", product, err)
			}
			switch {
			case strings.Contains(relative, "jpeg-turbo"):
				result["jpeg-turbo"] = append(result["jpeg-turbo"], evidence)
			case strings.Contains(relative, "openssl"):
				result["OpenSSL"] = append(result["OpenSSL"], evidence)
			default:
				result[product] = append(result[product], evidence)
			}
		}
	}
	return result, nil
}

func buildSPDX(
	modules []Module,
	moduleEvidence map[string][]licenseEvidence,
	sidecarEvidence map[string][]licenseEvidence,
	commit, version, goVersion string,
	epoch int64,
) spdxDocument {
	created := deterministicTime(epoch).Format("2006-01-02T15:04:05Z")
	productID := "SPDXRef-Package-aos-cx-docs-dldr"
	namespaceHash := sha256.New()
	fmt.Fprintf(namespaceHash, "%s\x00%s\x00%d", commit, version, epoch)
	for _, module := range modules {
		fmt.Fprintf(namespaceHash, "\x00%s\x00%s\x00%s", module.Path, module.Version, module.Sum)
	}
	namespace := fmt.Sprintf(
		"https://spdx.org/spdxdocs/aos-cx-docs-dldr-%s-%s",
		version, hex.EncodeToString(namespaceHash.Sum(nil))[:24],
	)
	document := spdxDocument{
		SPDXVersion:       SPDXVersion,
		DataLicense:       DataLicense,
		SPDXID:            "SPDXRef-DOCUMENT",
		Name:              "aos-cx-docs-dldr " + version + " macOS arm64 release package",
		DocumentNamespace: namespace,
		CreationInfo: spdxCreationInfo{
			Created:  created,
			Creators: []string{"Tool: aoscx-release-package"},
			Comment:  "Generated offline from the linked Go dependency graph and checksum-verified local sidecars.",
		},
		DocumentDescribes: []string{productID},
		Packages: []spdxPackage{{
			Name: "aos-cx-docs-dldr", SPDXID: productID, VersionInfo: version,
			DownloadLocation: "NOASSERTION", FilesAnalyzed: false,
			LicenseConcluded: ProductLicense, LicenseDeclared: ProductLicense,
			CopyrightText: "NOASSERTION",
			Comment:       "Application code is licensed under Apache-2.0; see LICENSE. Third-party components retain their own licenses.",
		}},
		Annotations: []spdxAnnotation{{
			AnnotationDate: created, AnnotationType: "OTHER",
			Annotator: "Tool: aoscx-release-package",
			Comment:   "No license identifiers were inferred from text. Every third-party package is NOASSERTION and its local license evidence is reproduced in THIRD_PARTY_NOTICES.",
		}},
	}
	for _, module := range modules {
		id := spdxID("Go", module.Path+"@"+module.Version)
		pkg := spdxPackage{
			Name: module.Path, SPDXID: id, VersionInfo: module.Version,
			DownloadLocation: "NOASSERTION", FilesAnalyzed: false,
			LicenseConcluded: "NOASSERTION", LicenseDeclared: "NOASSERTION",
			CopyrightText: "NOASSERTION",
			ExternalRefs: []spdxRef{{
				ReferenceCategory: "PACKAGE-MANAGER",
				ReferenceType:     "purl",
				ReferenceLocator:  "pkg:golang/" + module.Path + "@" + module.Version,
			}},
			Comment: licenseComment(moduleEvidence[module.Path+"\x00"+module.Version]),
		}
		if module.Sum != "" {
			pkg.Comment += "; Go module dirhash " + module.Sum
		}
		if module.Replace != nil {
			pkg.Comment += "; modified local replacement " + module.Replace.Path
		}
		document.Packages = append(document.Packages, pkg)
		document.Relationships = append(document.Relationships, spdxRelationship{
			SPDXElementID: productID, RelationshipType: "DEPENDS_ON", RelatedSPDXElement: id,
		})
	}
	sidecars := []struct {
		name, version string
	}{
		{"Go toolchain and standard library", goVersion},
		{"Chrome for Testing headless shell", pdfgen.ChromeVersion},
		{"qpdf", QPDFVersion},
		{"jpeg-turbo", JPEGVersion},
		{"OpenSSL", OpenSSLVersion},
	}
	for _, sidecar := range sidecars {
		id := spdxID("Sidecar", sidecar.name)
		document.Packages = append(document.Packages, spdxPackage{
			Name: sidecar.name, SPDXID: id, VersionInfo: sidecar.version,
			DownloadLocation: "NOASSERTION", FilesAnalyzed: false,
			LicenseConcluded: "NOASSERTION", LicenseDeclared: "NOASSERTION",
			CopyrightText: "NOASSERTION",
			Comment:       licenseComment(sidecarEvidence[sidecar.name]),
		})
		relationship := spdxRelationship{
			SPDXElementID: productID, RelationshipType: "DEPENDS_ON", RelatedSPDXElement: id,
			Comment: "Bundled executable-relative runtime sidecar component.",
		}
		if sidecar.name == "Go toolchain and standard library" {
			relationship = spdxRelationship{
				SPDXElementID: id, RelationshipType: "BUILD_TOOL_OF", RelatedSPDXElement: productID,
				Comment: "Exact compiler and linked standard-library toolchain.",
			}
		}
		document.Relationships = append(document.Relationships, relationship)
	}
	return document
}

func spdxID(prefix, value string) string {
	sum := sha256.Sum256([]byte(value))
	return "SPDXRef-" + prefix + "-" + hex.EncodeToString(sum[:8])
}

func licenseComment(evidence []licenseEvidence) string {
	parts := make([]string, 0, len(evidence))
	for _, item := range evidence {
		parts = append(parts, item.Name+" sha256:"+item.SHA256)
	}
	return "License classification not inferred; retained evidence: " + strings.Join(parts, ", ")
}

func buildNotices(
	modules []Module,
	moduleEvidence map[string][]licenseEvidence,
	sidecarEvidence map[string][]licenseEvidence,
) string {
	var builder strings.Builder
	builder.WriteString("THIRD-PARTY NOTICES FOR AOS-CX-DOCS-DLDR\n")
	builder.WriteString("===================================\n\n")
	fmt.Fprintf(&builder, "The aos-cx-docs-dldr application code is licensed under %s; see LICENSE.\n", ProductLicense)
	builder.WriteString("Third-party components retain their own licenses; the application license does not relicense them or downloaded documentation.\n")
	builder.WriteString("This unsigned package is for personal/internal distribution. No license identifiers are inferred here.\n")
	builder.WriteString("Review the reproduced source license and notice texts below before broader distribution.\n\n")
	for _, module := range modules {
		fmt.Fprintf(&builder, "----- Go module: %s %s -----\n", module.Path, module.Version)
		if module.Replace != nil {
			fmt.Fprintf(&builder, "Modified local replacement: %s\n", module.Replace.Path)
		}
		for _, evidence := range moduleEvidence[module.Path+"\x00"+module.Version] {
			appendEvidence(&builder, evidence)
		}
	}
	for _, name := range []string{
		"Go toolchain and standard library",
		"Chrome for Testing headless shell",
		"qpdf",
		"jpeg-turbo",
		"OpenSSL",
	} {
		fmt.Fprintf(&builder, "----- Sidecar component: %s -----\n", name)
		for _, evidence := range sidecarEvidence[name] {
			appendEvidence(&builder, evidence)
		}
	}
	return builder.String()
}

func appendEvidence(builder *strings.Builder, evidence licenseEvidence) {
	fmt.Fprintf(builder, "Evidence: %s\nSHA-256: %s\n\n", evidence.Name, evidence.SHA256)
	builder.WriteString(evidence.Text)
	if !strings.HasSuffix(evidence.Text, "\n") {
		builder.WriteByte('\n')
	}
	builder.WriteByte('\n')
}
