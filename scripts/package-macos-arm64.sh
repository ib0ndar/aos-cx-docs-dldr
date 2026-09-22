#!/bin/sh
set -eu
umask 022

usage() {
	echo "usage: $0 --source-root DIR --source-checksums FILE --chrome-source DIR --qpdf-source DIR DESTINATION" >&2
	exit 2
}

SOURCE_ROOT=
SOURCE_CHECKSUMS=
CHROME_SOURCE=
QPDF_SOURCE=
while [ "$#" -gt 1 ]; do
	case $1 in
	--source-root) SOURCE_ROOT=${2-}; shift 2 ;;
	--source-checksums) SOURCE_CHECKSUMS=${2-}; shift 2 ;;
	--chrome-source) CHROME_SOURCE=${2-}; shift 2 ;;
	--qpdf-source) QPDF_SOURCE=${2-}; shift 2 ;;
	*) usage ;;
	esac
done
[ "$#" -eq 1 ] || usage
[ -n "$SOURCE_ROOT" ] && [ -n "$SOURCE_CHECKSUMS" ] &&
	[ -n "$CHROME_SOURCE" ] && [ -n "$QPDF_SOURCE" ] || usage

if [ "$(uname -s)" != Darwin ] || [ "$(uname -m)" != arm64 ]; then
	echo "aos-cx-docs-dldr bundle assembly is supported only on macOS arm64" >&2
	exit 1
