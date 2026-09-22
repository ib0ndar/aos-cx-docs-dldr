# aos-cx-docs-dldr

`aos-cx-docs-dldr` creates a browsable offline library from supported guides in
the public HPE Aruba Networking AOS-CX Product Documentation catalogue. Choose
a switch platform and documentation release, download guides, then open the
release's `index.html` in a normal web browser.

Downloaded HTML and publisher PDFs open without an internet connection. HTML
is full-text searchable; publisher PDFs are searchable by title. Cross-guide
links become local when their targets are verified in the downloaded library;
links intentionally retained to online content still require internet access.

**What you get**

```text
$HOME/Documents/AOS-CX/6300/10.18.xxxx/index.html      <- open this
```

A contents page lists each downloaded guide and provides offline navigation and
search within the limits above.

**Supported delivery paths**

Two complete packages are available. Both include the pinned
Chrome and qpdf components required for optional HTML-to-PDF conversion.

| Use on | Deliverable | Architecture | Status |
| --- | --- | --- | --- |
| macOS | Native bundle | Apple Silicon (`arm64`) | Unsigned and unnotarized |
| Windows | Linux container under Docker Desktop | 64-bit Intel/AMD (`linux/amd64`) | Verified on the environment described below |

There is no native Windows executable, Linux native package, Intel Mac build,
or ARM container. Retrieval is browser-free. Packaged Chrome is used only for
local generated-PDF rendering, and sidecars are never downloaded at runtime.

