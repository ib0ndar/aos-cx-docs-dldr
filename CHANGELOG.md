# Release notes

## 0.8 replacement release - 2026-09-21

- Rename the product, executable, Go module, Windows wrapper, image, bundle and
  archive names to `aos-cx-docs-dldr`. The repository uses the same name.
  Library/cache identities also change: old-name state is rejected read-only
  and requires new destinations. The owner authorized replacement of all v0.8
  assets and its tag; use the new package checksums and detached provenance.
  Re-download v0.8 if you obtained it before the rename. No automatic migration
  of old-name libraries or caches occurs.

- License original application code, scripts, and project documentation under
  Apache-2.0. Include `LICENSE` in new macOS bundles and container images, and
  declare Apache-2.0 in the application SBOM entry and OCI license label.
  Third-party software retains its own licenses and notices. Retained `e1764e3`
  artifacts predate this change and require fresh clean-commit assembly.

## 0.8 - 2026-09-19

This release establishes exactly two deliverables, both complete builds with
generated-PDF conversion: the native macOS arm64 bundle and a linux/amd64
container image for Windows users. It changes no macOS retrieval or
publication behavior.

### Deliverables

- Add a linux/amd64 container image as the second deliverable and the way
  Windows users run the application. It is a complete build: the application
  plus pinned Chrome for Testing headless shell 153.0.8010.36 / r1681091
  (linux64) and qpdf 12.4.1 (linux-x86_64), the same versions as macOS. The
  `Dockerfile` downloads both archives by pinned SHA-256, stages them beside
  the executable, and runs the application's own verifier against the result
  before the image is finished. Base images are pinned by digest.
- Make generated-PDF conversion platform-aware. `internal/pdfgen` now carries a
  per-platform sidecar table: Mach-O arm64 pins for macOS and ELF x86-64 pins
  for Linux, each with its exact executable SHA-256 and dynamic-library
  closure. Every component is re-verified by format, architecture, hash and
  closure before every launch, exactly as before; only the expected values
  depend on the running platform.
- Run Chrome with `--no-sandbox` on Linux only. Container runtimes deny the
  user namespaces Chrome's layer-one sandbox needs and the headless shell
  refuses to start without them. Isolation in the container rests on the
  container boundary, the unprivileged user, and the renderer controls the
  application already enforces: scripts disabled, all network routed to a dead
  proxy, foreign file access refused at the CDP layer, private throwaway
  profile. macOS keeps Chrome's own sandbox.
- Record the producing platform's sidecar pins in each complete generated-PDF
  record and validate them against the running platform. Together with the
  existing Unix-permission-bit digests this means a library is validated only
  by the platform that produced it; libraries are not portable between macOS
  and the container.
- The container is linux/amd64 only. Upstream qpdf 12.4.1 publishes no arm64
  binary, so an arm64 image cannot carry the exact pin and is not built.
- Add `scripts/build-bundle.sh` so every local macOS build is a complete bundle.
  It verifies the sidecar source against its checksums, builds the application,
  stages both sidecars, re-verifies them in place, and finishes with a real
  pinned-Chrome conversion, so a successful build means conversion works. A
  bare `go build` still produces only the executable, which cannot convert.
- Add `scripts/package-container.sh`, which builds the image, asserts its
  architecture and version, and writes a loadable gzipped archive with
  `SHA256SUMS`. Add `cmd/aoscx-verify-sidecars`, build tooling that resolves
  both sidecars through the pinned verifier and prints their identities.
- Add `scripts/aoscx-docs.ps1`, a PowerShell wrapper that hides the container
  invocation, pins `--destination /library` (omitting it for `--list`, which
  the CLI rejects alongside a destination), allocates a TTY only when output is
  not redirected, and reports the Windows folder afterwards. The library is
  bind-mounted to a Windows folder; the raw cache stays on a named volume where
  its `0700`/`0600` gate holds.
- Drop the experimental native Windows executable. It was only ever a compile
  gate with no runtime evidence, and the container supersedes it. No Windows,
  Linux native, arm64 container, or macOS Intel build exists.

### State

- Remove the library upgrade engine. Nothing was ever distributed and the sole
  existing 0.7 library was migrated before removal, so no upgrade path is
  needed. This application reads and writes exactly one library version; a
  library recording any other version is rejected read-only with
  new-destination guidance. Deleted the migration driver, its schema-2 upgrade
  journal, snapshot staging and recovery, and the mode-exact tree copy in
  `internal/storage` that only migration used. Removing that copy removes the
  last code path that compared Unix permission bits.
- Keep history as a log. Records written by earlier builds retain their original
  application version and remain valid; only the newest record must match the
  manifest. Manifests carrying `upgraded_from` provenance from an earlier build
  keep it, and it is still validated but never acted on.
- Accept the preflight audit on a failed generated-PDF record. A failed record
  must not claim output, but the audit that ran before the failure is exactly
  the evidence needed to diagnose it.

### Housekeeping