fi

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPOSITORY=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
absolute_existing() {
	parent=$(CDPATH= cd -- "$(dirname -- "$1")" && pwd)
	printf '%s/%s\n' "$parent" "$(basename -- "$1")"
}
snapshot_source_files() {
	output=$1
	(
		cd "$REPOSITORY"
		git ls-files -co --exclude-standard -z |
			while IFS= read -r -d '' source_file; do
				if [ -e "$source_file" ] || [ -L "$source_file" ]; then
					printf '%s\0' "$source_file"
				fi
			done |
			LC_ALL=C sort -z >"$output"
	)
}
SOURCE_ROOT=$(absolute_existing "$SOURCE_ROOT")
SOURCE_CHECKSUMS=$(absolute_existing "$SOURCE_CHECKSUMS")
CHROME_SOURCE=$(absolute_existing "$CHROME_SOURCE")
QPDF_SOURCE=$(absolute_existing "$QPDF_SOURCE")
case $1 in
/*) DESTINATION=$1 ;;
*) DESTINATION=$PWD/$1 ;;
esac
# The application version is authoritative in internal/model/model.go. Derive
# the package identity from it so a version bump cannot leave this script
# asserting a stale name or a stale --version string.
APP_VERSION=$(sed -n 's/^const Version = "\(.*\)"$/\1/p' "$REPOSITORY/internal/model/model.go")
if [ -z "$APP_VERSION" ]; then
	echo "Could not determine application version from internal/model/model.go" >&2
	exit 1
fi
PACKAGE_NAME="aos-cx-docs-dldr-$APP_VERSION-macos-arm64"
if [ -e "$DESTINATION" ] || [ -L "$DESTINATION" ]; then
	echo "Refusing to replace existing release-set destination: $DESTINATION" >&2
	exit 1
fi
for tool in go git cmp sed; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		echo "Required bundle assembly tool is unavailable: $tool" >&2
		exit 1
	fi
done

OUTPUT_PARENT=$(dirname -- "$DESTINATION")
mkdir -p "$OUTPUT_PARENT"
WORK=$(mktemp -d "$OUTPUT_PARENT/.aos-cx-docs-dldr-package.XXXXXXXX")
trap 'rm -rf "$WORK"' EXIT HUP INT TERM
BUNDLE="$WORK/build/$PACKAGE_NAME"
MOVED="$WORK/moved/$PACKAGE_NAME"
EXTRACT_ROOT="$WORK/extracted"
RELEASE_SET="$WORK/release-set"
ZIP="$RELEASE_SET/$PACKAGE_NAME.zip"
PROVENANCE="$RELEASE_SET/$PACKAGE_NAME.zip.provenance.json"
DETACHED_SHA="$RELEASE_SET/$PACKAGE_NAME.zip.sha256"
RELEASE_TOOL="$WORK/aoscx-release-package"
SMOKE="$WORK/package-smoke.test"
INPUT_VERIFICATION="$WORK/input-verification.json"
SOURCE_FILES="$WORK/source-files.list"
SOURCE_VERIFICATION="$WORK/source-verification.json"
FINAL_SOURCE_FILES="$WORK/final-source-files.list"
FINAL_SOURCE_VERIFICATION="$WORK/final-source-verification.json"
DEPENDENCIES="$WORK/package-dependencies.json"
mkdir -p "$BUNDLE" "$WORK/moved" "$RELEASE_SET"

(
	cd "$REPOSITORY"
	export GOPROXY=off GOSUMDB=off GONOPROXY= GONOSUMDB= GOPRIVATE=
	export GOENV=off GOFLAGS= GOEXPERIMENT= GOTOOLCHAIN=local GOWORK=off
	export CGO_ENABLED=0 GOOS=darwin GOARCH=arm64
	go build -trimpath -buildvcs=false -ldflags=-buildid= \
		-o "$RELEASE_TOOL" ./cmd/aoscx-release-package
	snapshot_source_files "$SOURCE_FILES"
	"$RELEASE_TOOL" source-identity \
		--repository "$REPOSITORY" \
		--files-list "$SOURCE_FILES" \
		--output "$SOURCE_VERIFICATION"
	"$RELEASE_TOOL" verify-source \
		--root "$SOURCE_ROOT" \
		--checksums "$SOURCE_CHECKSUMS" \
		--required "$CHROME_SOURCE" \
		--required "$QPDF_SOURCE" \
		--output "$INPUT_VERIFICATION"
	go build -trimpath -buildvcs=false -ldflags=-buildid= \
		-o "$BUNDLE/aos-cx-docs-dldr" ./cmd/aos-cx-docs-dldr
	AOSCX_RELEASE_TOOL="$RELEASE_TOOL" \
		"$SCRIPT_DIR/stage-chrome-sidecar.sh" "$CHROME_SOURCE" "$BUNDLE/sidecars"
	AOSCX_RELEASE_TOOL="$RELEASE_TOOL" \
		"$SCRIPT_DIR/stage-qpdf-sidecar.sh" "$QPDF_SOURCE" "$BUNDLE/sidecars"
	"$RELEASE_TOOL" verify-sidecars \
		--chrome "$BUNDLE/sidecars/chrome-headless-shell-mac-arm64" \
		--qpdf "$BUNDLE/sidecars/qpdf-mac-arm64"
	go list -deps -json ./cmd/aos-cx-docs-dldr >"$DEPENDENCIES"
	go test -c -tags chromepdftests -o "$SMOKE" ./internal/pdfgen
)

mv "$BUNDLE" "$MOVED"
"$MOVED/aos-cx-docs-dldr" --help >"$WORK/moved-help.txt"
"$MOVED/aos-cx-docs-dldr" --version >"$WORK/moved-version.txt"
test -s "$WORK/moved-help.txt"
grep -Fqx "aos-cx-docs-dldr $APP_VERSION" "$WORK/moved-version.txt"
cp "$SMOKE" "$MOVED/.package-smoke.test"
MOVED_SENTINEL="$WORK/moved-smoke.complete"
env -u AOSCX_DOCS_CHROME_PATH -u AOSCX_DOCS_QPDF_PATH \
	AOSCX_WP11_PACKAGE_SMOKE=1 \
	AOSCX_WP13_PACKAGE_SMOKE_SENTINEL="$MOVED_SENTINEL" \
	"$MOVED/.package-smoke.test" \
	-test.run '^TestPinnedSidecarsExecutableRelativeConversion$' \
	-test.count=1
grep -Fqx 'complete' "$MOVED_SENTINEL"
rm "$MOVED/.package-smoke.test"

COMMIT=$(git -C "$REPOSITORY" rev-parse HEAD)
HEAD_TREE=$(git -C "$REPOSITORY" rev-parse 'HEAD^{tree}')
SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-$(git -C "$REPOSITORY" show -s --format=%ct HEAD)}
DIRTY=false
if [ -n "$(git -C "$REPOSITORY" status --porcelain)" ]; then
	DIRTY=true
fi
(
	snapshot_source_files "$FINAL_SOURCE_FILES"
	"$RELEASE_TOOL" source-identity \
		--repository "$REPOSITORY" \
		--files-list "$FINAL_SOURCE_FILES" \
		--output "$FINAL_SOURCE_VERIFICATION"
)
cmp "$SOURCE_FILES" "$FINAL_SOURCE_FILES"
cmp "$SOURCE_VERIFICATION" "$FINAL_SOURCE_VERIFICATION"
GO_VERSION=$(GOTOOLCHAIN=local go version)
GOROOT=$(GOTOOLCHAIN=local GOENV=off GOWORK=off go env GOROOT)
"$RELEASE_TOOL" generate \
	--bundle "$MOVED" \
	--repository "$REPOSITORY" \
	--dependencies "$DEPENDENCIES" \
	--commit "$COMMIT" \
	--head-tree "$HEAD_TREE" \
	--dirty="$DIRTY" \
	--go-version "$GO_VERSION" \
	--goroot "$GOROOT" \
	--source-date-epoch "$SOURCE_DATE_EPOCH" \
	--input-verification "$INPUT_VERIFICATION" \
	--source-verification "$FINAL_SOURCE_VERIFICATION" \
	--moved-smoke \
	--help-smoke \
	--version-smoke
"$RELEASE_TOOL" verify-bundle --bundle "$MOVED"
"$RELEASE_TOOL" archive \
	--bundle "$MOVED" \
	--output "$ZIP" \
	--root-name "$PACKAGE_NAME" \
	--source-date-epoch "$SOURCE_DATE_EPOCH"
EXTRACTED=$("$RELEASE_TOOL" verify-zip \
	--zip "$ZIP" \
	--extract "$EXTRACT_ROOT" \
	--root-name "$PACKAGE_NAME" \
	--source-date-epoch "$SOURCE_DATE_EPOCH")
"$EXTRACTED/aos-cx-docs-dldr" --help >"$WORK/extracted-help.txt"
"$EXTRACTED/aos-cx-docs-dldr" --version >"$WORK/extracted-version.txt"
test -s "$WORK/extracted-help.txt"
grep -Fqx "aos-cx-docs-dldr $APP_VERSION" "$WORK/extracted-version.txt"
cp "$SMOKE" "$EXTRACTED/.package-smoke.test"
EXTRACTED_SENTINEL="$WORK/extracted-smoke.complete"
env -u AOSCX_DOCS_CHROME_PATH -u AOSCX_DOCS_QPDF_PATH \
	AOSCX_WP11_PACKAGE_SMOKE=1 \
	AOSCX_WP13_PACKAGE_SMOKE_SENTINEL="$EXTRACTED_SENTINEL" \
	"$EXTRACTED/.package-smoke.test" \
	-test.run '^TestPinnedSidecarsExecutableRelativeConversion$' \
	-test.count=1
grep -Fqx 'complete' "$EXTRACTED_SENTINEL"
rm "$EXTRACTED/.package-smoke.test"
"$RELEASE_TOOL" verify-bundle --bundle "$EXTRACTED"
"$RELEASE_TOOL" compare --first "$MOVED" --second "$EXTRACTED"
"$RELEASE_TOOL" provenance \
	--bundle "$MOVED" \
	--zip "$ZIP" \
	--output "$PROVENANCE" \
	--sha-output "$DETACHED_SHA" \
	--extracted-smoke

mv "$MOVED" "$RELEASE_SET/$PACKAGE_NAME"
if [ -e "$DESTINATION" ] || [ -L "$DESTINATION" ]; then
	echo "Refusing to replace release-set destination created during assembly: $DESTINATION" >&2
	exit 1
fi
mv "$RELEASE_SET" "$DESTINATION"
echo "Assembled, archived, and smoke-tested unsigned macOS arm64 release set at $DESTINATION"
