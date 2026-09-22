#!/bin/sh
# Build the distributable aos-cx-docs-dldr container image and save it as a loadable
# archive.
#
# The image is linux/amd64 only. It is a complete build with pinned Chrome and
# qpdf sidecars, so generated-PDF conversion works inside it. Upstream qpdf
# 12.4.1 publishes no arm64 binary, so an arm64 image cannot carry the exact
# pin and is deliberately not built.
#
# The archive contains the image tagged exactly `aos-cx-docs-dldr:<version>`, so the
# PowerShell wrapper works with no further configuration.
set -eu
umask 022

usage() {
	echo "usage: $0 [--version VERSION] DESTINATION" >&2
	exit 2
}

VERSION=
while [ "$#" -gt 1 ]; do
	case $1 in
	--version) VERSION=${2-}; shift 2 ;;
	*) usage ;;
	esac
done
[ "$#" -eq 1 ] || usage
DESTINATION=$1

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPOSITORY=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)

# The application version is authoritative in internal/model/model.go. Derive it
# rather than accepting a value that could disagree with the binary.
if [ -z "$VERSION" ]; then
	VERSION=$(sed -n 's/^const Version = "\(.*\)"$/\1/p' "$REPOSITORY/internal/model/model.go")
fi
[ -n "$VERSION" ] || { echo "could not determine application version" >&2; exit 1; }

PLATFORMS="linux/amd64"

for tool in docker gzip shasum sed; do
	command -v "$tool" >/dev/null 2>&1 || { echo "required tool not found: $tool" >&2; exit 1; }
done
docker info >/dev/null 2>&1 || { echo "container engine is not running" >&2; exit 1; }

mkdir -p "$DESTINATION"
DESTINATION=$(CDPATH= cd -- "$DESTINATION" && pwd)
TAG="aos-cx-docs-dldr:${VERSION}"

echo "Building ${TAG} for:${PLATFORMS}"
ARCHIVES=
for platform in $PLATFORMS; do
	arch=$(printf '%s\n' "$platform" | sed 's|.*/||')
	archive="aos-cx-docs-dldr-${VERSION}-linux-${arch}.tar"

	# Build and tag as the plain version tag so the loaded image needs no
	# AOSCX_DOCS_IMAGE override on the user's machine.
	docker build --platform "$platform" -t "$TAG" "$REPOSITORY"

	built=$(docker image inspect "$TAG" --format '{{.Architecture}}')
	[ "$built" = "$arch" ] || {
		echo "built architecture ${built} does not match requested ${arch}" >&2
		exit 1
	}

	# The image verified its sidecars at build time. Confirm the finished
	# image also starts and reports the expected version before saving it.
	reported=$(docker run --rm --platform "$platform" --network none "$TAG" --version)
	[ "$reported" = "aos-cx-docs-dldr $VERSION" ] || {
		echo "image reports ${reported}, expected aos-cx-docs-dldr ${VERSION}" >&2
		exit 1
	}

	rm -f "$DESTINATION/$archive" "$DESTINATION/$archive.gz"
	docker save "$TAG" -o "$DESTINATION/$archive"
	# Omit the source filename and mtime so identical image exports have
	# identical compressed bytes across independent packaging runs.
	gzip -n -9 "$DESTINATION/$archive"
	ARCHIVES="${ARCHIVES} ${archive}.gz"
	echo "  wrote ${archive}.gz"
done

( cd "$DESTINATION" && rm -f SHA256SUMS && for archive in $ARCHIVES; do
	shasum -a 256 "$archive"
done > SHA256SUMS )

echo
echo "Archives in ${DESTINATION}:"
( cd "$DESTINATION" && cat SHA256SUMS )
echo
echo "On the target machine:"
echo "  shasum -a 256 -c SHA256SUMS"
echo "  docker load -i aos-cx-docs-dldr-${VERSION}-linux-<arch>.tar.gz"
