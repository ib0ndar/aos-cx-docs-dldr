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
$HOME/Documents/AOS-CX/6300/10.18/index.html           <- open this
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
| Linux | The same container under Docker Engine | 64-bit Intel/AMD (`linux/amd64`) | Verified on the environment described below |

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
- [Install on Linux](#install-on-linux)
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
| Release | An AOS-CX documentation release as the portal lists it, such as `10.18`. Treated as text, not a number. |
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

The bundle is unsigned and unnotarized. Browser downloads give every extracted
file the macOS quarantine attribute, and Gatekeeper then asks about each
quarantined program and library separately, sometimes with a dialog suggesting
moving the file to Trash. Choose one of two ways to allow the bundle, after
**both checksum checks above pass** and only if you trust this release.

**Approve the application once.** Run `./aos-cx-docs-dldr --version` from the
extracted bundle directory. When macOS refuses it, choose **Open Anyway** for
`aos-cx-docs-dldr` in **System Settings > Privacy & Security** and run it
again. No other approval is needed: before a download that creates PDFs, the
application checks every file of its bundled Chrome and qpdf against the
SHA-256 values built into it, then removes the quarantine from their programs
and libraries itself and says how many files it released. It never changes a
file that fails that check, PDF tools named by `--chrome-path`, `--qpdf-path`
or their environment variables, or anything else on disk.

**Or remove the quarantine from the whole bundle** before the first launch, so
macOS asks nothing. Run this while still inside the extracted bundle directory:

```sh
xattr -dr com.apple.quarantine "$PWD"
./aos-cx-docs-dldr --version
```

`-d` removes only the named quarantine attribute; `-r` applies recursively to
the entire bundle, including its sidecars and libraries. It does not sign or
notarize the software, create a permanent exception, or disable Gatekeeper
globally. No `sudo` is normally needed for a bundle extracted into your own
folder. It reports `Permission denied` for qpdf's four read-only files, because
macOS changes no file's attributes without write permission; the application
releases those itself before its first PDF conversion.

Move the **whole folder** somewhere permanent, for example
`$HOME/Applications/aos-cx-docs-dldr/VERSION/`. Moving only the executable will break
PDF creation. If you already installed the bundle there, use its exact path:

```sh
xattr -dr com.apple.quarantine "$HOME/Applications/aos-cx-docs-dldr/${APP_VERSION}"
```

Apply this only to the verified bundle, never to your Downloads folder, home
directory, or the whole disk. Repeat for a newly downloaded replacement bundle
after verifying its checksums.

If the application cannot remove the quarantine from its PDF tools itself, for
example from a bundle another account installed, it stops before downloading
anything and prints the exact command for those folders, such as
`sudo xattr -dr com.apple.quarantine '/Applications/aos-cx-docs-dldr/VERSION/sidecars/qpdf-mac-arm64'`.
Run it and start the download again. If macOS still blocks a file, or
explicitly reports malware, stop and investigate the exact message; quarantine
removal does not fix damaged files, invalid signatures, or malware detections.

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

## Install on Linux

On Linux the tool runs in the same container as on Windows, which includes
everything, including PDF creation. You do not need to install Go or compile
anything. The host must be 64-bit Intel/AMD (`x86_64`); ARM Linux is not
supported. The documented route is Docker Engine in its default setup, where
the engine runs as root. See [Linux status](#limits-and-current-status) for the
verification scope.

### Step 1 — Install Docker Engine

Install Docker Engine from Docker's
[installation guide](https://docs.docker.com/engine/install/) or from your
distribution's packages (`docker.io` on Debian and Ubuntu), and start it. Then
allow your account to run `docker` without `sudo`, and log out and back in:

```sh
sudo usermod -aG docker "$USER"
```

Members of the `docker` group control the engine, which amounts to root access;
see Docker's
[post-installation steps](https://docs.docker.com/engine/install/linux-postinstall/).
After logging back in, `docker version` must show both a client and a server.

Rootless Docker, Docker Desktop for Linux and Podman map user IDs differently
and have not been tested.

### Step 2 — Load the image

Download `aos-cx-docs-dldr-VERSION-linux-amd64.tar.gz` and `SHA256SUMS` from
your chosen [release](https://github.com/ib0ndar/aos-cx-docs-dldr/releases/latest).
In a terminal, change to the download folder. Set `APP_VERSION` once,
replacing `VERSION` as described above, then check the archive:

```sh
APP_VERSION='VERSION'
sha256sum -c SHA256SUMS
```

Stop unless it prints `OK`. Then load the archive; `docker load` handles the
compressed file directly:

```sh
docker load -i "aos-cx-docs-dldr-${APP_VERSION}-linux-amd64.tar.gz"
```

### Step 3 — Install the shortcut command

The shortcut is a small script that lets you type `aos-cx-docs-dldr` instead of
a long `docker run` line. It runs the container with your user and group IDs,
so the library and cache it writes in your home folder belong to you, and
without privileges. In the same terminal, because it uses `APP_VERSION`,
create it in `~/.local/bin`:

```sh
mkdir -p "$HOME/.local/bin"
cat > "$HOME/.local/bin/aos-cx-docs-dldr" <<'EOF'
#!/bin/sh
# aos-cx-docs-dldr in its container, writing to your folders as you.
image=${AOSCX_DOCS_IMAGE:-aos-cx-docs-dldr:VERSION}
library=${AOSCX_DOCS_LIBRARY:-$HOME/Documents/AOS-CX}
cache=${XDG_CACHE_HOME:-$HOME/.cache}/aos-cx-docs-dldr-container
# Create both folders first: Docker would create missing ones owned by root.
mkdir -p "$library" "$cache" || exit 1
# --list takes no destination; every other run saves to the library.
listing=
for arg in "$@"; do
    [ "$arg" = --list ] && listing=1
done
[ -n "$listing" ] || set -- --destination /library "$@"
# A terminal only when both input and output are one, so that redirected
# output stays plain.
tty=
[ -t 0 ] && [ -t 1 ] && tty=-t
docker run --rm -i $tty --cap-drop ALL --security-opt no-new-privileges \
    --user "$(id -u):$(id -g)" \
    -v "$library:/library" -v "$cache:/cache" \
    -e HTTPS_PROXY -e HTTP_PROXY -e NO_PROXY \
    -e https_proxy -e http_proxy -e no_proxy \
    "$image" "$@"
status=$?
if [ -z "$listing" ] && [ -t 1 ]; then
    printf '\nLibrary folder: %s\n' "$library" >&2
fi
exit "$status"
EOF
sed -i "s/aos-cx-docs-dldr:VERSION/aos-cx-docs-dldr:${APP_VERSION}/" "$HOME/.local/bin/aos-cx-docs-dldr"
chmod +x "$HOME/.local/bin/aos-cx-docs-dldr"
```

Check it from the same terminal:

```sh
"$HOME/.local/bin/aos-cx-docs-dldr" --version
```

The printed application version should match the release you downloaded. The
script runs the image of that release; when you update, load the new image and
create the script again. An `AOSCX_DOCS_IMAGE` variable takes precedence, so
update or remove it when switching releases. Run the shortcut as yourself, not
with `sudo`, which would make the files belong to root.

Most distributions put `~/.local/bin` on your `PATH` at login once the folder
exists. If `command -v aos-cx-docs-dldr` still finds nothing after logging in
again, add `export PATH="$HOME/.local/bin:$PATH"` to your shell's startup file.

### Step 4 — First run on Linux

Start guided selection:

```sh
aos-cx-docs-dldr
```

Your downloads are saved to `$HOME/Documents/AOS-CX`. To use a different
folder, set `AOSCX_DOCS_LIBRARY` before running the command. Open
`$HOME/Documents/AOS-CX/<platform>/<release>/index.html` in a web browser, for
example with `xdg-open`. Downloaded web responses are cached in
`~/.cache/aos-cx-docs-dldr-container`.

> Progress messages come from inside the container, so they show paths like
> `/library/6300/...`. The shortcut prints your real folder when the run
> finishes.

---

## Update the application

1. Check the [latest release](https://github.com/ib0ndar/aos-cx-docs-dldr/releases/latest)
   and read its release notes for compatibility changes.
2. Follow the installation steps again with files from that release. On macOS,
   extract the complete bundle into its own folder. On Windows, load the new
   image, replace the PowerShell shortcut, and dot-source it again (or open a
   new PowerShell session if it is already in your profile). On Linux, load
   the new image and create the shortcut script again.
3. Run `--version` to confirm the version being used.
4. Choose a new library destination when changing application versions. On
   macOS, use `--destination`; on Windows and Linux, set `AOSCX_DOCS_LIBRARY`
   before running the shortcut. Then download the guides you need again.

There is no automatic library migration. Existing libraries remain browsable
through their `index.html`; see [Libraries from other versions](#libraries-from-other-versions)
for compatibility details. Libraries and caches created under the former
product name also require new locations; incompatible state is left untouched.

---

## Everyday use

Examples below use the native macOS command. On Windows and Linux, use
`aos-cx-docs-dldr` without `./`; the shortcut automatically supplies the
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

`--replace-html-with-pdf` creates the same PDF, then removes the guide's HTML
and its folder and keeps the PDF in the release folder. Guides that come from
the publisher as a PDF are kept there too. The HTML is removed only after the
PDF and the HTML it was made from pass verification. The guided selection
offers the same choice as "generate PDFs with pinned Chrome and remove the
HTML".

When every guide of the release ends up as a PDF and nothing failed or was
cancelled, the release folder holds only the PDFs: the release `index.html`,
its search files, `manifest.json` and `history.json` are removed too, and so
are the tool's snapshots of earlier copies of that release. Nothing records
the download any more, so the tool no longer recognizes the folder: to
download the release there again, first move or delete the folder, or choose
another `--destination`. With `--json`, the output still carries the details
`manifest.json` would have held, and reports `"pdf_only": true`.

A guide whose PDF fails keeps its HTML in its folder and is reported as
incomplete. The release folder then keeps its index and records, so that
running the same download again can finish it; once it does, only the PDFs
remain. Until then, the converted guides are searchable by title only, and
links to them from other guides stay online.

### Stop and resume

Press Ctrl-C. Accepted guides and prior complete guides are published, verified
raw cache entries are retained, and the batch is marked incomplete. Closing the
terminal, a dropped SSH session, or `kill` stop the run the same way, with exit
code 130. Run the same download again to reuse valid cached data.

---

## Output and library lifecycle

```text
your-chosen-folder/
  6300/                          <- platform
    10.18/                       <- release; open index.html here
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
built from HTML get the browsable folder shown above. With
`--replace-html-with-pdf`, each guide is instead a single PDF file in the
release folder, named `<platform> - <release> - <title>.pdf`, and has no
folder; once every guide is, the release folder holds nothing else (see
[Choose PDF behavior](#choose-pdf-behavior)).

The release-level search covers titles and archived HTML text. Publisher PDFs
remain title-only. Cross-guide links are localized only when the application
can verify one accepted target; ambiguous or unverifiable links remain online.
When a guide later loses a page, or all its HTML to a PDF, links to it from
guides kept from earlier downloads go back online.

Downloaded web responses are cached separately, outside your library, so
repeating or resuming a download does not re-fetch everything. The cache is
disposable; deleting it only costs you download time.

Guide updates are transactional within library publication. If a guide update
fails, it never replaces that guide's prior complete copy; other accepted
guides in the batch can still be published. A degraded copy (see
[A guide is degraded](#a-guide-is-degraded)) never replaces a complete one
either.

### Libraries from other versions

This application reads and writes libraries created by the same application
version only. If you point it at a folder created by a different application
version, it refuses and leaves that folder untouched. Choose a new folder and
download again.

A library is also tied to the runtime platform that produced it: native macOS,
or the Linux container on Windows or on Linux. Integrity digests include Unix
permissions and generated-PDF records include platform sidecar pins, so these
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
| `--guide-source` | `GUIDE_ID=URL`: download that guide from a publisher document you name; see [A guide is shown as unavailable](#a-guide-is-shown-as-unavailable). |
| `--destination` | Base folder for output. Fixed automatically by the Windows and Linux shortcuts. |
| `--zip` | Also produce a verified ZIP of the release folder. |
| `--refresh` | Revalidate cached guide bytes; the catalogue is always refreshed. |
| `--prefer-pdf` / `--no-prefer-pdf` | Prefer a verified publisher PDF, or complete HTML. |
| `--convert-html-to-pdf` / `--no-convert-html-to-pdf` | Create a companion PDF from HTML, or keep the default HTML-only behavior. |
| `--replace-html-with-pdf` | Create a PDF from HTML and keep only the PDFs, in the release folder: no guide folders, and once every guide is a PDF, no index or records either. A guide whose PDF fails keeps its HTML. |
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

When the mapped route answers "not found", the tool still looks where the
publisher's own data points, and says so while checking availability:

- the copy that the platforms sharing this platform's copy of the guide in
  the neighbouring releases all map to in this release;
- for releases 10.13 to 10.16, the publisher's PDF of the guide in the
  release's PDF folder (`AOS-CX/RELEASE/PDF/`), under this platform's mapped
  name if the PDF's own title names the guide, or under its peers' name if
  the title also names the release and this platform.

The first copy the publisher serves is downloaded, and the release's
`manifest.json` records both routes (`catalogue_warnings`, route origin
`publisher-peer-mapping`, `publisher-release-pdf` or
`publisher-peer-release-pdf`).

If you know where the publisher does publish the guide, for example as an
HPE Support Center document, name it for that download:

```sh
./aos-cx-docs-dldr --platform PLATFORM --release RELEASE --all --destination DIR \
  --guide-source GUIDE_ID=URL
```

`URL` must be a document on `arubanetworking.hpe.com` or an HPE Support Center
`docDisplay?docId=…` address; it is checked like any other source and recorded
with route origin `user-supplied`.

### An existing library was rejected

The tool writes only to current, recognized state that it created. Point
`--destination` at a new, empty folder; unsupported state is left untouched.

If the message names a platform, release or housekeeping folder that belongs to
another account or that other users can write to, another user could tamper
with the library through it. Fix the folder's owner or permissions (for
example `chmod go-w FOLDER`) or choose a new `--destination`.

### The cache folder was rejected

Use an empty folder, or the one the tool created previously. It will not write
into a folder it does not recognise, so that it cannot damage unrelated files.

### A download was interrupted

Just run the same command again. Leave the `.staging`, `.snapshots` and cache
folders in place; the tool checks and resumes from them safely.

### Downloads pause with "requests to … paused"

The publisher's site sometimes stops answering for a minute or two, and serves
some files slowly the first time they are requested. A download that keeps
arriving at 1 KiB/s or more is not cut off, however long it takes, and one
still arriving after a minute is reported once a minute. When the site stops
answering, the tool stops sending it requests, waits (30 seconds, then 60
seconds by default) and tries again, so a large guide can take a few minutes longer. A guide that
still fails this way is tried again after the other guides, at least 3 minutes
after it failed, and once more 8 minutes after that; its `manifest.json` entry
records the earlier failure. If it still fails, run the same command again;
everything already downloaded is reused from the cache.

### A guide is degraded

At the end of a download, below `Status:`, the tool says whether every
selected guide is present, whether each has a PDF file (downloaded from the
publisher or converted from the guide's downloaded web pages), and, under
`Missing:`, what each affected guide lacks and why, for example:

```text
Status: degraded
Guides: all 27 selected guides are present: 25 complete, 2 degraded (the publisher does not serve all of their content).
PDF files: all 27 guides have one, converted from their downloaded web pages.
Missing:
  Multicast Guide: 1 topic the publisher does not serve: "Use Case 3: …" (HTTP 404); the contents entry links to the publisher's page
```

A degraded guide was published, but the publisher does not serve everything it links:
an image is shown as a placeholder, or a topic listed in the contents returns
"not found" on the publisher's site. Such a topic's contents entry and links
lead to the publisher's page (marked ↗); the guide's `manifest.json` lists it
under `unpublished_topics`. A degraded copy never replaces a complete one, so
run the download again later; it is replaced once the publisher serves more.

### PDF creation is not available

On macOS, the package folder must be intact: the Chrome and qpdf components sit
beside the executable and are found nowhere else. Every file in their folders is
checked, so an added, removed or changed file disables conversion; only a
Finder `.DS_Store` is tolerated. Keep the bundle on an APFS or Mac OS Extended
volume, because exFAT marks every file executable and adds `._` files. In the
container the components are built in. There is no automatic download of
components.

### A source address was refused

The tool contacts only public HTTP and HTTPS addresses. A catalogue entry, link
or redirect that points to `localhost` or to a private, link-local or other
non-public address is refused and named in the message rather than fetched.
For plain HTTP without a proxy, the address the name resolves to is checked
too. A redirect from HTTPS to plain HTTP is also refused.

### macOS refuses to open the application

Verify the checksums and follow the narrow bundle approval in
[Install on macOS](#install-on-macos). If macOS reports damaged files, an
invalid signature, or malware rather than quarantine, stop and investigate the
exact message.

### A download stops with "macOS keeps the download quarantine"

The application could not remove the quarantine from its bundled Chrome and
qpdf, or they were named by `--chrome-path` or `--qpdf-path`, which it never
changes. Nothing was downloaded. Run the `xattr` command the message prints,
which names exactly those PDF tool folders, then start the download again.

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
docker run --rm -i --cap-drop ALL --security-opt no-new-privileges `
  -v aos-cx-docs-dldr-cache:/cache `
  -v "C:\certs\corporate-ca.pem:/certs/ca.pem:ro" `
  -e SSL_CERT_FILE=/certs/ca.pem `
  -e HTTPS_PROXY=$env:HTTPS_PROXY `
  -e NO_PROXY=$env:NO_PROXY `
  $image --list
```

The Linux shortcut forwards `HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY`, in
upper or lower case, when they are set in your shell. A proxy listening on the
host's own `localhost` cannot be reached from inside the container; use an
address the container can reach. If the proxy inspects encrypted traffic, call
Docker directly and mount its CA file, as on Windows. The cache folder must
exist first, or Docker creates it owned by root:

```sh
image='aos-cx-docs-dldr:VERSION'
cache="${XDG_CACHE_HOME:-$HOME/.cache}/aos-cx-docs-dldr-container"
mkdir -p "$cache"
docker run --rm -i --cap-drop ALL --security-opt no-new-privileges \
  --user "$(id -u):$(id -g)" \
  -v "$cache:/cache" \
  -v /path/to/corporate-ca.pem:/certs/ca.pem:ro \
  -e SSL_CERT_FILE=/certs/ca.pem \
  -e HTTPS_PROXY -e NO_PROXY \
  "$image" --list
```

---

## Environment variables

None are required. Most are application settings that work on both native
macOS and inside the Linux container. The Windows and Linux shortcuts have a
few settings of their own.

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
its container path. The Linux shortcut forwards the proxy variables when they
are set, but not `SSL_CERT_FILE` or `SSL_CERT_DIR`.

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
overrides are intended for development and diagnostics, not normal use. Chrome
and qpdf receive only a minimal environment (locale and time zone); `DYLD_*`,
`LD_*` and other inherited variables are not passed to them.

The container already sets `XDG_CACHE_HOME=/cache`,
`HOME=/home/nonroot`, and `SSL_CERT_FILE` to the public certificate bundle.
Change those only if you know why.

### Shortcuts only

These variables are read by the Windows shortcut (`scripts/aos-cx-docs-dldr.ps1`)
and the Linux shortcut script, not by the application. Set them before running
`aos-cx-docs-dldr`.

| Variable | Default | Effect |
| --- | --- | --- |
| `AOSCX_DOCS_LIBRARY` | `%USERPROFILE%\Documents\AOS-CX` on Windows, `$HOME/Documents/AOS-CX` on Linux | Folder where downloads are saved. |
| `AOSCX_DOCS_IMAGE` | Versioned image tag pinned in the shortcut | Container image to run, including a company-registry image. |
| `AOSCX_DOCS_ENGINE` | `docker` | Windows only. Container command. The `podman` override exists but is unverified. |

No other PowerShell environment variables are forwarded by the Windows
shortcut. The Linux shortcut forwards only the proxy variables, and keeps the
download cache in `${XDG_CACHE_HOME:-$HOME/.cache}/aos-cx-docs-dldr-container`.

---

## Limits and current status

The tool is honest about what it cannot do rather than guessing:

- It never falls back to a web browser, and stops if it cannot confirm that
  fetching is permitted.
- Live catalogue operations cover the Product Documentation surface, not every
  AOS-CX portal surface or historical document.
- Guides unavailable at their resolved mapped routes are reported, not replaced
  with guessed alternatives. The only replacements come from the publisher's
  own data, or from `--guide-source` (see
  [A guide is shown as unavailable](#a-guide-is-shown-as-unavailable)).
- Releases 10.17 and later are Oxygen WebHelp HTML, 10.13 to 10.16 are Flare
  HTML (the Command Line Interface Guide is a publisher PDF there), and older
  releases are archived publisher PDFs. Oxygen guides have no publisher PDF.
- The catalogue follows the portal's own routing rules. If the publisher
  changes how its portal routes guides, the catalogue refresh stops and says an
  updated aos-cx-docs-dldr is required, rather than guessing new addresses.
- PDF creation from HTML is optional and never changes the HTML, which stays
  the authoritative copy.

**Two deliverables, nothing else.** A native macOS Apple Silicon bundle and a
Linux container for 64-bit Intel/AMD, used on Windows and on Linux. Both
include PDF creation. There is no native Windows program, no Linux native
package, no Intel Mac build, and no ARM container.

**Why no ARM container.** The pinned qpdf distribution has no Linux ARM binary,
so an ARM container cannot carry the required component. See
[container details](docs/container.md#what-is-inside) for exact component pins.

**Windows status.** Image loading, the PowerShell shortcut, library writes,
generated-PDF validation, and library reopening have been verified on Windows
11 x64 with Docker Desktop's WSL 2 backend. The runs used a fixture portal;
live publisher retrieval from Windows, Podman Desktop, and other Windows
releases have not been tested. See [docs/container.md](docs/container.md) for
the exact tested environment and verification record.

**Linux status.** The installation steps above, including the shortcut
script, were run as written on Ubuntu 24.04 x86_64 with Docker Engine 29.1.3
(Ubuntu's `docker.io` package, set to the `cgroupfs` cgroup driver), in a
virtual machine on an Apple Silicon Mac where x86_64 programs run under
Rosetta translation. Against a fixture portal they passed version and help,
listing, a download with generated PDF and ZIP, reopening the library,
redirected `--json` output, a group-writable umask, and library and cache
files owned by the user. Live publisher retrieval, a physical x86_64 machine,
other distributions, SELinux-enforcing hosts, rootless Docker, Docker Desktop
for Linux and Podman have not been tested.

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

Watch long runs, such as live downloads, with `scripts/watch-run.sh`. It
starts the command detached and returns when the command ends, at regular
checkpoints, or when the command stalls with no output and no CPU use; it can
then make the program write its goroutine stacks. `scripts/watch-run.sh --help`
describes it.

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
- [`docs/chrome-sidecar.md`](docs/chrome-sidecar.md) — pinned Chrome component
- [`docs/qpdf-sidecar.md`](docs/qpdf-sidecar.md) — pinned qpdf component
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
