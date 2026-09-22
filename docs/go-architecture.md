# aos-cx-docs-dldr 0.8 architecture

## System boundary

The executable and CLI command are `aos-cx-docs-dldr` on both deliverables,
defined by `model.ExecutableName`; the Go entry point is `cmd/aos-cx-docs-dldr`.
The application/module identity is `aos-cx-docs-dldr`. Old-name state is foreign
and rejected read-only; the rename does not migrate libraries or caches.

`aos-cx-docs-dldr` 0.8 is a native Go CLI with two deliverables, both complete
builds with generated-PDF conversion: a native macOS arm64 bundle and a
linux/amd64 container (see [the container guide](container.md)). It refreshes
the public
AOS-CX Product Documentation catalogue, resolves exact source mappings, archives
supported guides, publishes a transactional offline library, and optionally
exports a verified ZIP or generated-PDF companion.

The pre-cutover source is historical at
`ead3087fc385c707c501006c370e076bab39a153`; it is not an active dependency or
parallel implementation. Existing foreign user data remains independently
browsable and read-only.

## Data flow

```text
CLI
  -> fresh catalogue and exact mapping resolution
  -> mapped-source and optional-PDF availability probes
  -> immutable platform/release/guide selection
  -> native library preflight
  -> marked raw-v2 cache
  -> source-specific complete plan
  -> bounded topic prefetch
  -> serial two-pass archive and validation
  -> optional publisher PDF or generated companion
  -> transactional guide acceptance/publication
  -> version search/history/index and optional ZIP
```

The live catalogue and source mapping are never hard-coded by platform count or
release cutoff. “Flare” and “HPE multipage” describe source formats, not release
age.

## Packages

| Package | Role |
| --- | --- |
| `internal/model` | Stable shared data contracts and `Version = 0.8`. |
| `internal/cli` | Cobra flags, guided terminal flow, progress, availability, and orchestration. |
| `internal/source` | Portal catalogue, mapping resolution, Flare/HPE/static planners, source PDF discovery. |
| `internal/fetch` | Compatible and standard HTTP transports, robots, leases, retries, deadlines, diagnostics. |
| `internal/cache` | Integrity-checked raw-v2 persistence and same-URL flight sharing. |
| `internal/archive` | DOM/CSS/SVG processing, assets, local links, search records, and archive validation. |
| `internal/publication` | Transient print assembly, canonical cover, TOC, footer/outline plans. |
| `internal/pdfgen` | Pinned Chrome rendering, qpdf overlay/outline postprocessing, atomic companion publication. Per-platform sidecar pins for macOS arm64 and Linux amd64. |
| `internal/pdfcheck` | Portable shipped PDF structure validation and generated-output inspection. |
| `internal/pdfacceptance` | Maintainer-only generated-PDF acceptance primitives. Darwin-gated and excluded from the shipped binary. |
| `internal/library` | State preflight, lock/journal ownership, staging, snapshots, history, search, ZIP. |
| `internal/storage` | Rooted filesystem validation, copying, hashing, synchronization, and replacement. |
| `internal/releasepkg` | Offline release assembly metadata, SBOM, notices, archive, provenance, repository audit. |

## Retrieval

The default `compatible` profile uses pinned req 3.61.0/uTLS 1.8.2 Chrome120
TLS behavior, header order, and HTTP/2 settings with identity
`aos-cx-docs-dldr/0.8`. `--transport http` selects standard native HTTP. Neither
profile falls back to the other or to browser automation.

`model.Transport.Download` owns:

- a 150-second default overall deadline;
- a fresh 45-second attempt deadline capped by remaining overall time;
- two retries after the first attempt;
- complete response consumption and sink replacement on retry;
- canonical request/final URL, elapsed, attempt, retry, timeout-scope, stage,
  status, cancellation, and deadline evidence.

Dispatch pacing occurs after compatible lease admission. Up to four deep
transport clones isolate mutable HTTP/2 setup while retaining bounded reuse.
The cache adds no retry loop.

