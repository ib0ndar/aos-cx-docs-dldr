# Chrome for Testing PDF sidecar

The native macOS arm64 application can optionally create a PDF companion for a
complete archived HTML guide. Rendering uses exactly one separately shipped
Chrome for Testing headless-shell build controlled through CDP. Retrieval stays
browser-free. The application never downloads Chrome, searches `PATH`, attaches
to a personal browser, or falls back to another renderer.

## Supported pin

| Item | Value |
| --- | --- |
| Product | Chrome for Testing headless shell |
| Version | `153.0.8010.36` |
| Revision | `1681091` |
| Platform | `mac-arm64` |
| Official archive | `https://storage.googleapis.com/chrome-for-testing-public/153.0.8010.36/mac-arm64/chrome-headless-shell-mac-arm64.zip` |
| Project-recorded archive SHA-256 | `3b133378fe44a5f9c849df9049763577fbed296ee4d02d1ffb31b9fcabf79850` |
| Project-recorded executable SHA-256 | `ad3cf5ce958e43a6e3457d6f4d9df0a0c1483fc24ffb36a30530e88d726a82f8` |

Google's official Chrome for Testing metadata supplies the HTTPS download
provenance but does not publish a vendor checksum for this archive. The hashes
above are project-recorded integrity pins, not vendor checksum attestations.
The executable must be a real, executable arm64 Mach-O file with the exact
recorded hash. Symlinks and arbitrary Chrome installations are rejected.
The application verifies this identity before transient assembly and again
immediately before process launch.

## Acquisition, packaging, and installation

For an ordinary command-line bundle, place the complete extracted directory at:

```text
<directory containing aos-cx-docs-dldr>/
  sidecars/
    chrome-headless-shell-mac-arm64/
      chrome-headless-shell
      ABOUT
      LICENSE.headless_shell
      ...all other files from the official archive...
```

For an application bundle, the alternative discovery location is:

```text
<bundle>/Contents/Resources/chrome-headless-shell-mac-arm64/
```

Maintainers acquire both sidecars into a new checksum-sealed local input root
with the explicitly networked command:

```sh
./scripts/acquire-macos-arm64-sidecars.sh /path/to/new-sidecar-input
```

Acquisition validates each hash-pinned archive member path before extraction
into a fresh owned temporary directory. It depends on the recorded
Chrome/Homebrew archive layouts; an unsafe member, missing expected path, or
changed input hash fails explicitly.

Final assembly is a distinct offline operation. It accepts explicit local
Chrome and qpdf directories only after their complete source root matches the
specified checksum file:

```sh
GOPROXY=off GOSUMDB=off ./scripts/package-macos-arm64.sh \
  --source-root /path/to/verified-sidecar-input \
  --source-checksums /path/to/verified-sidecar-input/SHA256SUMS \
  --chrome-source /path/to/verified-sidecar-input/sidecars/chrome-headless-shell-mac-arm64 \
  --qpdf-source /path/to/verified-sidecar-input/sidecars/qpdf-mac-arm64 \
  /path/to/output/aos-cx-docs-dldr-0.8-macos-arm64-release
```

It builds `aos-cx-docs-dldr` from current source, stages Chrome and qpdf under the
required `<bundle>/sidecars/` directory, moves the bundle, and completes real
generated-PDF fixtures before archiving and after fresh extraction through
automatic executable-relative discovery. It emits the schema-1 manifest, SPDX
SBOM, notices, complete checksums, ZIP and detached provenance described in
[the release guide](native-macos-arm64-release.md).

The low-level `stage-chrome-sidecar.sh` accepts only an already verified local
Chrome directory and an explicit release-helper path supplied by the assembler.
It has no acquisition behavior. Do not commit archives, executables, generated
PDFs, profiles, caches, or renderer logs. The ZIP is deliberately unsigned;
signing/notarization is deferred.

Development and controlled testing may select the exact executable with the
hidden `--chrome-path PATH` flag or `AOSCX_DOCS_CHROME_PATH`. The flag takes
precedence. These overrides do not weaken architecture or hash verification.

## Runtime contract

`--convert-html-to-pdf` is explicit opt-in. Source-verified publisher PDFs
remain preferred, and a selected publisher PDF bypasses generation. Generated
PDFs are companion files inside HTML guide directories; canonical browsable
HTML, full-text search, and source-change history remain HTML-based.