The source is public at
[github.com/ib0ndar/aos-cx-docs-dldr](https://github.com/ib0ndar/aos-cx-docs-dldr).
Get prebuilt packages from the
[latest release](https://github.com/ib0ndar/aos-cx-docs-dldr/releases/latest).
For a specific version, use
[all releases](https://github.com/ib0ndar/aos-cx-docs-dldr/releases).
Each release provides its downloads, checksums, and release notes; the
[changelog](CHANGELOG.md) records the full history.

**Contents**

- [Install on macOS](#install-on-macos)
- [Install on Windows](#install-on-windows)
- [Update the application](#update-the-application)
- [Everyday use](#everyday-use)
- [Output and library lifecycle](#output-and-library-lifecycle)
- [Command reference](#command-reference)
- [Troubleshooting](#troubleshooting)
- [Environment variables](#environment-variables)
- [Limits and current status](#limits-and-current-status)
- [For maintainers](#for-maintainers)
- [License](#license)

**Three words you will see**

| Term | Meaning |
| --- | --- |
| Platform | A switch family, such as `6300` or `8320`. |
| Release | An AOS-CX documentation version label, such as `10.18.xxxx`. Treated as text, not a number. |
| Guide | One document, such as the Job Scheduler guide. Each has a short ID like `jobscheduler`. |

The **application version** is separate from the AOS-CX documentation release.
In installation examples, replace `VERSION` with the application version from
your chosen release tag, **without the leading `v`**. Download all files for an
installation from that same release.

---

## Install on macOS

macOS runs the application directly. It is built for Apple Silicon (arm64).

Download `aos-cx-docs-dldr-VERSION-macos-arm64.zip` and its `.sha256` file from
your chosen [release](https://github.com/ib0ndar/aos-cx-docs-dldr/releases/latest).
In Terminal, change to the folder containing both downloads, then verify and
unpack them. Set `APP_VERSION` once, replacing `VERSION` as described above:

```sh
APP_VERSION='VERSION'
shasum -a 256 -c "aos-cx-docs-dldr-${APP_VERSION}-macos-arm64.zip.sha256"
```

If the archive checksum passes, extract it and check the bundle contents:

```sh
unzip "aos-cx-docs-dldr-${APP_VERSION}-macos-arm64.zip"
cd "aos-cx-docs-dldr-${APP_VERSION}-macos-arm64"
shasum -a 256 -c SHA256SUMS
```

Stop if either checksum command fails. Checksums establish that the files match
the release metadata; they do not replace Developer ID signing or notarization.

The folder must stay together as a unit:

```text
aos-cx-docs-dldr-VERSION-macos-arm64/
  aos-cx-docs-dldr         <- the application
  sidecars/                <- needed only for PDF creation
  LICENSE
  PACKAGE-MANIFEST.json
  SHA256SUMS
  SBOM.spdx.json
  THIRD_PARTY_NOTICES
```

### Allow the verified bundle before first launch

The bundle is unsigned and unnotarized. Browser downloads can carry a macOS
quarantine attribute into the extracted files. Approving only the main program
can leave Chrome, Vulkan libraries, qpdf, or other bundled libraries blocked
separately, sometimes with a dialog suggesting moving the file to Trash.

After **both checksum checks above pass**, and if you trust this release, run
the following while still inside the extracted bundle directory:

```sh
xattr -dr com.apple.quarantine "$PWD"
./aos-cx-docs-dldr --version
```

`-d` removes only the named quarantine attribute; `-r` applies recursively to
the entire bundle, including its sidecars and libraries. This avoids approving
each quarantined component individually. It does not sign or notarize the
software, create a permanent exception, or disable Gatekeeper globally. No
`sudo` is normally needed for a bundle extracted into your own folder.

Move the **whole folder** somewhere permanent, for example
`$HOME/Applications/aos-cx-docs-dldr/VERSION/`. Moving only the executable will break
PDF creation. If you already installed the bundle there, use its exact path:

```sh
xattr -dr com.apple.quarantine "$HOME/Applications/aos-cx-docs-dldr/${APP_VERSION}"
```

Apply this only to the verified bundle, never to your Downloads folder, home
directory, or the whole disk. Repeat for a newly downloaded replacement bundle
after verifying its checksums. Individual approval through **System Settings >
Privacy & Security > Open Anyway** remains an alternative, but may need to be
repeated for bundled components. If macOS still blocks a file, or explicitly
reports malware, stop and investigate the exact message; quarantine removal
does not fix damaged files, invalid signatures, or malware detections.

### First run on macOS

From the bundle directory, start guided selection and choose a current platform,
release, and guide:

```sh
./aos-cx-docs-dldr --destination "$HOME/Documents/AOS-CX"
```

The catalogue and downloads require internet access. After publication, open
`$HOME/Documents/AOS-CX/<platform>/<release>/index.html`; browsing the archived
content itself is offline except for links explicitly retained online.

---

## Install on Windows

On Windows the tool runs inside a Linux container that includes everything,
including PDF creation. You do not need to install Go or compile anything. The
documented route is Windows 11 x64 with Docker Desktop using its WSL 2 backend.
The workflow was verified against a fixture portal; live publisher retrieval
from Windows has not been tested. See [Windows status](#limits-and-current-status)
for the verification scope.

### Step 1 — Install Docker Desktop

Install **Docker Desktop** with the WSL 2 backend and leave it running.

Docker Desktop licensing depends on how it is used; consult its
[current subscription terms](https://docs.docker.com/subscription/desktop-license/).

If the engine is not running, the tool reports an engine error rather than a
documentation error. Podman Desktop has not been tested and is not part of this
primary installation path.

### Step 2 — Load the image

Download `aos-cx-docs-dldr-VERSION-linux-amd64.tar.gz`, `SHA256SUMS`, and
`aos-cx-docs-dldr.ps1` from
your chosen [release](https://github.com/ib0ndar/aos-cx-docs-dldr/releases/latest).

The image requires an x64 Windows host capable of running a `linux/amd64`
container. Windows on ARM is not supported.

In PowerShell, change to the download folder. Set `$appVersion` once, replacing
`VERSION` with the application version without the leading `v`, then check the
archive's hash:

```powershell
$appVersion = 'VERSION'
Get-FileHash "aos-cx-docs-dldr-$appVersion-linux-amd64.tar.gz" -Algorithm SHA256
```

Compare the printed hash with the archive's line in `SHA256SUMS`. Stop if it
does not match. If it matches, load the archive; `docker load` handles the
compressed file directly:

```powershell
docker load -i "aos-cx-docs-dldr-$appVersion-linux-amd64.tar.gz"
```

### Step 3 — Install the shortcut command

The downloaded `aos-cx-docs-dldr.ps1` lets you type `aos-cx-docs-dldr` instead
of a long `docker run` line. Put it in a permanent folder, for example
`$HOME\Applications\aos-cx-docs-dldr`, then allow local scripts for your account
and remove the browser download mark from this script:

```powershell
New-Item -ItemType Directory -Force "$HOME\Applications\aos-cx-docs-dldr" | Out-Null
Move-Item -Force .\aos-cx-docs-dldr.ps1 "$HOME\Applications\aos-cx-docs-dldr\aos-cx-docs-dldr.ps1"
Set-ExecutionPolicy -Scope CurrentUser RemoteSigned
Unblock-File "$HOME\Applications\aos-cx-docs-dldr\aos-cx-docs-dldr.ps1"
. "$HOME\Applications\aos-cx-docs-dldr\aos-cx-docs-dldr.ps1"
```

Check the current session before making the shortcut permanent:

```powershell
aos-cx-docs-dldr --version
```

The printed application version should match the release you downloaded. The
shortcut defaults to a specific image tag; always use the script supplied with
your image. An existing `AOSCX_DOCS_IMAGE` override takes precedence, so update
or remove it when switching releases.

To load the command in future PowerShell sessions, add the same dot-source line
to your profile once:

```powershell
if (-not (Test-Path $PROFILE)) { New-Item -ItemType File -Path $PROFILE -Force | Out-Null }
$line = '. "$HOME\Applications\aos-cx-docs-dldr\aos-cx-docs-dldr.ps1"'
if (-not (Select-String -Path $PROFILE -SimpleMatch $line -Quiet)) { Add-Content $PROFILE $line }
```

### Step 4 — First run on Windows

Start guided selection:

```powershell
aos-cx-docs-dldr
```

Your downloads are saved to `%USERPROFILE%\Documents\AOS-CX`. To use a
different folder, set `AOSCX_DOCS_LIBRARY` before running the command.

> Progress messages come from inside the container, so they show paths like
> `/library/6300/...`. The shortcut prints your real Windows folder when the
> run finishes.

---

## Update the application

1. Check the [latest release](https://github.com/ib0ndar/aos-cx-docs-dldr/releases/latest)
   and read its release notes for compatibility changes.
2. Follow the installation steps again with files from that release. On macOS,
   extract the complete bundle into its own folder. On Windows, load the new
   image, replace the PowerShell shortcut, and dot-source it again (or open a
   new PowerShell session if it is already in your profile).
3. Run `--version` to confirm the version being used.
4. Choose a new library destination when changing application versions. On
   macOS, use `--destination`; on Windows, set `AOSCX_DOCS_LIBRARY` before
   running the shortcut. Then download the guides you need again.

There is no automatic library migration. Existing libraries remain browsable
through their `index.html`; see [Libraries from other versions](#libraries-from-other-versions)
for compatibility details. Libraries and caches created under the former
product name also require new locations; incompatible state is left untouched.

---

## Everyday use

Examples below use the native macOS command. On Windows, use
`aos-cx-docs-dldr` without `./`; the wrapper automatically supplies the
download destination, so omit `--destination` there.

### Guided selection

```sh
./aos-cx-docs-dldr --destination "$HOME/Documents/AOS-CX"
```

It asks for a platform, a release, which guides you want, whether to prefer a
publisher PDF, and whether to generate PDFs from HTML. Use the arrow keys,
space to tick items, Enter to confirm, and Left Arrow to go back a step.

### See what is currently available

```sh
./aos-cx-docs-dldr --list
```

Listing refreshes the live Product Documentation catalogue. After choosing an
exact platform and release from that output, inspect per-guide availability:

```sh
./aos-cx-docs-dldr --list --platform PLATFORM --release RELEASE
```

Do not add `--destination` to a listing command. A guide shown as unavailable
could not be resolved at its mapped publisher route and is not downloaded.

### Download guides

`PLATFORM`, `RELEASE`, and `GUIDE_ID` below are placeholders to replace with
exact values from the current listing. Download one guide by its exact ID:

```sh
./aos-cx-docs-dldr \
  --platform PLATFORM \
  --release RELEASE \
  --guides "GUIDE_ID" \
  --destination "$HOME/Documents/AOS-CX"
```

Download every currently resolved guide and also create a verified ZIP:

```sh
./aos-cx-docs-dldr \
  --platform PLATFORM \
  --release RELEASE \
  --all --zip \
  --destination "$HOME/Documents/AOS-CX"
```

`--destination` is a base folder. The tool appends `<platform>/<release>/`, so
many platforms and releases can coexist.

### Choose PDF behavior

Complete archived HTML is preferred by default. `--prefer-pdf` instead prefers
a source-verified whole-document publisher PDF when one is available. It does
not create a PDF. Append `--prefer-pdf` to a download command to select it.

`--convert-html-to-pdf` creates an additional PDF from complete archived HTML
using the packaged renderer. Append it to a download command; the HTML remains
the canonical offline copy.

### Stop and resume

Press Ctrl-C. Accepted guides and prior complete guides are published, verified
raw cache entries are retained, and the batch is marked incomplete. Run the
same download again to reuse valid cached data.

---

## Output and library lifecycle

```text
your-chosen-folder/
  6300/                          <- platform
    10.18.xxxx/                  <- release; open index.html here
      index.html                 <- contents page for the whole release
      manifest.json              <- what was downloaded, and from where
      history.json
      search-index.js
      search.js
      jobscheduler/              <- one folder per guide
        index.html
        pages/
        assets/
        manifest.json
        search.json
    .<version>.lock              <- housekeeping; leave these alone
    .<version>.transaction.json
    .staging/
    .incomplete/
    .snapshots/
```

The dot-prefixed entries are the tool's own bookkeeping. Do not edit or delete
them while a download is running.

Guides that come as a publisher PDF are saved as that single PDF file. Guides
built from HTML get the browsable folder shown above.

The release-level search covers titles and archived HTML text. Publisher PDFs
remain title-only. Cross-guide links are localized only when the application
can verify one accepted target; ambiguous or unverifiable links remain online.

Downloaded web responses are cached separately, outside your library, so
repeating or resuming a download does not re-fetch everything. The cache is
disposable; deleting it only costs you download time.

Guide updates are transactional within library publication. If a guide update
fails, it never replaces that guide's prior complete copy; other accepted
guides in the batch can still be published.

### Libraries from other versions

This application reads and writes libraries created by the same application
version only. If you point it at a folder created by a different application
version, it refuses and leaves that folder untouched. Choose a new folder and
download again.

A library is also tied to the runtime platform that produced it: native macOS
or the Linux container used on Windows. Integrity digests include Unix
permissions and generated-PDF records include platform sidecar pins, so the two
libraries are not interchangeable. Choose a new destination and download
again.

---

## Command reference

| Option | What it does |
| --- | --- |
| `--help`, `-h` | Show complete command help. Works offline. |
| `--version`, `-V` | Print the application version. Works offline. |
| `--list` | Refresh and show the Product Documentation catalogue, optionally for one platform and release. |
| `--platform` | Switch family, for example `6300`. |
| `--release` | AOS-CX documentation release label, not the application version. |
| `--guides` | Guide IDs to download, space separated. |
| `--all` | Download every resolved mapped guide that is not definitively unavailable. |
| `--destination` | Base folder for output. Fixed automatically on Windows. |
| `--zip` | Also produce a verified ZIP of the release folder. |
| `--refresh` | Revalidate cached guide bytes; the catalogue is always refreshed. |
| `--prefer-pdf` / `--no-prefer-pdf` | Prefer a verified publisher PDF, or complete HTML. |
| `--convert-html-to-pdf` / `--no-convert-html-to-pdf` | Create a companion PDF from HTML, or keep the default HTML-only behavior. |
| `--workers` | How many downloads run at once. Default 4. |
| `--raw-cache` | Use a different cache folder. |
| `--transport compatible\|http` | Browser-free request mode. Leave at the compatible default. |
| `--json` | Print machine-readable output instead of a human summary. |

Run `--help` for timeouts, retries and size limits.

### Exit codes

| Code | Meaning |
| ---: | --- |
| `0` | The requested operation completed successfully. |
| `1` | Invalid input or a fatal error prevented completion. |
| `2` | The result was incomplete or degraded; usable content may still have been listed or published. Inspect diagnostics and the manifest. |
| `130` | The operation was cancelled; accepted work and verified cache entries were retained. |

---

## Troubleshooting

### A guide is shown as unavailable

The guide could not be resolved at its mapped publisher route for that release,
so the tool does not guess another source or produce a partial copy.

### An existing library was rejected

The tool writes only to current, recognized state that it created. Point
`--destination` at a new, empty folder; unsupported state is left untouched.

### The cache folder was rejected

Use an empty folder, or the one the tool created previously. It will not write
into a folder it does not recognise, so that it cannot damage unrelated files.

### A download was interrupted

Just run the same command again. Leave the `.staging`, `.snapshots` and cache
folders in place; the tool checks and resumes from them safely.

### PDF creation is not available

On macOS, the package folder must be intact: the Chrome and qpdf components sit
beside the executable and are found nowhere else. In the container they are
built in. There is no automatic download of components.

### macOS refuses to open the application

Verify the checksums and follow the narrow bundle approval in
[Install on macOS](#install-on-macos). If macOS reports damaged files, an
invalid signature, or malware rather than quarantine, stop and investigate the
exact message.

### Nothing downloads and it mentions robots or a proxy

The tool stops rather than guessing if it cannot confirm it is allowed to
fetch. Behind a company proxy, see
[Environment variables](#environment-variables).

### Behind a company proxy

On native macOS, set proxy variables in the shell that launches the application:

```sh
HTTPS_PROXY='http://proxy.example.com:8080' \
NO_PROXY='localhost,127.0.0.1' \
./aos-cx-docs-dldr --list
```

The Windows shortcut does not forward PowerShell proxy or CA variables. Call
Docker directly and pass them explicitly. If the proxy inspects encrypted
traffic, mount its CA file because the Linux container does not read the
Windows certificate store. Replace `VERSION` below with the application version
of the image you loaded, without the leading `v`:

```powershell
$image = 'aos-cx-docs-dldr:VERSION'
docker run --rm -i `
  -v aos-cx-docs-dldr-cache:/cache `
  -v "C:\certs\corporate-ca.pem:/certs/ca.pem:ro" `
  -e SSL_CERT_FILE=/certs/ca.pem `
  -e HTTPS_PROXY=$env:HTTPS_PROXY `
  -e NO_PROXY=$env:NO_PROXY `
  $image --list
```

---

## Environment variables

None are required. Most are application settings that work on both native
macOS and inside the Linux container. The Windows shortcut has three additional
wrapper-only settings.

On macOS, set application variables in the shell before running
`./aos-cx-docs-dldr`. For a direct container command, pass each application
variable with `docker run -e NAME=value` or the equivalent engine option.

### Application: network

These are read by the application on macOS and inside the container. Both
request modes honour them.

| Variable | Effect |
| --- | --- |
| `HTTPS_PROXY` / `https_proxy` | Proxy for encrypted traffic. All publisher traffic is encrypted. |
| `HTTP_PROXY` / `http_proxy` | Proxy for plain traffic. |
| `NO_PROXY` / `no_proxy` | Comma-separated hosts that bypass the proxy. |
| `SSL_CERT_FILE` | Certificate authority file used by the macOS or Linux application. |
| `SSL_CERT_DIR` | Directory of certificate authority files used by the macOS or Linux application. |

The Windows shortcut does not forward these variables from PowerShell. Use a
direct container command and `-e`, as shown in the proxy example above. A CA
file must also be mounted into the container before `SSL_CERT_FILE` can name
its container path.

### Application: appearance

| Variable | Effect |
| --- | --- |
| `NO_COLOR` | Any value turns colour off. |
| `TERM` | Set to `dumb` to turn off colour and progress animation. |
| `COLORTERM` | Chooses the colour range when colour is on. |

Output sent to a file or another program is always plain, whatever these say.

### Application: files and PDF components

| Variable | Effect |
| --- | --- |
| `XDG_CACHE_HOME` | Base cache folder on systems that use it, including the container. |
| `HOME` | Supplies the macOS user cache location, Linux fallback, and `~` expansion. |
| `AOSCX_DOCS_CHROME_PATH` | Alternative location of the exact pinned Chrome component. |
| `AOSCX_DOCS_QPDF_PATH` | Alternative location of the exact pinned qpdf component. |

`--raw-cache` overrides the cache location and wins over the variables above.
The packaged Chrome and qpdf components are selected automatically; their path
overrides are intended for development and diagnostics, not normal use.

The container already sets `XDG_CACHE_HOME=/cache`,
`HOME=/home/nonroot`, and `SSL_CERT_FILE` to the public certificate bundle.
Change those only if you know why.

### Windows shortcut only

These three variables are read by `scripts/aos-cx-docs-dldr.ps1`, not by the
application. Set them in PowerShell before running `aos-cx-docs-dldr`.

| Variable | Default | Effect |
| --- | --- | --- |
| `AOSCX_DOCS_LIBRARY` | `%USERPROFILE%\Documents\AOS-CX` | Windows folder where downloads are saved. |
| `AOSCX_DOCS_ENGINE` | `docker` | Container command. The `podman` override exists but is unverified. |
| `AOSCX_DOCS_IMAGE` | Versioned image tag pinned in the downloaded shortcut | Container image to run, including a company-registry image. |

No other PowerShell environment variables are forwarded by the shortcut.

---

## Limits and current status

The tool is honest about what it cannot do rather than guessing:

- It never falls back to a web browser, and stops if it cannot confirm that
  fetching is permitted.
- Live catalogue operations cover the Product Documentation surface, not every
  AOS-CX portal surface or historical document.
- Guides unavailable at their resolved mapped routes are reported, not replaced
  with guessed alternatives.
- Direct PDF, Flare HTML, HPE multipage HTML, and explicit complete
  static/Oxygen plans are implemented. Static/Oxygen is fixture-verified only;
  the 2026-09-16 production snapshot contained no current static mappings.
- PDF creation from HTML is optional and never changes the HTML, which stays
  the authoritative copy.

**Two deliverables, nothing else.** A native macOS Apple Silicon bundle and a
Linux container for 64-bit Intel/AMD. Both include PDF creation. There is no
native Windows program, no Linux native package, no Intel Mac build, and no
ARM container.

**Why no ARM container.** The pinned qpdf distribution has no Linux ARM binary,
so an ARM container cannot carry the required component. See
[container details](docs/container.md#what-is-inside) for exact component pins.

**Windows status.** Image loading, the PowerShell shortcut, library writes,
generated-PDF validation, and library reopening have been verified on Windows
11 x64 with Docker Desktop's WSL 2 backend. The runs used a fixture portal;
live publisher retrieval from Windows, Podman Desktop, and other Windows
releases have not been tested. See [docs/container.md](docs/container.md) for
the exact tested environment and verification record.

---

## For maintainers

Development requires Go 1.26 or newer. The architecture and platform documents
below describe the application boundaries and packaging workflows.

A bare `go build` creates only the executable, so generated-PDF conversion
fails closed without its packaged sidecars. Build a complete development bundle:

```sh
./scripts/build-bundle.sh
```

For release assembly, sidecar acquisition, container packaging, and complete
validation commands, use the authoritative documents below rather than this
README. Ordinary tests make no publisher requests; live publisher traffic
requires explicit authorization.

Keep this README focused on installation, everyday use, and current behavior.
Record version-specific changes and one-time upgrade notices in the changelog
and release notes. A version bump alone should not require editing the README;
update it when supported platforms, installation steps, options, or compatibility
rules change. Keep the released PowerShell shortcut's default image tag aligned
with its container image.

### Further reading

- [`docs/container.md`](docs/container.md) — container workflow in detail
- [`docs/go-architecture.md`](docs/go-architecture.md) — how the application is built
- [`docs/native-macos-arm64-release.md`](docs/native-macos-arm64-release.md) — macOS packaging
- [`CHANGELOG.md`](CHANGELOG.md)

---

## License

The original aos-cx-docs-dldr source code, scripts, and project documentation are
licensed under the [Apache License, Version 2.0](LICENSE) (`Apache-2.0`), except
where a file or directory identifies a different license.

Third-party code and bundled software, including `third_party/json5`, Chrome,
qpdf, Go dependencies, and container system packages, retain their own licenses
and notices. The macOS release includes `THIRD_PARTY_NOTICES`; sidecars retain
their upstream license files, and container system-package copyright files are
under `/usr/share/doc`. Downloaded publisher documentation is not licensed by
this project and remains subject to its publisher's terms.

The application `LICENSE` is included beside the executable and at
`/opt/aos-cx-docs-dldr/LICENSE` in the container.
