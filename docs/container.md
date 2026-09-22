# Running aos-cx-docs-dldr in a container

This is how Windows users run `aos-cx-docs-dldr`. The container is one of the
application's two deliverables, alongside the native macOS arm64 bundle. Both
are complete builds: the application plus its pinned Chrome and qpdf sidecars,
so generated-PDF conversion works identically in either.

Current source builds run `aos-cx-docs-dldr`, with container entry point
`/opt/aos-cx-docs-dldr/aos-cx-docs-dldr` and wrapper `scripts/aos-cx-docs-dldr.ps1`.
The replacement v0.8 release includes this rename. Image, package and cache names use the new product
name. Existing old-name libraries are rejected read-only; use a new destination.

The image is **linux/amd64 only**. Upstream qpdf 12.4.1 publishes no arm64
binary, so an arm64 image cannot carry the exact pin and is deliberately not
built.

## What is inside

The application code is licensed under Apache-2.0, with its license text at
`/opt/aos-cx-docs-dldr/LICENSE` and the application OCI license label `Apache-2.0`.
Bundled components retain their own licenses; the label does not relicense
Chrome, qpdf, their dependencies, or the base-image packages. Archives built
at `e1764e3` predate this change and need fresh assembly to include it.

| Component | Version | Verified by |
| --- | --- | --- |
| `aos-cx-docs-dldr` | 0.8 | built from source in the image |
| Chrome for Testing headless shell | 153.0.8010.36, r1681091, linux64 | archive SHA-256 at build; executable SHA-256, ELF x86-64, before every launch |
| qpdf | 12.4.1, linux-x86_64 | archive SHA-256 at build; every component's SHA-256, ELF x86-64 and exact shared-library closure, before every launch |
| Base | `debian:trixie-slim`, pinned by digest | Chrome's runtime libraries, `libgmp10`/`zlib1g` for qpdf, `procps`, fonts |

Every external input is pinned. Base images are pinned by digest; when
updating one, change the tag and the digest together. The Chrome and qpdf pins
live in `internal/pdfgen/sidecar_platform.go` and the `Dockerfile` must agree
with them, which the build proves by running the application's own verifier
against the staged sidecars.

Chrome runs with `--no-sandbox` inside the container, because container
runtimes deny the user namespaces its sandbox needs and the headless shell
refuses to start without them. Isolation rests on the container boundary, the
unprivileged user (UID 65532), and the renderer controls the application
already enforces: scripts disabled, all network routed to a dead proxy,
foreign file access refused at the CDP layer, private throwaway profile. macOS
keeps Chrome's own sandbox.

## Get the image

