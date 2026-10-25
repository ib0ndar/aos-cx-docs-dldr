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
| `internal/source` | Portal catalogue and route-adapter check, mapping resolution, Flare/HPE/Oxygen/static planners, source PDF discovery. |
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

Every hop, including redirects and robots.txt fetches, must pass the
destination policy in `internal/fetch/destination.go`: HTTP or HTTPS to a DNS
name or a public IP literal. `localhost`, non-public IP literals (loopback,
private, link-local, CGNAT, documentation, multicast, reserved, and mapped or
embedded forms of them) and non-dotted-quad numeric hosts are refused with a
final, non-retried error naming the URL. For HTTPS, host names are not resolved
in advance: certificate verification stops a name that resolves to an internal
host, and split DNS keeps working. A direct plain-HTTP connection has no such
protection, so its dialer refuses any non-public address the name resolves to,
which also covers DNS rebinding; through a configured proxy, the proxy resolves
the name. The hidden `--portal-url` override, when it names a loopback URL,
enables fixture mode, which additionally admits loopback only. Responses over the
size limit are never retried. The raw cache does not persist `Set-Cookie` or
other credential headers.

Dispatch pacing occurs after compatible lease admission. Up to four deep
transport clones isolate mutable HTTP/2 setup while retaining bounded reuse.
The cache adds no retry loop.

Catalogue mappings and planned topics default to four workers. Same canonical
URL callers share one cache transaction but receive independently verified file
handles. A canceled zero-waiter flight remains the only writer until retirement.
DOM parsing, supplementary discovery, assets, rewriting, and emission remain
serial and ordered.

## Source planning

The catalogue comes from the Product Documentation portal, a JavaScript
application that is read, never run. Its page names the product and loads
`js/shell.js`; the portal root beside the shell holds the registry
`models/products.json` and the product configuration, whose selectors give the
platforms and releases and whose `catalog` resources give the guides. Guide
mappings are `json/aoscx/<key>.json` beside the page. A per-product adapter
module turns a mapping into a guide URL; the catalogue refresh checks that the
adapter still contains the routing statements `resolve` reproduces, takes the
documentation root from it, and stops with an update-required error if they
changed. Releases from 10.17 route to Oxygen at
`AOS-CX/<release>.xxxx/HTML/<folder>/index.html`, releases after 10.12 to Flare
(the `cli` guide to its PDF), and older releases to archived PDFs.

`Topic.URL` retains public identity; `Topic.FetchURL` identifies the body
endpoint. `TocEntry` retains hierarchy, duplicates, and fragments while topic
downloads are deduplicated.

Flare planning uses one explicit linked TOC or the narrowly verified WebHelp2
`Data/HelpSystem.xml` fallback. JSON5 is decoded by the bounded local module and
never executed.

HPE planning preserves exact document, page, query, final URL, and optional
identity-header agreement. `content.json` is the complete TOC; only genuine
same-document `main.ditasrc` content accepts HPE `multiPage`.

Oxygen WebHelp publications, used from release 10.17, open on a tile index
whose first tile is the planner's seed topic, as it is the portal's. A topic
page carries only the top level of the TOC; every deeper level is a data file,
`oxygen-webhelp/app/nav-links/json/<id>.js`, that assigns
`oxy_webhelp_navlinks`. The planner parses those files with the bounded JSON5
module and never executes them. A topic's children are inline in `topics` or
in the file named by its `next` value; `menu.hasChildren` describes the top
menu, not the TOC, and is ignored. Topics outside the TOC, such as a legal
disclaimer, are found by following `rel="prev"` from the first TOC page and
`rel="next"` from the last, within the publication and up to a fixed limit.
The nav-links files are recorded as inventory chunks. Other static sources
still require an explicit complete TOC; a lazy one is refused.

## Archive and publication

HTML archiving is two-pass, one topic DOM at a time, with a shared source-byte
budget. Publisher HTML, table structure, code whitespace, raster bytes, fonts,
and SVG semantics are preserved. CSS uses the pinned parser. Generated
navigation is compact and shared; complete TOC content is stored once.

Publisher content is sanitized against an allowlist: active and loading
elements such as `script`, `meta`, `base`, `link`, `template`, frames and form
controls are removed, URL attributes survive only where they are rewritten to
verified targets, SVG is rebuilt from its XML tokens with real namespaces, and
unrecognized assets are stored as `.bin`. Every generated page carries a
Content Security Policy with `base-uri`, `form-action` and `object-src` set to
`'none'`; only index pages run their own scripts.

Inside the `src` descriptor of an `@font-face` rule only (stylesheets,
`<style>` blocks, SVG `<style>`), a source whose host answers HTTP 403, 404 or
410 after redirects and retries, or refuses its robots.txt with 403 or 410 so
the font is never requested, is dropped. A rule left with no source is removed;
text falls back to the next family in each `font-family` stack, as it does
online. Each rule is recorded in `stylesheet_recoveries`
(`unavailable-font-face-sources`) with a warning, and the guide stays complete.
Timeouts, 5xx, connection errors, robots disallow rules, the destination
policy, size limits, invalid bodies, non-font resources and fonts used outside
`@font-face src` still make the guide incomplete. Font recoveries are not
missing resources. A TOC entry whose fragment the publisher's page does not
define keeps its link to the page and is reported as a warning.

Active references are validated according to element/attribute semantics.
Cross-guide links localize only to a unique accepted topic identity. HTML search
covers titles and archived text; PDFs remain title-only.

Publication owns lock, staging, incomplete work, snapshots, history, and
rollback. It rechecks lock inode/owner and journal inode/content before shared
mutation. This protects cooperating application operations, not arbitrary
external filesystem changes. Guide outputs and ZIP targets are handed out as
directory handles opened inside the destination root, never as absolute paths
to reopen, and deletion and copying walk one component at a time without
following symlinks. Pre-existing managed directories owned by another account,
or writable by group or others on a filesystem that keeps real mode bits, are
refused.

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

Chrome is driven over `--remote-debugging-pipe` through a modified local
replacement of chromedp v0.16.0 (`third_party/chromedp`), so no DevTools TCP
port exists. Both sidecars get an allowlisted environment, Chrome's whole
directory and qpdf's whole closure are pinned file by file, and both are
re-verified immediately before every process start. SIGINT, SIGTERM and SIGHUP
all cancel a run and exit with 130.

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
