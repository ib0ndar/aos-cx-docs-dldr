# qpdf generated-PDF outline sidecar

The native macOS arm64 application uses qpdf only after pinned Chrome for
Testing has rendered an HTML-derived PDF. qpdf replaces Chrome's inferred
heading outline with the exact occurrence-aware hierarchy derived from the
authoritative `DocumentPlan.TOC`. Source-verified publisher PDFs never enter
this pipeline.

## Supported pin

| Item | Value |
| --- | --- |
| Product | qpdf |
| Version | `12.4.1` |
| Platform | macOS arm64 |
| Homebrew qpdf bottle SHA-256 | `7e3e764df933760c100b2bd5d7177ebd0511733685e647e57d9e90afa01427c5` |
| Homebrew jpeg-turbo 3.2.0 bottle SHA-256 | `02539b0736cfacdc6c4bb6a7d274d0c5c8b6e1faf9b5bab1e155168961d288aa` |
| Homebrew OpenSSL 3.6.4 bottle SHA-256 | `8c7dab98311d025fda379dba80eaf985af1ceca26c403c66318b3c7d43a028b9` |
| `bin/qpdf` SHA-256 | `0326859206213694229c4b0917cc0eedf11d023dc5b1aa517eeab7ba3e94aec4` |
| relocated `lib/libqpdf.30.dylib` SHA-256 | `56ee3ac586187e129aaaacaa4e458cc8bedc98811bd961f73196d03be442fd8a` |
| relocated `lib/libjpeg.8.dylib` SHA-256 | `cbdd66791e24c27c483587aed048a0c6270ff7ce9f6c8a83a010e90dd63af498` |
| relocated `lib/libcrypto.3.dylib` SHA-256 | `f9563883fdbb4f2b367c708a6abd786119c935d3f9cd01fa8e11c34666775d1b` |
| runtime component-manifest SHA-256 | `751e59fcecb6d3cd2fce2d5379a477c799065dbf4343b07f049a1ed48f93a15c` |

The bottle and component hashes are project-recorded integrity pins. Homebrew
and GHCR provide package provenance, not vendor checksum attestation for the
relocated result. The explicit acquisition script rewrites qpdf's jpeg/OpenSSL
imports to `@loader_path`, gives those copied libraries stable `@loader_path`
IDs, and ad-hoc signs the three changed dylibs. The resulting closure has no
`/opt/homebrew`, `/usr/local`, or unresolved bottle-prefix dependency. The
final ZIP is deliberately unsigned; product signing/notarization is deferred.

## Acquisition, packaging, and installation

Maintainers acquire and relocate both pinned sidecars with the explicitly
networked command:

```sh
./scripts/acquire-macos-arm64-sidecars.sh /path/to/new-sidecar-input
```

Acquisition validates each hash-pinned archive member path before extraction
into a fresh owned temporary directory through the build-only Go release
helper. It depends on the recorded Chrome/Homebrew archive layouts; an unsafe
or colliding member, unsupported type/metadata, missing expected path, or
changed input hash fails explicitly. GHCR authentication is parsed and written
by the same helper to an exclusive mode-0600 curl header file; the acquisition
shell has no Python dependency and never carries the token in argv or the
environment.

Homebrew bottle extraction validates the complete archive and accounts every
regular member, but materializes only exact explicitly mapped regular inputs
needed by relocation. Unselected symlink/hardlink records are validated as
inert metadata and are never resolved, followed or written. Read-only replay of
the exact cached OpenSSL 3.6.4 bottle selected only `libcrypto.3.dylib` and
`LICENSE.txt` successfully. qpdf and jpeg-turbo replay remain pending because
their exact source bottles were not retained locally.

Final assembly is offline and consumes only explicit directories covered by
that input root's complete `SHA256SUMS`:

```sh
GOPROXY=off GOSUMDB=off ./scripts/package-macos-arm64.sh \
  --source-root /path/to/verified-sidecar-input \
  --source-checksums /path/to/verified-sidecar-input/SHA256SUMS \
  --chrome-source /path/to/verified-sidecar-input/sidecars/chrome-headless-shell-mac-arm64 \
  --qpdf-source /path/to/verified-sidecar-input/sidecars/qpdf-mac-arm64 \
  /path/to/output/aos-cx-docs-dldr-0.8-macos-arm64-release
```

The low-level `stage-qpdf-sidecar.sh` accepts only an already verified local
qpdf directory and an explicit release-helper path supplied by the assembler;
it never downloads, invokes Homebrew, or searches `PATH` for qpdf. Acquisition
checks every bottle and input component, writes the relocatable closure, and
retains qpdf, jpeg-turbo and OpenSSL notices. Final assembly verifies the
complete source checksum set and every final runtime component before
packaging. See
[the release guide](native-macos-arm64-release.md) for the schema-1
manifest, SPDX, checksum, ZIP and Gatekeeper contracts.

The command-line bundle layout is:

```text
<directory containing aos-cx-docs-dldr>/
  sidecars/
    qpdf-mac-arm64/
      bin/qpdf
      lib/libqpdf.30.dylib
      lib/libjpeg.8.dylib
      lib/libcrypto.3.dylib
      licenses/...
```

An application bundle may instead place `qpdf-mac-arm64` under
`Contents/Resources`. Controlled development can select the exact executable
with hidden `--qpdf-path PATH` or `AOSCX_DOCS_QPDF_PATH`; the flag takes
precedence. Every component must be a real nonsymlink arm64 Mach-O file with
the expected hash and exact imported-library closure. The application never
downloads qpdf, searches `PATH`, invokes Homebrew, or falls back to a system
installation.

## Runtime contract

Chrome writes a bounded private raw PDF with its inferred outline disabled.
qpdf receives that PDF plus a private bounded JSON update and streams a second
bounded private PDF. The same generation deadline covers both sidecars; qpdf
has a 2 GiB RSS limit and 1 MiB diagnostic cap. Cancellation kills its dedicated
process group. Both PDF partials and JSON/temp data are removed after success or
failure.

The update changes the catalog outline reference and adds the required outline
objects. The application then reads qpdf JSON and traverses the actual
renumbered object graph. It verifies `/Parent`, `/First`, `/Last`, `/Prev`,
`/Next`, positive descendant `/Count`, cycle/count/depth bounds, exact Unicode
titles, named local destinations, page targets, and exact URI-only external
actions. JavaScript, launch, file and mixed actions fail closed.

The accepted outline has top-level `Cover`, `Contents`, followed directly by
normalized authoritative roots. Only a validated leading short-title, Home,
long-title, or source Contents navigation chain is omitted from the sidebar;
its substantive children are promoted and ambiguous roots fail closed.
A Flare trailing root `Title` navigation leaf is omitted only when it is the
exact planned same-origin `tit.htm` title-page topic in that validated source
shape. Its page remains visible with the guide-title footer fallback; duplicate
or ambiguous candidates fail closed.
Authoritative categories and topics retain order, depth, repeated occurrences,
duplicate titles and exact fragment destinations. URL-less categories target
their first descendant. Supplementary archived topics absent from the source
TOC appear under `Additional archived topics` only when needed. Publisher
content headings and generated provenance remain visible/tagged page content
but are not sidebar entries.

Generated-PDF settings schema 5 records qpdf identity, footer-plan schema 2,
outline-plan schema 2, both plan hashes, entry/depth/action counters,
footer-overlay and outline durations/RSS, exact destination preservation, and
successful exact-tree validation. qpdf first merges the pinned-Chrome footer
overlay, then installs the outline in a separate private partial. A qpdf
failure never publishes either private partial, never downgrades canonical
complete HTML, and cannot replace a prior verified companion.

## Notices

The staged bundle retains:

- qpdf `LICENSE.txt` and `NOTICE.md`;
- jpeg-turbo `LICENSE.md`;
- OpenSSL `LICENSE.txt`.

Release dependency inventory must include all three components and the
separately documented Chrome/chromedp components.