- Move maintainer-only MuPDF acceptance analysis out of `internal/pdfcheck`
  into the darwin-gated `internal/pdfacceptance`. `internal/pdfcheck` now holds
  only the portable PDF structure checks it is named for, and the shipped
  binary no longer links macOS-specific acceptance code it never used.
- Derive the release package root name and the package script's version
  assertions from `model.Version` instead of hardcoding them, so a version bump
  cannot leave a stale package identity behind.
- Rewrite the README for first-time users and remove wording that only made
  sense as a contrast with the retired implementation, from the documentation
  and from two user-facing strings in the application itself.
- Remove the retired implementation's leftover virtual-environment and
  test/lint cache directories from the working tree.

## 0.7 - 2026-09-17

This is the first production-native `aoscx-docs` release. The 0.6 line was an
internal checkpoint and had no production tag.

- Replace the active application with the native Go CLI and remove the retired
  implementation, tests, and packaging configuration from the current tree.
  The pre-cutover source remains available in Git history at
  `ead3087fc385c707c501006c370e076bab39a153`; old installations and user data
  are never automatically removed or migrated.
- Activate the native 0.5/0.6-to-0.7 migration engine. Migration validates and
  preserves a pre-upgrade snapshot, uses the schema-2 upgrade journal, records
  sticky `upgraded_from` provenance, and is idempotent for already-0.7 state.
  Foreign, malformed, future, old implementation, raw-v1, and unmarked state
  remains read-only.
- Publish the unsigned self-contained macOS arm64 package as
  `aoscx-docs-0.7-macos-arm64`, retaining schema-1 package
  manifest/provenance, SPDX 2.3 SBOM, complete checksums, third-party notices,
  `NOASSERTION` product licensing, and offline moved/fresh-extraction
  Chrome/qpdf conversion smoke.
- Preserve documented intentional non-parities, source-blocked cases, and
  Static/Oxygen's fixture-only status with zero current mappings in the
  2026-09-16 production-resolution snapshot.

- Replace the generated-PDF acceptance script with the maintainer-only native
  `aoscx-pdf-acceptance` command. It preserves the existing report and
  pass/fail policy while using explicit manifest-bound MuPDF 1.28.4 for
  structured glyph layout/raster samples and the pinned qpdf object graph for
  outline authority. Explicit missing source roots now fail, and clipped
  command prefixes require two distinct normalized rows from historical
  outermost `pre`/`code` or explicit `screen`/`codeblock` evidence.
  MuPDF remains local acceptance tooling and is not shipped or added to the
  application SBOM.
- Move sidecar archive extraction and GHCR token parsing out of Python and into
  the build-only Go release helper. Acquisition now verifies archive identity
  before bounded allowlisted extraction and supplies curl an exclusive private
  header file without exposing the bearer token in argv, environment, stdout,
  shell tracing, or error text. Release payloads and package schemas are
  unchanged. Homebrew bottle mode validates every member but materializes only
  explicitly mapped required regular files; unselected link records remain
  inert and are never followed or written. Exact cached OpenSSL and
  acceptance-only MuPDF bottle replays passed this selective path.

## 0.6 - 2026-09-16

This release advances the native macOS arm64 Go implementation while retaining
the frozen Python reference tree. It does not claim complete coverage of every
publisher guide or cross-platform release support.

- Add the first bounded WP13 release-package foundation without changing the
  application version. Final macOS arm64 assembly is now offline and accepts
  only explicit Chrome/qpdf directories covered by one complete verified
  checksum set; maintainer acquisition is a separate command. The assembler
  always builds current source, runs moved and freshly extracted real
  Chrome/footer/qpdf conversion smokes, and emits a deterministic
  self-contained unsigned internal ZIP with schema-1 package manifest,
  complete checksums, detached checksum/provenance, SPDX 2.3 JSON SBOM, and
  retained third-party notices. Product licensing remains `NOASSERTION`, and
  Developer ID signing/notarization remains deferred unless distribution
  becomes public.
- Record the approved future 0.7 release boundary without implementing it:
  macOS arm64 only, documented intentional non-parities and source-blocked
  cases accepted, Static/Oxygen fixture-only with zero current mappings,
  native 0.5/0.6 state upgraded forward with a pre-upgrade snapshot and
  `upgraded_from` provenance, older binaries rejecting 0.7 state, and Python
  implementation/tests/config removed only at the coordinated cutover.
- Make the guided Chrome conversion question conditional on the expected output
  mix. Direct publisher PDFs and positively verified optional publisher PDFs
  selected by `prefer PDF` bypass it; known HTML and availability-unknown
  fallbacks receive an exact contextual `Generate PDF companions` count.
  Changing preference dynamically adds/removes the step without overriding
  fixed flags.
- Add availability-aware bulk selection. When fresh exact source checks prove
  mapped guides unavailable, guided flows offer `All available mapped guides`
  and `--all` selects the available plus availability-unknown subset while
  persisting skipped guide provenance. Skips do not create failed attempts or
  make a successful subset incomplete; prior verified guides may remain
  visible with `retained_previous` provenance.
