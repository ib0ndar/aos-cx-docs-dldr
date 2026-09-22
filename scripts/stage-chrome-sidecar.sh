#!/bin/sh
set -eu

if [ "$#" -ne 2 ] || [ -z "${AOSCX_RELEASE_TOOL:-}" ]; then
	echo "usage: AOSCX_RELEASE_TOOL=/absolute/path/to/helper $0 VERIFIED_SOURCE_DIR OUTPUT_ROOT" >&2
	exit 2
fi
if [ "$(uname -s)" != Darwin ] || [ "$(uname -m)" != arm64 ]; then
	echo "Chrome sidecar staging is supported only on macOS arm64" >&2
	exit 1
fi

SOURCE=$1
OUTPUT_ROOT=$2
DESTINATION="$OUTPUT_ROOT/chrome-headless-shell-mac-arm64"
mkdir -p "$OUTPUT_ROOT"
"$AOSCX_RELEASE_TOOL" stage-tree --source "$SOURCE" --destination "$DESTINATION"
echo "Staged checksum-verified local Chrome sidecar at $DESTINATION"
