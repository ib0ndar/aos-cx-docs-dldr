#!/bin/sh
# Build a runnable macOS arm64 bundle: the application plus its checksum
# verified Chrome and qpdf sidecars, laid out so generated-PDF conversion
# works.
#
# A bare `go build` produces only the executable, and generated-PDF conversion
# then fails closed because the sidecars are resolved relative to the
# executable and are never searched for on PATH. Use this script instead when
# you want a build you can actually convert with.
#
# By default it finishes by running a real pinned-Chrome conversion, so a
# successful run means conversion genuinely works rather than merely that the
# files are present. Pass --no-smoke to skip that check.
#
# This produces a development bundle, not a release. It has no package
# manifest, SBOM, notices, checksums, ZIP or provenance, and it does not verify
# repository source identity. Cut a release with scripts/package-macos-arm64.sh.
set -eu
umask 022

usage() {
	echo "usage: $0 [--sidecar-source DIR] [--output DIR] [--smoke]" >&2
	echo "" >&2
	echo "  --sidecar-source  directory holding sidecars/ and SHA256SUMS" >&2
	echo "                    (default: \$AOSCX_DOCS_SIDECAR_SOURCE, then" >&2
	echo "                    ~/Library/Application Support/aos-cx-docs-dldr/sidecar-source)" >&2
	echo "  --output          bundle destination (must not exist)" >&2
	echo "  --no-smoke        skip the real pinned-Chrome conversion check" >&2
	exit 2
}

SIDECAR_SOURCE=${AOSCX_DOCS_SIDECAR_SOURCE:-}
OUTPUT=
SMOKE=1
while [ "$#" -gt 0 ]; do
	case $1 in
	--sidecar-source) SIDECAR_SOURCE=${2-}; shift 2 ;;
	--output) OUTPUT=${2-}; shift 2 ;;
	--no-smoke) SMOKE=0; shift ;;
	*) usage ;;
	esac
done

if [ "$(uname -s)" != Darwin ] || [ "$(uname -m)" != arm64 ]; then
	echo "aos-cx-docs-dldr bundle assembly is supported only on macOS arm64" >&2
	exit 1
fi
for tool in go sed; do
	command -v "$tool" >/dev/null 2>&1 || { echo "required tool not found: $tool" >&2; exit 1; }
done

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPOSITORY=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)

APP_VERSION=$(sed -n 's/^const Version = "\(.*\)"$/\1/p' "$REPOSITORY/internal/model/model.go")
[ -n "$APP_VERSION" ] || { echo "could not determine application version" >&2; exit 1; }

[ -n "$SIDECAR_SOURCE" ] || SIDECAR_SOURCE="$HOME/Library/Application Support/aos-cx-docs-dldr/sidecar-source"
if [ ! -d "$SIDECAR_SOURCE/sidecars" ] || [ ! -f "$SIDECAR_SOURCE/SHA256SUMS" ]; then
	echo "No sidecar source at: $SIDECAR_SOURCE" >&2
	echo "It must contain sidecars/chrome-headless-shell-mac-arm64," >&2
	echo "sidecars/qpdf-mac-arm64 and a matching SHA256SUMS." >&2
	echo "Acquire one with scripts/acquire-macos-arm64-sidecars.sh, or point" >&2
	echo "--sidecar-source at an existing verified copy." >&2
	exit 1
fi
SIDECAR_SOURCE=$(CDPATH= cd -- "$SIDECAR_SOURCE" && pwd)
CHROME_SOURCE="$SIDECAR_SOURCE/sidecars/chrome-headless-shell-mac-arm64"
QPDF_SOURCE="$SIDECAR_SOURCE/sidecars/qpdf-mac-arm64"