- Reset persistent TTY diagnostics and every Bubble Tea prompt to a true fresh
  terminal line. Long wrapped warnings now end with CRLF and prompts begin at
  column zero without adding blank lines; redirected output remains plain LF.
- Split native retrieval timing into a 150-second overall operation deadline
  and a fresh 45-second deadline for each application attempt. The default two
  retries now permit up to three complete attempts within that one overall cap;
  each attempt covers robots, compatible-transport admission, pacing,
  redirects, headers and body consumption. `--attempt-timeout` exposes the
  per-attempt bound, while `--timeout` remains the overall bound. Diagnostics
  now distinguish application attempts/retries and attempt versus overall
  deadline exhaustion.
- Extend the strict Flare same-page ordinary-stylesheet recovery to redundant
  nested `Content/.../Resources/Stylesheets/` declarations. The same topic must
  independently advertise exactly one literal in-`Content/`
  exact-case-same-basename candidate; it must be
  nonredirecting, valid `text/css`, parser-valid and already accepted locally,
  while every other in-guide same-basename declaration returns exact 404/410.
  Typed provenance and one warning are retained per broken URL; valid,
  ambiguous, query-conflicting, cross-page, transient and non-Flare cases still
  fail closed.
- Publish otherwise valid HTML guides as `degraded` when the only exhausted
  dependencies are standalone PNG, JPEG, GIF, WebP, or AVIF image slots.
  Accessible inert placeholders preserve alt/caption context and bounded
  geometry, while manifests retain exact missing-resource provenance. Degraded
  libraries and ZIPs return exit code 2; complete guides outrank degraded
  refreshes, and degraded replacements may only keep or reduce the exact
  missing-URL set. CSS images, SVG, topics, fonts, scripts, unsafe identities,
  cancellation, and resource/integrity limits remain fail-closed.
- Treat parser-recovered `<img>` elements with neither `src` nor `srcset` as
  inert during generated-PDF preflight while retaining strict failure for every
  declared image resource. Failed companions now persist bounded image/source
  identities for diagnosis.
- Refine the acceptance-only adjacent-page analyzer for long publisher command
  output: a clipped visible prefix is source-authored only when at least two
  distinct `screen`/`codeblock` source rows begin with that exact normalized
  prefix. Single source rows, prose prefixes and arbitrary repetition still
  fail.
- End the already identified transient Flare homepage section at a physical
  page boundary so positioned or floated publisher cards cannot overlap the
  following authoritative topic. Archived HTML and card content remain
  unchanged.
- Extend the strict Flare WebHelp2 fallback to an optional detailed-Contents
  shortcut that returns HTTP 404/410 only when the homepage independently
  proves the existing HelpSystem shape and the missing shortcut is absent from
  the complete authoritative TOC. Archive discovery retains that link online
  instead of crawling it as a supplementary topic.
- Recover one narrowly proven same-page duplicate ordinary Flare stylesheet:
  the valid exact-case same-basename declaration must stay inside the selected
  guide's `Content/` tree, return nonredirected parse-valid CSS, and accompany a
  duplicate same-origin URL that returns HTTP 404/410. Typed provenance and one
  warning are retained; ambiguity and transient failures remain incomplete.
- Recover a narrowly verified Flare publisher defect where a table-style link
  crosses into a malformed release/project path and returns HTTP 404/410.
  Recovery requires one exact-case basename match independently advertised
  within the complete selected-guide inventory, exact requested/final
  same-guide identity, valid CSS, and parsed selector coverage for the affected
  `TableStyle-*` family. Manifests retain typed URL/status/hash/size/topic/family
  provenance and the console keeps one actionable warning per replacement;
  every ambiguous or non-permanent failure remains incomplete.
- Separate expected publisher policy/transformation notices from actionable
  warnings and integrity errors. External navigation retained online, omitted
  generated navigation styles, safe CSS cleanup and the validated HPE public
  API policy fact now use typed durable notice records. Human output emits one
  categorized summary per guide while manifests, attempts, history and ZIPs
  retain every detail. This additive schema-1 change remains backward-readable
  when older manifests omit `notices`.
- Add versioned typed HTML archive progress phases for authoritative topic
  validation, post-supplementary page emission, real post-emission hash reads,
  per-HTML-file offline reference checks, indeterminate manifest write/sync,
  and final check completion. Building percentage is emitted pages only;
  dynamic `assets completed/discovered` stays count-only and never implies
  completion. TTY transitions repaint immediately within width-minus-one while
  redirected/`TERM=dumb` output uses sparse quartile milestones.
- Normalize only transient Flare print topic headings to 16pt/14pt/12pt and
  keep headings intact with following content where feasible. Add a
  renderer-owned, pinned-Chrome footer overlay merged by pinned qpdf: cover has
  no footer, Contents starts with visible Arabic physical page `2`, later pages
  show the authoritative parent TOC category at lower left and their Arabic
  physical number at lower right. Normalize the generated PDF sidebar so
  `Cover` and `Contents` are followed directly by authoritative roots: validated
  leading short-title, Home, long-title, and source Contents wrappers are
  omitted from the outline and their children promoted, while ambiguous roots
  fail closed and visible content remains. An exact Flare trailing `Title`
  navigation leaf for a planned same-origin `tit.htm` title page is also
  omitted from the outline without removing its visible page; ambiguous or
  duplicate candidates remain errors. Promoted root topics use their own title
  for the footer, while an omitted source title page uses the guide title.
  Settings schema 5 rejects older companions;
  HPE heading typography and canonical browsable HTML remain unchanged.
