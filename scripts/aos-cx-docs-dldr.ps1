# aos-cx-docs-dldr container wrapper for Windows PowerShell.
#
# Dot-source this file from your PowerShell profile, or paste the function into
# it:
#
#     . C:\path\to\aos-cx-docs-dldr.ps1
#
# Then use the tool as if it were installed natively:
#
#     aos-cx-docs-dldr --list --platform 6300 --release 10.18.xxxx
#     aos-cx-docs-dldr --platform 6300 --release 10.18.xxxx --all
#     aos-cx-docs-dldr                # guided flow, destination already fixed
#
# The published library appears in a normal Windows folder. Override the
# location with $env:AOSCX_DOCS_LIBRARY, and the container engine with
# $env:AOSCX_DOCS_ENGINE (docker or podman).
#
# See docs/container.md for the full workflow and its limitations.

function aos-cx-docs-dldr {
    [CmdletBinding()]
    param([Parameter(ValueFromRemainingArguments = $true)] $CliArgs)

    # ValueFromRemainingArguments yields $null when the caller passes nothing,
    # and splatting $null into a native command is not reliable. Normalise to an
    # array so the no-argument guided flow works.
    $userArgs = @($CliArgs | Where-Object { $null -ne $_ })

    $engine = if ($env:AOSCX_DOCS_ENGINE) { $env:AOSCX_DOCS_ENGINE } else { 'docker' }
    $image  = if ($env:AOSCX_DOCS_IMAGE)  { $env:AOSCX_DOCS_IMAGE }  else { 'aos-cx-docs-dldr:0.8' }
    # USERPROFILE is always set on Windows; HOME covers PowerShell on other
    # systems, where this wrapper is also handy for testing. Join-Path is
    # nested rather than given three arguments so the script stays valid on
    # Windows PowerShell 5.1, which lacks -AdditionalChildPath.
    $profileDir = if ($env:USERPROFILE) { $env:USERPROFILE } else { $HOME }
    $library = if ($env:AOSCX_DOCS_LIBRARY) {
        $env:AOSCX_DOCS_LIBRARY
    } else {
        Join-Path (Join-Path $profileDir 'Documents') 'AOS-CX'
    }

    if (-not (Get-Command $engine -ErrorAction SilentlyContinue)) {
        Write-Error "$engine was not found on PATH. Install Docker Desktop or Podman Desktop first."
        $global:LASTEXITCODE = 1
        return
    }

    # Create the library folder up front so the bind mount maps to a real
    # directory rather than letting the engine create a root-owned one.
    New-Item -ItemType Directory -Force -Path $library | Out-Null

    # Allocate a TTY only for genuinely interactive use. With redirected output
    # the application already emits plain, control-free text, and forcing a TTY
    # would reintroduce escape sequences into a captured stream.
    $ttyFlags = if ([Console]::IsOutputRedirected) { @('-i') } else { @('-i', '-t') }

    # --list is catalogue-only and the CLI rejects it alongside --destination,
    # so the pinned destination must be omitted for listing runs.
    $listing = $userArgs -contains '--list'
    $fixed = if ($listing) { @() } else { @('--destination', '/library') }

    $runArgs = @('run', '--rm') + $ttyFlags +
        @('-v', "${library}:/library", '-v', 'aos-cx-docs-dldr-cache:/cache', $image) +
        $fixed + $userArgs

    # The engine's stdout flows straight through to the caller, and its exit
    # code is left in $LASTEXITCODE exactly as a native command would leave it.
    # Nothing is returned, so `$x = aos-cx-docs-dldr --json ...` captures only the
    # application's output.
    & $engine @runArgs
    $code = $LASTEXITCODE

    if (-not $listing -and -not [Console]::IsOutputRedirected) {
        # Progress and result output name the container path, so translate it
        # back to the location the user actually browses. Write-Host goes to
        # the console, not the output stream.
        Write-Host ""
        Write-Host "Library folder: $library"
    }
    $global:LASTEXITCODE = $code
}
