#!/bin/sh
set -eu

if [ "$#" -ne 1 ]; then
	echo "usage: $0 DESTINATION_ROOT" >&2
	exit 2
fi
if [ "$(uname -s)" != Darwin ] || [ "$(uname -m)" != arm64 ]; then
	echo "sidecar acquisition is supported only on macOS arm64" >&2
	exit 1
fi

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPOSITORY=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
case $1 in
/*) DESTINATION=$1 ;;
*) DESTINATION=$PWD/$1 ;;
esac
if [ -e "$DESTINATION" ] || [ -L "$DESTINATION" ]; then
	echo "Refusing to replace existing sidecar input root: $DESTINATION" >&2
	exit 1
fi
for tool in go curl install_name_tool codesign shasum; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		echo "Required sidecar acquisition tool is unavailable: $tool" >&2
		exit 1
	fi
done

WORK=$(mktemp -d "${TMPDIR:-/tmp}/aos-cx-docs-dldr-sidecar-acquire.XXXXXXXX")
trap 'rm -rf "$WORK"' EXIT HUP INT TERM
TOOL="$WORK/aoscx-release-package"
STAGED="$WORK/staged"
mkdir -p "$STAGED/sidecars"
(
	cd "$REPOSITORY"
	GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOWORK=off GOENV=off GOFLAGS= \
		CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 \
		go build -trimpath -buildvcs=false -ldflags=-buildid= \
		-o "$TOOL" ./cmd/aoscx-release-package
)
pin() {
	"$TOOL" pin "$1"
}
extract_archive() {
	archive=$1
	destination=$2
	format=$3
	sha256=$4
	root=$5
	"$TOOL" extract \
		--archive "$archive" \
		--destination "$destination" \
		--format "$format" \
		--sha256 "$sha256" \
		--root "$root"
}
extract_selected_archive() {
	archive=$1
	destination=$2
	sha256=$3
	root=$4
	shift 4
	set -- "$TOOL" extract \
		--archive "$archive" \
		--destination "$destination" \
		--format tar.gz \
		--sha256 "$sha256" \
		--root "$root" "$@"
	"$@"
}

CHROME_VERSION=$(pin chrome-version)
CHROME_PLATFORM=$(pin chrome-platform)
CHROME_ARCHIVE_SHA256=$(pin chrome-archive-sha256)
CHROME_EXECUTABLE_SHA256=$(pin chrome-executable-sha256)
CHROME_URL=$(pin chrome-url)
CHROME_ARCHIVE='chrome-headless-shell-mac-arm64.zip'
curl --fail --location --proto '=https' --tlsv1.2 \
	--output "$WORK/$CHROME_ARCHIVE" "$CHROME_URL"
extract_archive "$WORK/$CHROME_ARCHIVE" "$WORK/chrome" zip \
	"$CHROME_ARCHIVE_SHA256" chrome-headless-shell-mac-arm64
CHROME_SOURCE="$WORK/chrome/chrome-headless-shell-mac-arm64"
printf '%s  %s\n' "$CHROME_EXECUTABLE_SHA256" "$CHROME_SOURCE/chrome-headless-shell" |
	shasum -a 256 -c -
"$TOOL" stage-tree \
	--source "$CHROME_SOURCE" \
	--destination "$STAGED/sidecars/chrome-headless-shell-mac-arm64"

download_bottle() {
	repository=$1
	digest=$2
	output=$3
	header=$output.header
	curl --fail --silent --show-error --proto '=https' --tlsv1.2 \
		"https://ghcr.io/token?service=ghcr.io&scope=repository:homebrew/core/$repository:pull" |
		"$TOOL" auth-header --output "$header"
	if curl --fail --location --proto '=https' --tlsv1.2 \
		--header "@$header" \
		--output "$output" \
		"https://ghcr.io/v2/homebrew/core/$repository/blobs/sha256:$digest"; then
		rm -f -- "$header"
	else
		status=$?
		rm -f -- "$header"
		return "$status"
	fi
	printf '%s  %s\n' "$digest" "$output" | shasum -a 256 -c -
}

QPDF_VERSION=$(pin qpdf-version)
JPEG_VERSION=$(pin jpeg-version)
OPENSSL_VERSION=$(pin openssl-version)
QPDF_BOTTLE_SHA256=$(pin qpdf-bottle-sha256)
JPEG_BOTTLE_SHA256=$(pin jpeg-bottle-sha256)
OPENSSL_BOTTLE_SHA256=$(pin openssl-bottle-sha256)
download_bottle qpdf "$QPDF_BOTTLE_SHA256" "$WORK/qpdf.tar.gz"
download_bottle jpeg-turbo "$JPEG_BOTTLE_SHA256" "$WORK/jpeg.tar.gz"
download_bottle openssl/3 "$OPENSSL_BOTTLE_SHA256" "$WORK/openssl.tar.gz"
extract_selected_archive "$WORK/qpdf.tar.gz" "$WORK/qpdf-unpacked" \
	"$QPDF_BOTTLE_SHA256" qpdf \
	--select "qpdf/$QPDF_VERSION/bin/qpdf=qpdf/$QPDF_VERSION/bin/qpdf" \
	--select "qpdf/$QPDF_VERSION/lib/libqpdf.30.4.1.dylib=qpdf/$QPDF_VERSION/lib/libqpdf.30.4.1.dylib" \
	--select "qpdf/$QPDF_VERSION/LICENSE.txt=qpdf/$QPDF_VERSION/LICENSE.txt" \
	--select "qpdf/$QPDF_VERSION/NOTICE.md=qpdf/$QPDF_VERSION/NOTICE.md"
extract_selected_archive "$WORK/jpeg.tar.gz" "$WORK/jpeg-unpacked" \
	"$JPEG_BOTTLE_SHA256" jpeg-turbo \
	--select "jpeg-turbo/$JPEG_VERSION/lib/libjpeg.8.3.2.dylib=jpeg-turbo/$JPEG_VERSION/lib/libjpeg.8.3.2.dylib" \
	--select "jpeg-turbo/$JPEG_VERSION/LICENSE.md=jpeg-turbo/$JPEG_VERSION/LICENSE.md"
extract_selected_archive "$WORK/openssl.tar.gz" "$WORK/openssl-unpacked" \
	"$OPENSSL_BOTTLE_SHA256" openssl@3 \
	--select "openssl@3/$OPENSSL_VERSION/lib/libcrypto.3.dylib=openssl@3/$OPENSSL_VERSION/lib/libcrypto.3.dylib" \
	--select "openssl@3/$OPENSSL_VERSION/LICENSE.txt=openssl@3/$OPENSSL_VERSION/LICENSE.txt"

QPDF="$WORK/qpdf-unpacked/qpdf/$QPDF_VERSION"
JPEG="$WORK/jpeg-unpacked/jpeg-turbo/$JPEG_VERSION"
OPENSSL="$WORK/openssl-unpacked/openssl@3/$OPENSSL_VERSION"
printf '%s  %s\n' "$(pin qpdf-executable-sha256)" "$QPDF/bin/qpdf" | shasum -a 256 -c -
printf '%s  %s\n' "$(pin qpdf-library-input-sha256)" "$QPDF/lib/libqpdf.30.4.1.dylib" | shasum -a 256 -c -
printf '%s  %s\n' "$(pin jpeg-library-input-sha256)" "$JPEG/lib/libjpeg.8.3.2.dylib" | shasum -a 256 -c -
printf '%s  %s\n' "$(pin crypto-library-input-sha256)" "$OPENSSL/lib/libcrypto.3.dylib" | shasum -a 256 -c -

QPDF_STAGED="$STAGED/sidecars/qpdf-mac-arm64"
mkdir -p "$QPDF_STAGED/bin" "$QPDF_STAGED/lib" \
	"$QPDF_STAGED/licenses/qpdf" "$QPDF_STAGED/licenses/jpeg-turbo" \
	"$QPDF_STAGED/licenses/openssl"
cp "$QPDF/bin/qpdf" "$QPDF_STAGED/bin/qpdf"
cp "$QPDF/lib/libqpdf.30.4.1.dylib" "$QPDF_STAGED/lib/libqpdf.30.dylib"
cp "$JPEG/lib/libjpeg.8.3.2.dylib" "$QPDF_STAGED/lib/libjpeg.8.dylib"
cp "$OPENSSL/lib/libcrypto.3.dylib" "$QPDF_STAGED/lib/libcrypto.3.dylib"
cp "$QPDF/LICENSE.txt" "$QPDF/NOTICE.md" "$QPDF_STAGED/licenses/qpdf/"
cp "$JPEG/LICENSE.md" "$QPDF_STAGED/licenses/jpeg-turbo/"
cp "$OPENSSL/LICENSE.txt" "$QPDF_STAGED/licenses/openssl/"
install_name_tool \
	-id '@rpath/libqpdf.30.dylib' \
	-change '@@HOMEBREW_PREFIX@@/opt/jpeg-turbo/lib/libjpeg.8.dylib' '@loader_path/libjpeg.8.dylib' \
	-change '@@HOMEBREW_PREFIX@@/opt/openssl@3/lib/libcrypto.3.dylib' '@loader_path/libcrypto.3.dylib' \
	"$QPDF_STAGED/lib/libqpdf.30.dylib"
codesign --force --sign - "$QPDF_STAGED/lib/libqpdf.30.dylib"
install_name_tool -id '@loader_path/libjpeg.8.dylib' "$QPDF_STAGED/lib/libjpeg.8.dylib"
codesign --force --sign - "$QPDF_STAGED/lib/libjpeg.8.dylib"
install_name_tool -id '@loader_path/libcrypto.3.dylib' "$QPDF_STAGED/lib/libcrypto.3.dylib"
codesign --force --sign - "$QPDF_STAGED/lib/libcrypto.3.dylib"

"$TOOL" verify-sidecars \
	--chrome "$STAGED/sidecars/chrome-headless-shell-mac-arm64" \
	--qpdf "$QPDF_STAGED"
"$TOOL" checksums --root "$STAGED" --output "$STAGED/SHA256SUMS"
"$TOOL" verify-source \
	--root "$STAGED" \
	--checksums "$STAGED/SHA256SUMS" \
	--required "$STAGED/sidecars/chrome-headless-shell-mac-arm64" \
	--required "$QPDF_STAGED" \
	--output "$WORK/acquisition-verification.json"
mv "$STAGED" "$DESTINATION"
echo "Acquired and checksum-sealed Chrome $CHROME_VERSION ($CHROME_PLATFORM) and qpdf $QPDF_VERSION at $DESTINATION"