- Tolerate Finder's exact `.DS_Store` file at published version, guide,
  `pages`, and `assets` levels only when it is a nonsymlink regular file no
  larger than 1 MiB. It is ignored by shape/hash checks and ZIP, omitted from a
  replacement stage, and retained in the prior snapshot; every other hidden,
  AppleDouble, case-variant, symlink, oversized, directory, or special entry
  remains rejected.
- Add a macOS arm64 CLI bundle assembler that always stages Chrome and qpdf
  under `<bundle>/sidecars/`, moves the candidate, and performs a real local
  generated-PDF conversion through executable-relative discovery with no
  sidecar path overrides before writing `SHA256SUMS`. This supersedes earlier
  evidence that had valid sidecar components at the wrong package level.
- Replace Chrome's flattened heading-derived bookmarks for generated PDFs with
  an explicit occurrence-aware tree built from the authoritative source TOC.
  A separately shipped pinned qpdf 12.4.1 macOS-arm64 sidecar writes only the
  outline/catalog update after Chrome rendering. Runtime validation traverses
  the resulting `/Outlines` graph and verifies exact order, hierarchy,
  parent/sibling/count links, local named destinations and URI-only external
  actions. The schema-3 top level was `Cover`, `Contents`, and one guide-title root;
  repeated TOC positions remain distinct, URL-less groups remain collapsible,
  and supplementary topics are grouped only when present. Publisher PDFs
  bypass qpdf. Settings schema 3 rejects earlier Chrome-inferred outlines.
- Correct WP10/WP11 Flare pagination by removing the source-verified homepage
  `topichero`/`docname` navigation subtree only from transient print assembly
  and resetting its companion container offset only on that homepage topic.
  The canonical generated cover is now the sole title cover. Pinned-Chrome
  preflight rejects any visible fixed/sticky publisher content that survives,
  verifies one bounded canonical cover title, and persists those audit results.
  Generated-PDF settings schema 3 prevents companions produced with the earlier
  overlay-prone print contract from being accepted as current. Acceptance
  diagnostics now check repeated large text, meaningful geometric text overlap,
  adjacent-page boundary duplication with source and repeated-table-header
  evidence, cover geometry, Letter media, and deterministic interior samples.
- Implement WP11 optional HTML-derived PDF companions with the pinned
  macOS-arm64 Chrome for Testing headless-shell sidecar controlled through
  chromedp. `--convert-html-to-pdf` is explicit opt-in; publisher PDFs remain
  preferred, complete HTML remains canonical, and no renderer is downloaded,
  searched from `PATH`, or selected as a fallback. Rendering uses a private
  profile, disables scripts, denies network/foreign-file access, enforces
  time/RSS/output/page/log limits, streams into a private partial file, and
  validates before atomic publication. Manifests record the transient input,
  renderer, settings, output, timing and validation evidence; guide/version
  indexes and ZIPs link the companion while search and source history remain
  HTML-based. Failed conversion preserves complete HTML and, during refresh,
  retains a prior valid companion.
- Implement WP10 canonical combined print HTML as a transient internal renderer
  input. The bounded streaming assembler consumes only a complete verified
  local HTML guide plus its authoritative plan, preserves topic/TOC/overview
  and supplementary ordering, namespaces topic/bookmark targets, rebases and
  validates local CSS/image/SVG/font references, preserves substantive
  table/list/caption/code semantics, and records an in-memory source-manifest
  hash. It has no CLI flag and does not write guide/version manifests, normal
  libraries, ZIPs, snapshots, or history; generated PDF was deferred to WP11.
  One-use acceptance completed nine of ten exact live guides and validated
  their 4,636 topics offline; 9300/10.15 IP Routing remained source-incomplete
  after one exact planned-topic request timed out. Network-blocked Chrome
  checks also led to scoped screen containment for preserved preformatted and
  Flare CLI content without changing source text or publisher table styling.
- Separate mapped-source availability from optional native-PDF availability in
  model, JSON and human selection/listing output. Fresh exact Flare/static/HPE
  fronts and body-free direct-PDF HEADs classify only HTTP 404/410 as definitive
  absence; such rows remain visible but disabled, block complete `--all`, and
  reject fixed selections before native library output. Transient and ambiguous
  failures remain selectable unknowns, and full PDF bytes are still verified
  independently during download.
- Keep HPE multipage `code.codeph` tokens inside publisher tables atomic when
  the intentionally omitted DataTables runtime no longer supplies live column
  sizing. The generated rule is HPE/table-only; unusually long tokens scroll
  within a 60-character/viewport cap instead of widening the page, while prose
  code, preformatted blocks, Flare/static guides, source text and publisher
  table presentation remain unchanged.