Users load a prebuilt archive; they need no source, no Go toolchain, and no
access to the Go module proxy. Download the archive, `SHA256SUMS`, and the
PowerShell wrapper from
[release `v0.8`](https://github.com/ib0ndar/aos-cx-docs-dldr/releases/tag/v0.8).

```powershell
Get-FileHash aos-cx-docs-dldr-0.8-linux-amd64.tar.gz -Algorithm SHA256
docker load -i aos-cx-docs-dldr-0.8-linux-amd64.tar.gz
```

The archive is about 230 MB and contains the image tagged exactly
`aos-cx-docs-dldr:0.8`, so the wrapper needs no configuration.

### Producing the archive

```sh
./scripts/package-container.sh ~/Downloads/aos-cx-docs-dldr-container
```

This builds the `linux/amd64` image, asserts it reports the requested
architecture and version, saves it as a gzipped archive, and writes
`SHA256SUMS`. The version comes from `internal/model/model.go`.

### Building directly

```sh
docker build --platform linux/amd64 -t aos-cx-docs-dldr:0.8 .
```

The build downloads the Chrome and qpdf archives by pinned checksum, stages
them beside the executable, and runs `aoscx-verify-sidecars` against the result
before the image is finished. Pass `--platform linux/amd64` on an Apple Silicon
host; the image is amd64 only.

## Set up the wrapper

Dot-source [`scripts/aos-cx-docs-dldr.ps1`](../scripts/aos-cx-docs-dldr.ps1) from your
PowerShell profile:

```powershell
. C:\path\to\aos-cx-docs-dldr.ps1
```

A default Windows 11 installation ships Windows PowerShell with the
`Restricted` execution policy, which refuses to dot-source any script. Allow
local scripts for your own account once, and clear the download mark if the
file came through a browser:

```powershell
Set-ExecutionPolicy -Scope CurrentUser RemoteSigned
Unblock-File C:\path\to\aos-cx-docs-dldr.ps1
```

The wrapper hides the container plumbing, pins `--destination /library`, omits
that pin for `--list` (which the CLI rejects alongside a destination), and
reports the Windows folder when the run finishes. Docker Desktop must be
running; the wrapper only reports a missing `docker` command, not a stopped
engine.

## Use it

```powershell
aos-cx-docs-dldr --list --platform 6300 --release 10.18.xxxx
aos-cx-docs-dldr --platform 6300 --release 10.18.xxxx --all
aos-cx-docs-dldr --platform 6300 --release 10.18.xxxx --all --convert-html-to-pdf
aos-cx-docs-dldr
```

The last form runs the guided flow. It still asks for platform, release, guide
selection, and PDF preference. It does not ask for a destination, because a
fixed flag is an immutable wizard input.

Results appear as normal Windows files:

```text
C:\Users\<you>\Documents\AOS-CX\
  6300\
    10.18.xxxx\
      index.html
      manifest.json
      jobscheduler\
        index.html
        6300 - 10.18.xxxx - Job Scheduler Guide.pdf   <- with --convert-html-to-pdf
        pages\
        assets\
```

## Paths

| Container path | Host |
| --- | --- |
| `/library` | `%USERPROFILE%\Documents\AOS-CX`, or `$env:AOSCX_DOCS_LIBRARY` |
| `/cache` | A named volume, `aos-cx-docs-dldr-cache`. Not visible from Windows. |
| `/opt/aos-cx-docs-dldr` | The application and its sidecars. Read-only in practice. |

The raw response cache stays inside a Linux-native volume on purpose. It
enforces private `0700`/`0600` permissions that a Windows bind mount cannot
represent, and it is disposable: delete the volume to reclaim the space.

```powershell
docker volume rm aos-cx-docs-dldr-cache
```

Because progress and result output come from inside the container, they name
`/library/...` rather than `C:\...`. The wrapper prints the real folder after
each run.

## Environment variables

The complete list, including the application's own variables, the proxy and CA
variables the Go standard library honours, and the wrapper's own settings, is
in the [README](../README.md#environment-variables).

### Proxies and TLS interception

Both transports honour `HTTPS_PROXY`, `HTTP_PROXY`, and `NO_PROXY`. The wrapper
does not forward arbitrary variables into the container, so pass them with
`-e` when calling the engine directly.

If a proxy intercepts TLS, mount its CA and point `SSL_CERT_FILE` at it. The
image is Linux and does not consult the Windows certificate store:

```powershell
docker run --rm -i `
  -v "$env:USERPROFILE\Documents\AOS-CX:/library" `
  -v aos-cx-docs-dldr-cache:/cache `
  -v "C:\certs\corporate-ca.pem:/certs/ca.pem:ro" `
  -e SSL_CERT_FILE=/certs/ca.pem -e HTTPS_PROXY=$env:HTTPS_PROXY `
  aos-cx-docs-dldr:0.8 --destination /library --platform 6300 --release 10.18.xxxx --all
```

Both behaviours were verified against local fixtures: a dead proxy produces a
`proxyconnect` failure on each transport, and a self-signed fixture is rejected
without `SSL_CERT_FILE` and accepted with it.

## Podman

Podman accepts the same arguments:

```powershell
$env:AOSCX_DOCS_ENGINE = 'podman'
```

## Scripting

For non-interactive use, add `--json` and either keep the wrapper (it omits the
TTY automatically when output is redirected) or call the engine directly:

```powershell
docker run --rm -i `
  -v "$env:USERPROFILE\Documents\AOS-CX:/library" `
  -v aos-cx-docs-dldr-cache:/cache `
  aos-cx-docs-dldr:0.8 --destination /library `
  --platform 6300 --release 10.18.xxxx --all --json > result.json
```

## Libraries from other versions or platforms

The container reads and writes one library version only, and refuses a folder
created by any other version, leaving it untouched.

A library is also tied to the platform that produced it. Generated-PDF records
carry the sidecar pins of that platform, and integrity digests carry Unix
permission bits, so a library made on macOS is rejected by the container and
vice versa. Download afresh rather than copying between them.

## What has and has not been verified

The fully renamed `aos-cx-docs-dldr` executable and wrapper were subsequently
verified under Windows PowerShell 5.1 with the real Docker engine:
help/version, fixture listing, PDF generation and library reopen passed.

The image and the CLI inside it were exercised on a macOS arm64 host using
OrbStack with `--platform linux/amd64`, against a local fixture server. No
publisher request was made. Confirmed there:

- the image builds and its build-time sidecar verification passes;
- version reporting and `--help` run with networking disabled;
- a missing network fails closed at the robots stage;
- the raw cache is created on a named volume with the required `0700`/`0600`
  permissions;
- a complete guide publishes into a bind-mounted host folder;
- **`--convert-html-to-pdf` produces a real PDF**: 5 pages, Letter, embedded
  fonts loaded, qpdf-installed outline, footer overlay, `qpdf --check` clean on
  the host, and pages render correctly when rasterised;
- the manifest records the Linux sidecar pins, and the library reopens and
  revalidates against them;
- `--zip` produces an archive that passes integrity checks;
- the archive produced by `scripts/package-container.sh` reloads as
  the then-current version-tagged image and runs.

The PowerShell wrapper was executed under PowerShell 7.6 on macOS against a
stand-in engine that echoes its arguments. Confirmed: the script parses; `--list`
omits the pinned destination and every other form includes it; the no-argument
guided form works; the engine's stdout is the only output, so
capturing the wrapper's `--json` output returns just the application's result; the exit
code is left in `$LASTEXITCODE`; and a missing engine produces a clear error
rather than a crash. The script avoids PowerShell 6+ constructs so it remains
valid on Windows PowerShell 5.1.

### Verified on a Windows host

The same archive was then loaded and driven on a real Windows host: Windows 11
Pro 25H2 (build 26200) x64, Windows PowerShell 5.1, Docker Desktop 4.91.0 with
the WSL 2 backend (WSL 2.7.14, engine 29.8.0 linux/amd64). The host was a
virtual machine with nested virtualization; the guest ran Hyper-V, WSL 2 and
Docker Desktop exactly as a physical machine would. The fixture portal was a
second container on the same engine, so no publisher request was made.
Confirmed there:

- `SHA256SUMS` verifies on Windows and `docker load` restores the tested image
  with the published image ID;
- dot-sourced under Windows PowerShell 5.1, the wrapper runs version reporting,
  `--list`, `--list --json` and a full download; `$LASTEXITCODE` carries the
   application's exit code; capturing the wrapper's `--json` output returns only the
  JSON while progress goes to the console;
- the wrapper creates `%USERPROFILE%\Documents\AOS-CX` and the container's
  UID 65532 writes through the Docker Desktop bind mount: staging, renames,
  durable writes and publication all succeed, and the published tree appears
  as ordinary Windows files;
- inside the container the bind mount presents every file as `65532:65532`
  with drvfs-synthesised modes `0600` (files) and `0700` (directories);
- **`--convert-html-to-pdf` produces a real PDF on the Windows host**: 5
  pages, rendered by the pinned Chrome 153.0.8010.36, outline installed by
  qpdf, `qpdf --check` clean;
- a second run reopens the library written through the bind mount, passes
  preflight and integrity validation, reuses the cache, republishes
  transactionally, and leaves `.staging` empty; `history.json` records both
  runs.

Two Windows-specific findings were folded into the setup steps above: the
default `Restricted` execution policy blocks dot-sourcing, and the wrapper
cannot tell a stopped Docker Desktop from a running one.

Not verified: Podman Desktop as the engine, and Windows hosts other than
Windows 11 25H2 x64.

The 2026-09-20 release validation repeated archive checksum/load, version,
listing, generated-PDF download, library reopen and qpdf checks with the newly
assembled archive on the same Windows host. It also fixed gzip timestamp
metadata so two cached container packaging runs produce identical checksums.
Two subsequent clean-commit assemblies of each deliverable and Windows
revalidation passed at `e1764e3`.

### Security restoration and guided flow

On the same Windows host, VSM automatic startup was restored, explicit
VBS/Credential Guard disable overrides removed, and HVCI enabled. After reboot,
WMI reported VBS running and memory integrity configured/running. Hyper-V and
Docker Desktop's WSL 2 engine remained operational; PDF generation, library
reopen and qpdf checks passed. The VM lacks Secure Boot and nested MBEC, so this
does not establish equivalence to default Windows security settings.

The guided flow was exercised in Windows PowerShell 5.1 over an SSH PTY with
the real Docker engine. Platform/release selection, individual-guide selection,
Back with selection preservation, skipping the fixed destination, opt-in PDF
generation, Windows folder output and exit code 0 passed. Input/output were
nonredirected and the wrapper allocated the container TTY. The resulting
five-page PDF passed outline/footer validation and qpdf checks.

Only fixture/transport/cache/timing controls were fixed. Literal zero-argument
publisher invocation, the optional publisher-PDF preference prompt, and a local
Windows console session remain unverified.