Catalogue mappings and planned topics default to four workers. Same canonical
URL callers share one cache transaction but receive independently verified file
handles. A canceled zero-waiter flight remains the only writer until retirement.
DOM parsing, supplementary discovery, assets, rewriting, and emission remain
serial and ordered.

## Source planning

`Topic.URL` retains public identity; `Topic.FetchURL` identifies the body
endpoint. `TocEntry` retains hierarchy, duplicates, and fragments while topic
downloads are deduplicated.

Flare planning uses one explicit linked TOC or the narrowly verified WebHelp2
`Data/HelpSystem.xml` fallback. JSON5 is decoded by the bounded local module and
never executed.

HPE planning preserves exact document, page, query, final URL, and optional
identity-header agreement. `content.json` is the complete TOC; only genuine
same-document `main.ditasrc` content accepts HPE `multiPage`.

Static/Oxygen requires an explicit complete supported plan and substantive
content. It is fixture-verified only, with zero current mappings in the
2026-09-16 production-resolution snapshot.

## Archive and publication

HTML archiving is two-pass, one topic DOM at a time, with a shared source-byte
budget. Publisher HTML, table structure, code whitespace, raster bytes, fonts,
and SVG semantics are preserved. CSS uses the pinned parser. Generated
navigation is compact and shared; complete TOC content is stored once.

Active references are validated according to element/attribute semantics.
Cross-guide links localize only to a unique accepted topic identity. HTML search
covers titles and archived text; PDFs remain title-only.

Publication owns lock, staging, incomplete work, snapshots, history, and
rollback. It rechecks lock inode/owner and journal inode/content before shared
mutation. This protects cooperating application operations, not arbitrary
external filesystem changes.

## Version and state policy

The application reads and writes exactly one library version, `model.Version`.
There is no upgrade path and no CLI, environment, or configuration selector for
one.

Preflight validates state before creating a lock. A library recording any other
application version is rejected, and its bytes are left untouched with
new-destination guidance. The same applies to malformed, future, and
unrecognised state, to raw caches without the private marker, and to
publication journals of an unknown schema.

History is a log. Records written by earlier builds keep their original version
string and remain valid; the newest record must match the manifest. Manifests
that already carry `upgraded_from` provenance from an earlier build retain it,
and it is validated but never acted on.

Publication remains transactional through the schema-1 journal, so a failed
update never replaces a prior complete guide. Adjacent ZIPs and raw-v2 caches
are outside library state handling.

## Generated PDFs

Conversion is explicit opt-in. Chrome for Testing headless shell
153.0.8010.36/revision 1681091 renders private local print input with script
execution disabled and fail-closed request interception. qpdf 12.4.1 merges the
footer overlay and installs the authoritative occurrence-aware outline.

The package uses only executable-relative verified sidecars. Runtime never
downloads, searches `PATH`, uses Homebrew, attaches to a personal browser, or
falls back. Source-verified publisher PDFs bypass conversion. Complete HTML
remains canonical, generated bytes are not promised deterministic, and no
PDF/UA claim is made.

MuPDF is used only by `cmd/aoscx-pdf-acceptance` with explicit identity input.
It is absent from the runtime package and SBOM.

## Release architecture

Two deliverables exist: the unsigned macOS arm64 package and the linux/amd64
container image. The macOS package is the signed-checksum release artifact; the
container carries the same pinned sidecars and is verified at image build. The
offline macOS assembler builds
the application and build-only release helper from current source with
`CGO_ENABLED=0`, `GOOS=darwin`, `GOARCH=arm64`, `GOPROXY=off`,
`GOSUMDB=off`, `GOTOOLCHAIN=local`, and no workspace or user Go flags.

The bundle and ZIP root are `aos-cx-docs-dldr-0.8-macos-arm64`. Schema-1 package
manifest and provenance, SPDX 2.3 SBOM, complete checksums, retained notices,
moved-layout conversion smoke, and fresh-extraction conversion smoke are
required. Application license is `Apache-2.0`, with `LICENSE` shipped in both
deliverables; third-party components retain their own licenses.
Signing/notarization and other architectures remain deferred.
