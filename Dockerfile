# syntax=docker/dockerfile:1

# Container image for aos-cx-docs-dldr, one of the two supported deliverables
# alongside the native macOS arm64 bundle. It is a complete build: the
# application plus its pinned Chrome for Testing headless shell and qpdf
# sidecars, so generated-PDF conversion works exactly as on macOS.
#
# linux/amd64 only. Upstream qpdf 12.4.1 publishes no arm64 binary, so an arm64
# image cannot carry the exact pin; it is deliberately not built.
#
# Every external input is pinned:
#   - base images by digest (change tag and digest together, never the tag alone)
#   - Chrome for Testing 153.0.8010.36 / r1681091 linux64 by archive SHA-256
#   - qpdf 12.4.1 linux-x86_64 by archive SHA-256
# The application then re-verifies every sidecar component by SHA-256,
# architecture and shared-library closure at build time and again at each
# launch. Nothing is downloaded at runtime and PATH is never searched.
#
# See docs/container.md.

# ---------------------------------------------------------------------------
# Build stage: compile the application and stage verified sidecars.
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.27.1-trixie@sha256:9baa6b4187bbb98d240372a8a235ac0bb6b5ddd52bba1431dc2f7c0705862728 AS build

ENV GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=linux GOARCH=amd64

WORKDIR /src
COPY go.mod go.sum ./
COPY third_party/ third_party/
RUN go mod download && go mod verify
COPY . .

RUN go build -trimpath -o /out/aos-cx-docs-dldr ./cmd/aos-cx-docs-dldr

RUN go build -trimpath -o /out/aoscx-verify-sidecars ./cmd/aoscx-verify-sidecars

# Pinned sidecar archives. The SHA-256 values are recorded in
# internal/pdfgen/sidecar_platform.go and must match exactly.
ARG CHROME_URL=https://storage.googleapis.com/chrome-for-testing-public/153.0.8010.36/linux64/chrome-headless-shell-linux64.zip
ARG CHROME_ARCHIVE_SHA256=a0079df5617da34bcd1debad18196568b072ec3d8ec57944008972a4fc970580
ARG QPDF_URL=https://github.com/qpdf/qpdf/releases/download/v12.4.1/qpdf-12.4.1-bin-linux-x86_64.zip
ARG QPDF_ARCHIVE_SHA256=db9122e88ec00c76ac6a14e09ffb92406db1773d47b968911ff6e69f28c09bf9

RUN apt-get update && apt-get install -y --no-install-recommends curl unzip ca-certificates && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /sidecar-src
RUN curl -fsSL -o chrome.zip "$CHROME_URL" && \
    echo "$CHROME_ARCHIVE_SHA256  chrome.zip" | sha256sum -c - && \
    curl -fsSL -o qpdf.zip "$QPDF_URL" && \
    echo "$QPDF_ARCHIVE_SHA256  qpdf.zip" | sha256sum -c - && \
    unzip -q chrome.zip && unzip -q qpdf.zip -d qpdf