Each render uses a private profile/cache/temp root, disables script execution,
intercepts requests before navigation, and allows only verified regular files
inside the guide root plus inline `data:` resources. Network, foreign paths,
symlinks, special files, and unresolved local dependencies fail closed. The
default limits are one renderer at a time, 15 minutes, 14 GiB process-group RSS,
2 GiB output, 20,000 pages, and 1 MiB of captured Chrome diagnostics.

The transient combined print HTML and raw Chrome partial are removed after the
attempt. Chrome's inferred outline is disabled. The separately shipped pinned
qpdf sidecar described in [qpdf-sidecar.md](qpdf-sidecar.md) writes an explicit
authoritative outline to a second private partial. That result is fsynced,
structurally and semantically validated, hashed, and atomically published
before links and manifests are updated. A conversion failure leaves
complete HTML browsable and records a failed companion attempt. If a refresh
would replace an already valid complete companion, the prior complete guide is
retained transactionally.

Generated PDF metadata records the transient input/source-manifest hashes,
Chrome and qpdf identities, outline-plan hash/schema/counts, settings, output
hash/size/page count, timing, peak observed RSS, and validation results. PDFs
are not promised to be byte-deterministic, and the project does not claim
PDF/UA compliance. The visible Contents retains links but this Chrome pin does
not render CSS `target-counter()` page numbers; the application does not
fabricate them. Maintainer acceptance uses
`cmd/aoscx-pdf-acceptance` with an explicit local MuPDF 1.28.4 `mutool` and
machine-local closure identity manifest. MuPDF is acceptance-only, is not
shipped or downloaded by `aos-cx-docs-dldr`, and is not part of the application SBOM;
PyMuPDF remains only as historical one-time parity evidence for the retired
acceptance implementation.

The local identity manifest is deliberately machine-specific and belongs in an
owned acceptance evidence directory, not the repository or application bundle:

```sh
go run ./cmd/aoscx-pdf-acceptance \
  --mutool-path /explicit/Cellar/mupdf/1.28.4/bin/mutool \
  --mutool-license-path /explicit/Cellar/mupdf/1.28.4/COPYING \
  --write-mutool-identity-manifest /new/evidence/mutool-identity.json

go run ./cmd/aoscx-pdf-acceptance /path/to/generated.pdf \
  --title "Guide title" \
  --report /new/evidence/report.json \
  --mutool-path /explicit/Cellar/mupdf/1.28.4/bin/mutool \
  --mutool-identity-manifest /new/evidence/mutool-identity.json \
  --qpdf-path /explicit/qpdf-mac-arm64/bin/qpdf
```

The accepted local formula/license metadata identifies MuPDF as
`AGPL-3.0-or-later`; this records local evidence and is not legal advice. The
manifest binds `mutool` version/hash/arm64 identity, `COPYING`, the host build,
and resolved nonsystem Mach-O paths, hashes and architectures. The tool
revalidates that closure before every process and rejects symlinked tool,
license and manifest paths. Its additive `tool` report object records the
manifest and sidecar identities without changing the historical acceptance
keys or exit meanings (`0` pass, `1` policy violation, `2` usage/tool failure).

The approved local acceptance pin is:

| Item | Value |
| --- | --- |
| Homebrew MuPDF version | `1.28.4` |
| Homebrew bottle (`arm64_tahoe`) SHA-256 | `33be235ebd5a0e3ff626ab3e9c0249f5ef45f71f2f097c97547ffe079b6c66f4` |
| `mutool` SHA-256 | `5a39feb8c4c85b57eea9ea7785c8747337741baec3c367a26f611c7be3767bef` |
| Local `COPYING` SHA-256 | `57c8ff33c9c0cfc3ef00e650a1cc910d7ee479a8bc509f6c9209a7c2a11399d6` |
| Local `libmupdf.dylib` SHA-256 | `92c04f0076ae8232c8e111229b6a19c4b478af0fd4ee00156e66fbc149ce2219` |

## Notices

The complete sidecar directory must retain the official `ABOUT` and
`LICENSE.headless_shell` files from the archive. Chrome is made possible by the
Chromium project and other open-source software; the archive's license file
contains the applicable notices.

The Go control layer uses `github.com/chromedp/chromedp` and
`github.com/chromedp/cdproto`, both under BSD-3-Clause licenses. Their source
and license texts are available through the pinned Go module versions recorded
in `go.mod` and `go.sum`; release dependency inventory must include them.