[ -n "$OUTPUT" ] || OUTPUT="$REPOSITORY/build/aos-cx-docs-dldr-$APP_VERSION-macos-arm64"
case $OUTPUT in /*) ;; *) OUTPUT=$PWD/$OUTPUT ;; esac
if [ -e "$OUTPUT" ] || [ -L "$OUTPUT" ]; then
	echo "Refusing to replace an existing bundle: $OUTPUT" >&2
	echo "Remove it first, or pass a different --output." >&2
	exit 1
fi

WORK=$(mktemp -d "${TMPDIR:-/tmp}/aos-cx-docs-dldr-bundle.XXXXXXXX")
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT INT TERM

RELEASE_TOOL="$WORK/aoscx-release-package"
mkdir -p "$OUTPUT"

(
	cd "$REPOSITORY"
	export GOENV=off GOFLAGS= GOEXPERIMENT= GOTOOLCHAIN=local GOWORK=off
	export CGO_ENABLED=0 GOOS=darwin GOARCH=arm64

	go build -trimpath -o "$RELEASE_TOOL" ./cmd/aoscx-release-package

	# Confirm the sidecar inputs still match their recorded checksums before
	# anything is copied into the bundle.
	"$RELEASE_TOOL" verify-source \
		--root "$SIDECAR_SOURCE" \
		--checksums "$SIDECAR_SOURCE/SHA256SUMS" \
		--required "$CHROME_SOURCE" \
		--required "$QPDF_SOURCE" \
		--output "$WORK/input-verification.json"

	go build -trimpath -o "$OUTPUT/aos-cx-docs-dldr" ./cmd/aos-cx-docs-dldr
	cp "$REPOSITORY/LICENSE" "$OUTPUT/LICENSE"

	AOSCX_RELEASE_TOOL="$RELEASE_TOOL" \
		"$SCRIPT_DIR/stage-chrome-sidecar.sh" "$CHROME_SOURCE" "$OUTPUT/sidecars"
	AOSCX_RELEASE_TOOL="$RELEASE_TOOL" \
		"$SCRIPT_DIR/stage-qpdf-sidecar.sh" "$QPDF_SOURCE" "$OUTPUT/sidecars"

	# Re-verify identity, architecture and the qpdf dependency closure in
	# their final executable-relative position.
	"$RELEASE_TOOL" verify-sidecars \
		--chrome "$OUTPUT/sidecars/chrome-headless-shell-mac-arm64" \
		--qpdf "$OUTPUT/sidecars/qpdf-mac-arm64"
)

VERSION_OUTPUT=$("$OUTPUT/aos-cx-docs-dldr" --version)
if [ "$VERSION_OUTPUT" != "aos-cx-docs-dldr $APP_VERSION" ]; then
	echo "Bundle reports unexpected version: $VERSION_OUTPUT" >&2
	exit 1
fi

if [ "$SMOKE" -eq 1 ]; then
	echo "Running pinned-Chrome conversion smoke..."
	SMOKE_BINARY="$WORK/pdfgen-smoke.test"
	SENTINEL="$WORK/smoke.complete"
	(
		cd "$REPOSITORY"
		export GOENV=off GOFLAGS= GOTOOLCHAIN=local GOWORK=off
		export CGO_ENABLED=0 GOOS=darwin GOARCH=arm64
		go test -c -tags chromepdftests -o "$SMOKE_BINARY" ./internal/pdfgen
	)
	cp "$SMOKE_BINARY" "$OUTPUT/.bundle-smoke.test"
	# Clearing the override variables proves the sidecars are found
	# executable-relative rather than through an explicit path.
	env -u AOSCX_DOCS_CHROME_PATH -u AOSCX_DOCS_QPDF_PATH \
		AOSCX_WP11_PACKAGE_SMOKE=1 \
		AOSCX_WP13_PACKAGE_SMOKE_SENTINEL="$SENTINEL" \
		"$OUTPUT/.bundle-smoke.test" \
		-test.run '^TestPinnedSidecarsExecutableRelativeConversion$' \
		-test.count=1
	grep -Fqx 'complete' "$SENTINEL"
	rm -f "$OUTPUT/.bundle-smoke.test"
	echo "Conversion smoke passed."
fi

echo
echo "Bundle ready: $OUTPUT"
echo "  $VERSION_OUTPUT"
echo "  sidecars verified: chrome-headless-shell-mac-arm64, qpdf-mac-arm64"
echo
echo "Run it with:"
echo "  \"$OUTPUT/aos-cx-docs-dldr\" --help"
echo
echo "Move the whole directory if you relocate it; the sidecars are found"
echo "relative to the executable."
