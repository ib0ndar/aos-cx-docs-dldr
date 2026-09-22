package releasepkg

import (
	"time"

	"aos-cx-docs-dldr/internal/model"
)

const (
	ManifestName       = "PACKAGE-MANIFEST.json"
	ChecksumsName      = "SHA256SUMS"
	SBOMName           = "SBOM.spdx.json"
	NoticesName        = "THIRD_PARTY_NOTICES"
	ManifestSchema     = 1
	ProvenanceSchema   = 1
	SPDXVersion        = "SPDX-2.3"
	DataLicense        = "CC0-1.0"
	TrustStatus        = "unsigned-internal-manual-approval-required"
	ProductLicense     = "Apache-2.0"
	TargetOS           = "darwin"
	TargetArchitecture = "arm64"

	// PackagePlatformLabel names the only platform this build-only helper
	// assembles. The other deliverable, the linux/amd64 container, is produced
	// by the Dockerfile and scripts/package-container.sh, not by this helper.
	PackagePlatformLabel = "macos-arm64"
)

// PackageRootName is derived from the authoritative application version so a
// release bump cannot leave a stale package identity behind.
var PackageRootName = "aos-cx-docs-dldr-" + model.Version + "-" + PackagePlatformLabel

type FileIdentity struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   string `json:"mode"`
}

type SourceIdentity struct {
	Commit          string `json:"commit"`
	HeadTree        string `json:"head_tree"`
	Dirty           bool   `json:"dirty"`
	ContentSHA256   string `json:"content_sha256"`
	SourceFileCount int    `json:"source_file_count"`
}

type BuildIdentity struct {
	GoVersion           string   `json:"go_version"`
	GoCommandVersion    string   `json:"go_command_version"`
	GOROOTVersionSHA256 string   `json:"goroot_version_sha256"`
	GOROOTLicenseSource string   `json:"goroot_license_source"`
	GOROOTLicenseSHA256 string   `json:"goroot_license_sha256"`
	TargetOS            string   `json:"target_os"`
	TargetArch          string   `json:"target_arch"`
	CGOEnabled          bool     `json:"cgo_enabled"`
	Flags               []string `json:"flags"`
	SourceDateEpoch     int64    `json:"source_date_epoch"`
	TimestampUTC        string   `json:"timestamp_utc"`
}

type DependencyInventory struct {
	Scope       string `json:"scope"`
	Command     string `json:"command"`
	ModuleCount int    `json:"module_count"`
}

type SidecarIdentity struct {
	Name       string         `json:"name"`
	Version    string         `json:"version"`
	Revision   string         `json:"revision,omitempty"`
	Platform   string         `json:"platform"`
	Components []FileIdentity `json:"components"`
}

type ArtifactIdentity struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type ChecksumPolicy struct {
	ManifestExcludes  []string `json:"manifest_excludes"`
	ChecksumsExcludes []string `json:"checksums_excludes"`
	DetachedArtifacts []string `json:"detached_artifacts"`
}

type SmokeResults struct {
	MovedLayoutConversion bool `json:"moved_layout_conversion"`
	OfflineHelp           bool `json:"offline_help"`
	OfflineVersion        bool `json:"offline_version"`
}

type AssemblyResults struct {
	OfflineInputsOnly     bool         `json:"offline_inputs_only"`
	NetworkToolsInvoked   bool         `json:"network_tools_invoked"`
	InputChecksumsSHA256  string       `json:"input_checksums_sha256"`
	InputChecksummedFiles int          `json:"input_checksummed_files"`
	Smoke                 SmokeResults `json:"smoke"`
	TrustStatus           string       `json:"trust_status"`
	Signed                bool         `json:"signed"`
	Notarized             bool         `json:"notarized"`
}

type Manifest struct {
	SchemaVersion      int                 `json:"schema_version"`
	Application        string              `json:"application"`
	ApplicationVersion string              `json:"application_version"`
	Source             SourceIdentity      `json:"source"`
	Build              BuildIdentity       `json:"build"`
	Dependencies       DependencyInventory `json:"dependencies"`
	ModuleInputs       []FileIdentity      `json:"module_inputs"`
	Sidecars           []SidecarIdentity   `json:"sidecars"`
	SBOM               ArtifactIdentity    `json:"sbom"`
	Notices            ArtifactIdentity    `json:"third_party_notices"`
	Assembly           AssemblyResults     `json:"assembly"`
	ChecksumPolicy     ChecksumPolicy      `json:"checksum_policy"`
	PayloadFiles       []FileIdentity      `json:"payload_files"`
}

type DetachedProvenance struct {
	SchemaVersion      int              `json:"schema_version"`
	Application        string           `json:"application"`
	ApplicationVersion string           `json:"application_version"`
	Source             SourceIdentity   `json:"source"`
	TrustStatus        string           `json:"trust_status"`
	Signed             bool             `json:"signed"`
	Notarized          bool             `json:"notarized"`
	ZIP                ArtifactIdentity `json:"zip"`
	Checksums          ArtifactIdentity `json:"in_package_checksums"`
	Manifest           ArtifactIdentity `json:"package_manifest"`
	SBOM               ArtifactIdentity `json:"sbom"`
	Notices            ArtifactIdentity `json:"third_party_notices"`
	ExtractedZIPSmoke  bool             `json:"extracted_zip_smoke"`
	TimestampUTC       string           `json:"timestamp_utc"`
}

type Module struct {
	Path    string
	Version string
	Sum     string
	Dir     string
	Replace *Module
	Main    bool
}

type MetadataOptions struct {
	Bundle               string
	Repository           string
	DependenciesJSON     string
	Commit               string
	Dirty                bool
	GoVersion            string
	GOROOT               string
	SourceDateEpoch      int64
	InputChecksums       string
	InputFileCount       int
	HeadTree             string
	SourceContentSHA256  string
	SourceFileCount      int
	MovedSmokeComplete   bool
	HelpSmokeComplete    bool
	VersionSmokeComplete bool
}

func deterministicTime(epoch int64) time.Time {
	return time.Unix(epoch, 0).UTC().Truncate(time.Second)
}