- Implement WP9 as a clean Go-only state cutover. Existing Python/incompatible
  exact-version libraries and journals are rejected read-only before native
  locks or staging and rechecked under the owned publication lock. Native raw
  caches now require a private atomic root marker (`application: aoscx-docs`,
  `cache_schema_version: 2`); the default moved from unmarked `raw-v1` to
  `raw-v2`. Nonempty unmarked/Python caches remain untouched. No migration,
  import, compatibility-reader, shared-cache, or offline-catalogue flags were
  added.
- Add a fail-closed MadCap WebHelp2 inventory fallback for pages without
  `data-mc-linked-toc`. The source-verified path reads guide-root
  `Data/HelpSystem.xml`, requires one safe root `Toc` attribute, and reuses the
  strict JSON5 module/chunk/tree parser without crawling auxiliary metadata.
  Retained 10000/10.16 IP Services metadata replay accounts for 354 navigation
  entries and 353 unique topics; no topic or asset bodies were fetched.
- Verify from current publisher handler code plus the exact refreshed
  `cli.json` mapping that 8320/10.17 CLI resolves to
  `cli_8320-8325.pdf`. Its exact HEAD response remains HTTP 404, so no PDF body
  was requested and no alternate route was invented.
- Add typed native retrieval failures with canonical request/final URLs,
  observable stage, elapsed time, application retry count and wrapped cause.
  The existing single complete-request deadline and shared application retry
  budget remain authoritative across robots, compatible lease admission,
  pacing, response headers/body and retry backoff; cancellation, deadline and
  HTTP status matching remain available through `errors.Is`/`errors.As`.
- Classify fragment-only HPE links as references to the exact current planned
  topic. Source-absent same-page anchors now receive publisher warnings, while
  source-present anchors lost locally remain errors; percent-encoded identity
  is decoded once and original `href` spelling is preserved.
- Use HTML-, PDF-, mixed-format or empty-library wording in generated native
  library indexes. PDF-only title-search and no-content-extraction notices
  appear only where applicable; links and search matching behavior are
  unchanged.
- Record optional route origin on resolved documents and central guide
  manifests, distinguishing literal publisher URLs, HPE document IDs and
  adapter-derived book-stem routes. A resolved PDF route still does not claim
  that PDF bytes were verified.
- Replace the short-page login keyword heuristic with source-aware structural
  shell validation. Valid short HPE/Flare/static instructions may mention login,
  denied/not-found/loading text or JavaScript identifiers, while password
  forms, exact bare error/loading responses, context-matched response prose,
  JSON placeholders, empty content and JavaScript-only shells remain
  fail-closed after source identity/container validation. Error headings alone,
  paragraph counts and decorative table/code elements are not classification
  shortcuts.
- Correct cross-page bookmark classification when an authoritative publisher
  topic itself lacks the referenced anchor. The warning-only exception is
  scoped to the exact generated referrer, generated target and normalized
  fragment; source-present local losses, other referrers/targets, missing files
  and SVG dependencies remain errors.
- Clear an owned live progress line on every command exit before outer fatal
  diagnostics, and use sparse plain progress for `TERM=dumb`. `NO_COLOR` still
  disables styling without disabling an otherwise usable TTY progress bar.
- Replace wide per-callback counters with a throttled, terminal-width topic
  progress bar. Dynamically growing assets and final verification use separate
  honest stages; fully specified terminal commands receive the same display,
  while redirected stderr remains sparse and control-free. Detailed cache
  counts appear once in the final publication summary.
- Report bounded per-guide archive errors and publisher/source warnings when a
  guide is incomplete even if archiving returned no Go error. Final diagnostics
  point to the complete manifest and retained unfinished work; complete,
  incomplete and failed statuses retain green, yellow and red presentation.
- Add Left-Arrow backward navigation across the native guided questions.
  Previously visited editable questions restore valid cursor, checkbox and
  destination drafts; explicit flag inputs are skipped, and platform/release
  changes rebuild dependent availability before download.
- Preserve every fixed `--guides` ID while interactive questions complete a
  partial command, rejecting unresolved IDs instead of silently filtering them.
  Explicit `--all=false` continues to ask for all versus individual guides.
- Match the Python Questionary destination prompt with an inline path and
  compact multi-column directory menu. Up/Down and Tab cycle sibling previews,
  inactive Tab inserts their common prefix, Slash continues into the selected
  directory, and Enter closes an active menu before a later Enter submits.
  Left Arrow remains wizard Back. Nonexistent typed destinations and blank
  Enter retain their prior meanings.
- Correct Python completion parity: a fresh blank prompt does not open a menu;
  explicit blank Tab starts completion; forward/reverse cycling includes the
  original or common-prefix-expanded draft; unique inactive-Tab completion
  closes the menu; and menus use minimum-row, column-major grouping with
  bounded long-label cells instead of a flat maximum-column row.
- Add Python-inspired semantic colors to interactive questions, active answers,
  disabled rows, completion menus, phases, warnings, failures, successes,
  human results and listings. Per-stream detection disables styling for
  redirects, `NO_COLOR` and `TERM=dumb`; safe display escaping precedes styling,
  and JSON/source/path values remain unchanged.