# Stage the layout the application resolves relative to its executable:
#   <dir>/sidecars/chrome-headless-shell-linux64/chrome-headless-shell
#   <dir>/sidecars/qpdf-linux-x86_64/bin/qpdf + lib/*.so
# The upstream qpdf archive ships libqpdf.so.30 as a symlink to
# libqpdf.so.30.4.1. The application refuses symlinked components, so the real
# file is materialised under its soname, exactly as the macOS bundle does.
RUN set -eu; \
    S=/out/sidecars; mkdir -p "$S/qpdf-linux-x86_64/bin" "$S/qpdf-linux-x86_64/lib" "$S/qpdf-linux-x86_64/licenses"; \
    mv chrome-headless-shell-linux64 "$S/chrome-headless-shell-linux64"; \
    cp qpdf/bin/qpdf "$S/qpdf-linux-x86_64/bin/qpdf"; \
    for lib in libffi.so.8 libgnutls.so.30 libhogweed.so.6 libidn2.so.0 libjpeg.so.8 \
               libnettle.so.8 libp11-kit.so.0 libtasn1.so.6 libunistring.so.2; do \
        cp "qpdf/lib/$lib" "$S/qpdf-linux-x86_64/lib/$lib"; \
    done; \
    cp qpdf/lib/libqpdf.so.30.4.1 "$S/qpdf-linux-x86_64/lib/libqpdf.so.30"; \
    cp -r qpdf/share/doc/* "$S/qpdf-linux-x86_64/licenses/" 2>/dev/null || true; \
    chmod 0755 "$S/qpdf-linux-x86_64/bin/qpdf" "$S/chrome-headless-shell-linux64/chrome-headless-shell"; \
    find "$S" -type l -delete

RUN mkdir -p /skel/library /skel/cache /skel/home/nonroot

# ---------------------------------------------------------------------------
# Runtime stage.
# ---------------------------------------------------------------------------
# debian:trixie-slim, resolved 2026-09-19.
FROM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a

ARG VERSION=0.8
LABEL org.opencontainers.image.title="aos-cx-docs-dldr" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.description="Offline copies of public HPE Aruba Networking AOS-CX Product Documentation, with generated-PDF conversion" \
      org.opencontainers.image.licenses="Apache-2.0"

# Runtime libraries, derived from the sidecars' actual DT_NEEDED closures
# rather than Chrome's generic deb.deps (which lists desktop packages such as
# GTK, pango, cups and wget that the headless shell does not link):
#   Chrome: X11/xcb, atk/atspi, dbus, expat, gbm, glib/gio, nss/nspr, udev,
#           xkbcommon, asound, plus fonts so text renders.
#   qpdf:   libgmp10 and zlib1g are the only non-bundled non-libc dependencies.
#   procps: /bin/ps for renderer process-group memory accounting.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        fonts-liberation fonts-dejavu-core \
        libasound2t64 libatk-bridge2.0-0t64 libatk1.0-0t64 libatspi2.0-0t64 \
        libdbus-1-3 libexpat1 libgbm1 libglib2.0-0t64 \
        libnspr4 libnss3 libudev1 \
        libx11-6 libxcb1 libxcomposite1 libxdamage1 libxext6 libxfixes3 libxrandr2 \
        libxkbcommon0 \
        libgmp10 zlib1g \
        procps \
    && rm -rf /var/lib/apt/lists/*

# Same UID as the previous distroless nonroot image, so existing named volumes
# keep working.
RUN groupadd --gid 65532 nonroot && \
    useradd --uid 65532 --gid 65532 --home-dir /home/nonroot --shell /usr/sbin/nologin nonroot

ENV XDG_CACHE_HOME=/cache \
    HOME=/home/nonroot \
    SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt

COPY --from=build --chown=65532:65532 /skel/library /library
COPY --from=build --chown=65532:65532 /skel/cache /cache
COPY --from=build --chown=65532:65532 /skel/home/nonroot /home/nonroot

# The application and its sidecars live together; the sidecars are resolved
# relative to the executable's real path (/proc/self/exe), so a PATH symlink
# is fine.
COPY --from=build /out/aos-cx-docs-dldr /opt/aos-cx-docs-dldr/aos-cx-docs-dldr
COPY --from=build /src/LICENSE /opt/aos-cx-docs-dldr/LICENSE
COPY --from=build /out/sidecars /opt/aos-cx-docs-dldr/sidecars
COPY --from=build /out/aoscx-verify-sidecars /tmp/aoscx-verify-sidecars
RUN ln -s /opt/aos-cx-docs-dldr/aos-cx-docs-dldr /usr/local/bin/aos-cx-docs-dldr

# Build-time proof that the staged sidecars match the pins the application
# enforces at every launch: regular file, x86-64 ELF, SHA-256, and exact
# shared-library closure for every component.
RUN /tmp/aoscx-verify-sidecars \
        /opt/aos-cx-docs-dldr/sidecars/chrome-headless-shell-linux64/chrome-headless-shell \
        /opt/aos-cx-docs-dldr/sidecars/qpdf-linux-x86_64/bin/qpdf && \
    rm /tmp/aoscx-verify-sidecars

VOLUME ["/cache"]
WORKDIR /library
USER 65532:65532
ENTRYPOINT ["/opt/aos-cx-docs-dldr/aos-cx-docs-dldr"]
