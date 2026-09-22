# Native macOS arm64 release package

## Scope and trust

Release 0.8 defines a self-contained macOS arm64 command-line bundle. It is
unsigned, unnotarized, and requires manual approval. The bundle and detached
checksum are published on the public GitHub release. Signing/notarization and
other architectures remain deferred.

The package source is native-only. The pre-cutover implementation is historical
at `ead3087fc385c707c501006c370e076bab39a153` and is not shipped.

## Acquisition and offline assembly

Sidecar acquisition is a separate, explicitly networked maintainer operation:

```sh
./scripts/acquire-macos-arm64-sidecars.sh /path/to/new-sidecar-input
```

It verifies pinned Chrome/Homebrew inputs, bounded archive structure, relocated
qpdf closure, licenses, component identities, and complete `SHA256SUMS`.
Acquisition is not part of application runtime or final assembly.

Final assembly consumes only an existing complete local input:

```sh
GOPROXY=off GOSUMDB=off ./scripts/package-macos-arm64.sh \
  --source-root /path/to/verified-sidecar-input \
  --source-checksums /path/to/verified-sidecar-input/SHA256SUMS \
  --chrome-source /path/to/verified-sidecar-input/sidecars/chrome-headless-shell-mac-arm64 \
  --qpdf-source /path/to/verified-sidecar-input/sidecars/qpdf-mac-arm64 \
  /path/to/output/aos-cx-docs-dldr-0.8-macos-arm64-release
```

The destination must not exist. Assembly uses:

```text
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64
-trimpath -buildvcs=false -ldflags=-buildid=
GOPROXY=off GOSUMDB=off
GOENV=off GOFLAGS= GOEXPERIMENT= GOTOOLCHAIN=local GOWORK=off
```

It builds the application and temporary release helper from current source,
verifies complete sidecar input, stages both sidecars beneath the executable,
runs moved-layout conversion smoke, generates metadata, creates and verifies
the ZIP, extracts it freshly, reruns help/version/conversion smoke, compares
bundle bytes, writes detached provenance/checksum, and publishes the release set
with one same-filesystem rename.

The assembly path does not download, invoke Homebrew, use network tools, search
`PATH` for sidecars, or reuse `build/aos-cx-docs-dldr`.

## Release-set contents

```text
aos-cx-docs-dldr-0.8-macos-arm64-release/
  aos-cx-docs-dldr-0.8-macos-arm64/
    aos-cx-docs-dldr
    LICENSE
    sidecars/
      chrome-headless-shell-mac-arm64/
      qpdf-mac-arm64/
    PACKAGE-MANIFEST.json
    SHA256SUMS
    SBOM.spdx.json
    THIRD_PARTY_NOTICES
  aos-cx-docs-dldr-0.8-macos-arm64.zip
  aos-cx-docs-dldr-0.8-macos-arm64.zip.sha256
  aos-cx-docs-dldr-0.8-macos-arm64.zip.provenance.json
```

Package manifest and detached provenance schemas remain 1. The application
library schema (1) and raw-cache schema (2) remain unchanged; publication uses
the schema-1 journal, and schema-2 upgrade journals are recognised only to be
rejected read-only.

The SPDX 2.3 SBOM contains the linked production Go closure, Go
toolchain/standard library, modified local JSON5 module, Chrome, qpdf,
jpeg-turbo, and OpenSSL. Application license is `Apache-2.0`; third-party
license classification remains separate. MuPDF is
acceptance-only and absent from package runtime and SBOM.

## Source and checksum identity

The manifest records commit, HEAD tree, dirty state, and a content hash over
every tracked or untracked nonignored source file before and after assembly.
The file lists and source identity must remain identical across the build.

Dirty-source pre-commit validation must report `source.dirty=true`. After review
and authorized commit, release acceptance requires two independent assemblies
with `source.dirty=false`.

Checksum semantics:

- `PACKAGE-MANIFEST.json` inventories immutable payload except itself and
  `SHA256SUMS`;
- `SHA256SUMS` hashes every package file except itself;
- detached `.zip.sha256` hashes the final ZIP;
- detached provenance hashes the ZIP and in-package checksum, manifest, SBOM,
  and notices and records extracted smoke.

Unsafe paths, links, special files, omissions, extras, duplicates, changed
bytes, wrong modes, wrong architecture, wrong sidecar identity, or incomplete
license evidence fail assembly.

Given identical source bytes/identity, toolchain, epoch, and sidecar input,
both independent release sets must be byte-identical. Generated documentation
PDFs are not package inputs and are not promised deterministic.

## Verify and install

In Terminal, start in the directory containing the ZIP and detached checksum.
Verify the ZIP, extract it, then verify every bundled file:

```sh
shasum -a 256 -c aos-cx-docs-dldr-0.8-macos-arm64.zip.sha256
unzip aos-cx-docs-dldr-0.8-macos-arm64.zip
cd aos-cx-docs-dldr-0.8-macos-arm64
shasum -a 256 -c SHA256SUMS
```

Before launching anything, if both checksum checks passed and you trust the
release, clear quarantine recursively from this exact extracted bundle:

```sh
xattr -dr com.apple.quarantine "$PWD"
./aos-cx-docs-dldr --version
./aos-cx-docs-dldr --help
```

Expected version output is `aos-cx-docs-dldr 0.8`. Re-download the replacement
v0.8 assets if you obtained a package before the product rename or the CLI
flag update. Documentation selection now uses `--release`; `--version` prints
the application version and `--app-version` is no longer accepted.

Install the complete directory, for example:

```text
$HOME/Applications/aos-cx-docs-dldr/0.8/
```

Do not move only the executable. Browser quarantine can apply to individual
sidecar executables and libraries, including Chrome/Vulkan and qpdf, so approving
only the main program may produce further dialogs suggesting moving components
to Trash. The recursive command above removes only `com.apple.quarantine` from
the whole verified bundle and avoids separate quarantine approvals. It neither
signs/notarizes the software nor creates a permanent Gatekeeper exception.

For a bundle already moved to the example installation directory, use:

```sh
xattr -dr com.apple.quarantine "$HOME/Applications/aos-cx-docs-dldr/0.8"
```

No `sudo` is normally needed for a user-owned bundle. Apply the command only to
the exact verified bundle, not a parent directory containing unrelated files.
Repeat after verifying a newly downloaded replacement. Individual approval in
**System Settings > Privacy & Security > Open Anyway** is an alternative, but
may be needed for multiple components. Never disable Gatekeeper globally. If a
block persists or explicitly reports malware, investigate the exact message;
removing quarantine does not repair invalid signatures, damaged files, or
malware detections.

## Update, rollback, and uninstall

Install updates into new versioned application directories. Keep the previous
bundle until the new binary and data are accepted.

Release 0.8 reads and writes one library version only. A library recording any
other application version is rejected and left untouched; choose a new
destination and download again. There is no upgrade path and no in-place
downgrade.

Libraries and caches this application did not create remain read-only and
require a new destination and cache directory.

Uninstall removes only the exact versioned application directory. User
libraries, raw caches, ZIP exports, snapshots, retained older bundles, and other
data are never removed automatically.