- Preserve directory-only completion for literal absolute, relative, `./`,
  `../` and `~/` prefixes, hidden-directory convention, bounded non-recursive
  scans, stale-result rejection and safe filesystem-name display without
  creating output during selection.
- Make completion generation admission and queue replacement atomic so a
  delayed initial request cannot displace fast typing, and report unavailable
  cwd/home bases without accidentally scanning `.` or `/`; absolute completion
  remains independent.
- Show complete selection lists when they fit the terminal, replacing the
  artificial 12-row window. Oversized lists adapt to terminal size and resizing.

- Add a native terminal-guided flow for platform, platform-specific release,
  all versus individual mapped guides, and base destination. Confirmed
  zero-mapping releases remain visible but disabled; mapping failures are
  reported as unknown/partial availability. Escape, Ctrl-C, Ctrl-D and closed
  input cancel without creating a library, and fully specified commands never
  prompt.
- Implement `--all` for the selected Product Documentation platform/release
  across mapped PDF, Flare, HPE multipage and supported static/Oxygen guides.
  It preserves catalogue processing order and fails before output when mapping
  or route failures prevent proving a complete selection.
- Make the fixed browser-free `compatible` transport the native default by
  explicit product decision. Standard native HTTP remains available only via
  `--transport http`; there is no browser or transport fallback.
- Add bounded stderr progress for elapsed phases, guide position, validated
  authoritative topics and dynamically discovered/processed assets. JSON
  stdout remains machine-readable.
- Keep the guided flow in one terminal program so Enter freezes the confirmed
  value and bounded type-ahead is delivered to the next question. Escape
  publisher-controlled terminal control/format characters in human prompts,
  progress, diagnostics and listings while preserving original JSON/manifest
  metadata. Emit topic completion once and throttle serial asset bursts while
  retaining an explicit final archive snapshot.

- Add bounded native download workers (`--workers`, default 4) for validated
  catalogue mappings and authoritative planned HTML topics. Topic parsing,
  supplementary discovery, assets and output remain serial and ordered.
- Replace cache-wide retrieval serialization with per-canonical-URL
  coordination. Same-URL callers share one transaction and receive independent
  verified disk handles; different URLs may use the existing shared transport
  policy concurrently. Conditional refresh is not repeated between planning,
  prefetch and archive reads.
- Ensure a new same-URL caller waits for a canceled zero-waiter flight to retire
  instead of inheriting its cancellation, and invalidate prefetch freshness
  after any failed newer validation.
- Add guarded live-benchmark telemetry for logical request concurrency,
  dispatch-to-headers/body-close and sent-headers-to-first-byte latency, with
  connection attempts/header writes kept separate from logical calls.

- Preserve publisher table borders, header separators, padding and alignment
  instead of overriding them with the archive's generic grey grid.

- Display source-advertised full-resolution images inline instead of popup
  thumbnails in old-style Flare guides, preserving image bytes and aspect ratio.
  Remove only their popup/thumbnail constraints; new-style and ordinary images
  retain their behavior.

- Keep guide search results hidden until a nonblank query is entered, and place
  HPE front matter in a separate Guide overview area above the Contents tree.
- Fix false missing-link errors caused by inert publisher attributes such as
  `<span href="...">`. Validate active HTML and SVG references without turning
  non-link markup into links or altering its content.

- Added local-only HPE multipage and supported static/Oxygen HTML archive
  support on the shared bounded pipeline. HPE validates exact endpoint,
  document/page/query and DITA identities; static output requires explicit
  complete plans and substantive content.
- Added authoritative HPE public/API/query/bare-GUID aliasing, local Graphik
  fonts, metadata/copyright retention, source-specific layout, and
  mode-aware SVG stylesheet processing. Cross-document/ambiguous links remain
  online and missing required resources make output incomplete.
- Verified retained HPE inventory plus deterministic topics, built-executable
  static publication, mixed PDF/Flare/HPE/static transactions, and offline
  font/image/table/code rendering. No M3D publisher request was made; its
  separate single-use live harness remains uninvoked pending authorization.

- Added an optional transport-wide HTTP attempt budget with conservative
  transparent-replay headroom, typed exhaustion/counters, one overall live-run
  deadline, and an exclusive single-use live authorization marker. Real
  standard HTTP/1, standard HTTP/2 and pinned req HTTP/2 retry paths are covered.
- Corrected M3C historical accounting: the failed live attempts produced 132
  distinct successful cache entries and a reconstructed 168–169 transmissions,
  not an exactly observed 169. Historical artifacts remain unchanged.
- Completed the one authorized fresh 6300/10.16 Job Scheduler verification in
  62 conservative attempt starts/header writes and 122.44 seconds, with exit0,
  a complete 24-topic/6-stylesheet archive, 228 checked local links, and no
  budget exhaustion, replay-allowance violation, retries or redirects.

- Added the first complete native MadCap Flare HTML archive path: two-pass topic
  and bookmark closure, source-aware content extraction, bounded dependency
  graph, tokenized CSS rewriting, SVG validation/inlining, shared TOC,
  previous/next navigation, offline search, provenance, and integrity checks.
- Enabled explicit Flare guide downloads through the existing CLI; at that
  checkpoint HPE/static HTML output and `--all` remained fail-closed. Added one shared aggregate
  source-byte limit and made `--refresh` revalidate inventory inputs.
- Extended transactional native libraries to mix verified HTML guides with
  one-file publisher-PDF guides, including source-change history, failed-update
  retention, and hashes for generated executable search code.
- Verified the 6300/10.16 Job Scheduler retained live inputs as 24 topics,
  6 required stylesheets, 20 tables, 124 cells, 31 exact command-screen blocks,
  and 228 local links. Local `file://` rendering completed with no HTTP events.
  The preceding live attempts failed closed and exceeded their request target;
  the later single-use guarded verification above succeeded.

- Fixed JSON5 state leakage across adjacent comments and NUL escape lookahead,
  including resulting Flare source URLs and titles.
- Restored bounded HTML BOM/HTTP/meta charset detection for catalogue and TOC
  text without applying text decoding to PDF bytes.
- Rejected source-advertised HPE PDFs when API-path, query, final URL or optional
  response document identities conflict with the selected document.

- Added typed, serializable complete source plans for Flare, HPE multipage and
  supported explicit static/Oxygen TOCs, including public/fetch identities,
  repeated hierarchical positions, deduplicated topics and input hashes.
- Added complete Flare detailed-TOC/module/chunk accounting using a licensed
  local pure-Go JSON5 decoder fork with duplicate-key and resource guards.
- Added exact HPE front matter and nested `content.json` planning, including the
  observed endpoint-specific `multiPage` JSON MIME, strict document/query scope,
  and source-advertised/direct returned PDF verification.
- Verified a live 24-topic/28-position Job Scheduler Flare inventory and retained
  live HPE inputs that replay to a complete 25-topic plan. No topic/asset archive
  output is claimed or enabled yet.

- Fixed publication after lock ownership loss: shared commit/rollback/recovery
  now revalidate lock and journal identity, journal creation refuses replacement,
  and foreign transaction files are preserved.
- Fixed rejection of valid compact PDF xref-stream objects such as `obj<<`,
  with real-offset stream fixtures and negative lexical-boundary regressions.

- Added M3A noninteractive downloads of directly mapped original publisher PDFs
  with space-separated guide IDs, destination/refresh options, unchanged bytes,
  readable names and bounded PDF byte/EOF/xref checks.
- Added safe native library staging, owned locks, snapshots, publication journals,
  rollback/recovery, central metadata, title-only search, history and Ctrl-C
  publication of accepted PDFs. Previous complete guides survive failed updates.
- Added an explicit native schema marker; Python-library/cache/catalogue
  interoperability remains pending and incompatible state is rejected without
  overwrite.
- Fixed compatible request pacing after occupied lease queues; rate checks now
  run at dispatch rather than allowing released requests to bunch together.
- Verified one complete live 6300/10.10 Job Scheduler original PDF and added
  actual-executable publication, refresh, corruption, unsafe-selection and
  cancellation regressions.

- Added explicit native `--transport compatible` catalogue listing with the
  actual application User-Agent and unchanged default HTTP backend. A bounded
  live run retrieved the portal and all 28 then-advertised mapping files with
  exit0/complete JSON; guide downloads remain unimplemented.
- Separated additional-header/order and HTTP2-setting groups in a controlled
  comparison. The header group passed the robots gate with either HTTP2 group;
  HTTP2 settings alone did not. The exact publisher/CDN rule remains unknown.
- Isolated a pinned req HTTP2 connection-setup race using bounded deep-clone
  leases, retaining parallel responses, cancellation, cleanup and warm reuse.
- Retained exact fresh source URLs/bytes/hashes and verified the real
  executable's 6300/10.16 listing via explicitly local replay.

- Added a native catalogue-only milestone alongside the unchanged Python 0.5
  implementation: offline help/version, validated flags, complete observed
  Product Documentation schema parsing, exact route listing and JSON diagnostics.
- Added native HTTP policy and a streamed, verified raw cache, with local
  HTTP fixture coverage. The native CLI does not launch/install a browser.
- Recorded the full rewrite parity plan and decision gates. Live catalogue
  access is currently blocked by HTTP 403 at the catalogue host's robots
  endpoint; a separate HPE Support front-matter retrieval succeeded. Go is
  not yet a full archiver, and this checkpoint does not change the release version.
- Corrected M1 robots validation so JSON/XML/error and unknown-only responses
  cannot silently allow retrieval. Legitimate empty/comment-only text remains
  explicitly unrestricted.
- Included transient streaming-body failures in one transport-owned retry
  budget/deadline, without appending partial data or replacing prior verified
  cache entries after a failed refresh.
- Preserved mapped document bookmarks separately from defragmented HTTP/cache
  identities, and made cancellation win during final catalogue parsing/output
  and before returning the CLI exit code.
- Added a build-gated M2A feasibility adapter for pinned req v3.61.0, using one
  fixed TLS profile under the existing request policy, plus shared contract and
  targeted secure-TLS/HTTP2/cookie-isolation tests. The default CLI is unchanged.
- Recorded the additional native client's robots HTTP 403 refusal and stopped
  before catalogue/mapping/guide requests. No browser, proxy change, alternate
  profile or saved-catalogue fallback was introduced.
- Added a separately authorized, build-gated controlled diagnostic matrix with
  actual TLS/cipher/HTTP/header/connection observations, redacted denial excerpts,
  request/deadline limits and application-identity robots enforcement.
- The 13-request diagnostic found that the full req Chrome120 preset retrieves
  permitted robots and a catalogue/one-mapping sample, whereas standalone UA,
  HTTP-version and TLS-version changes still receive403. Restoring the app UA
  within the full preset also retrieves robots; content under that narrowed
  configuration is not yet verified. The default CLI and parity scope remain
  unchanged; the precise CDN rule is not known.

## 0.5 - 2026-09-12

### Added

- Added concurrent body-free HPE PDF availability checks before rendering
  individual guide choices.

### Changed

- The interactive destination prompt now shows the process's current working
  directory as its default while leaving the editable field empty.
- Generated PDF tables recognize publisher headers encoded as `<thead><td>`
  and emphasize repeated header rows without inventing first-column styling
  absent from the source.
- Generated PDFs preserve Courier New/monospace typography for legacy and
  modern command syntax, screen output, code blocks, and parameter values.
- Publisher and HTML-derived PDFs use the readable filename
  `<platform> - <software version> - <document title>.pdf`; manifests retain
  whether the file is an unmodified original or a local conversion.
- Publisher-PDF guide directories are minimal one-file exports. Their
  provenance and update metadata are stored in the version-level manifest,
  and the root library links directly to the PDF.

### Fixed

- Interactive guide labels no longer claim that an HTML guide has a
  source-verified PDF before checking the publisher export endpoint; verified
  dual-format guides are shown as `[HTML / PDF]`.

## 0.4 - 2026-09-12

### Added

- Added optional `--convert-html-to-pdf` generation for complete HTML guides.
- Generated PDFs combine all topics into one Letter-size document with a
  hierarchical, clickable, page-numbered table of contents and PDF bookmarks.
- Added generated-PDF provenance, checksums, page/link/outline metrics, local
  resource isolation, and qpdf-backed structural validation.

### Changed

- Generated print layouts use modern HPE-style cover, typography, code, table,
  footer, and chapter pagination while preserving the archived HTML as the
  canonical source.
- Library indexes distinguish unmodified publisher PDFs from HTML-derived
  generated PDFs.

## 0.3 - 2026-09-12

### Added

- Added `--prefer-pdf` and `--no-prefer-pdf`, plus an equivalent interactive
  prompt.
- HPE PDF preference always uses the whole-document **Export all content**
  endpoint. Selected-topic and selected-topic-with-subtopics exports are never
  used.
- Added explicit platform/version availability errors that list the versions
  mapped to the selected platform.
- Added format-specific rendering for older Flare documents and newer HPE
  multipage documents.

### Changed

- Older Flare pages now retain Open Sans typography, publisher-aligned content
  geometry, and a contextual TOC branch matching the online guide.
- Newer HPE pages now archive HPE Graphik regular and bold fonts locally and
  preserve HPE content, TOC, table-caption, and figure-caption geometry.
- Command and code blocks use the full available content width while preserving
  horizontal scrolling and source whitespace.
- Legacy `<br></br>` markup is normalized using browser-compatible behavior so
  figure spacing is preserved.

### Fixed

- Prevented partial HPE PDF exports when whole-document export was requested.
- Prevented unsupported platform/version combinations from reaching output
  creation.
- Fixed duplicate publisher IDs, malformed nested links, and inline MadCap
  elements that previously produced structurally invalid local HTML.
- Rejected HPE API responses whose returned page identity differs from the
  requested topic.
- Prevented out-of-TOC HPE links from creating false supplementary topics.

## 0.2 - 2026-09-12

### Added

- Added bounded four-worker prefetching for direct HTTP topic downloads.
- Added `--offline-catalog` and `--workers`.
- Added topic throughput and estimated-time-remaining progress output.

### Changed

- Reduced the default per-host request-start interval from 0.25 seconds to
  0.1 seconds.
- Large guides now share one persistent TOC instead of duplicating thousands of
  links into every topic page.
- Catalogue-only browser sessions are released before long direct-HTTP
  downloads.

### Fixed

- Bounded browser shutdown so a disconnected Playwright driver cannot block
  process exit indefinitely.
- Added strict generated-HTML validation before a guide can be marked complete.

## 0.1 - Initial release

- Added live AOS-CX catalogue discovery, platform/release/guide selection,
  complete TOC-driven HTML archiving, source-verified PDF handling, offline
  navigation/search, resumable caching, integrity manifests, snapshots, and ZIP
  export.
