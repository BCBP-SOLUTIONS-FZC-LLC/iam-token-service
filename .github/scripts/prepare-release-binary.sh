#!/usr/bin/env bash
# Cross-compiles release binaries for all target platforms and writes
# per-binary checksums. The artifact directory is the working directory
# when this runs.
#
# This service ships THREE binaries from one image (see Dockerfile /
# Makefile's `build` target) — /iam-token-service-server (HTTP API TS-1..
# TS-4 + outbox runner), /iam-token-service-consumer (offboarding cascade,
# enqueue-only), and /iam-token-service-rotator (overlap-expiry sweep +
# orphan-material reconciler + prune, run-to-completion CronJob), and
# /iam-token-service-scheduler (cadence rotation CronJob). All four
# are cross-compiled here, per platform, mirroring the Makefile's own build
# target's -ldflags convention.
#
# Produces for each platform x binary:
#   iam-token-service-{server,consumer,rotator,scheduler}_{version}_{os}_{arch}[.exe]
#   iam-token-service-{server,consumer,rotator,scheduler}_{version}_{os}_{arch}[.exe].sha256
#
# The caller (create-github-release.sh) aggregates all the non-.sha256 files
# into a single checksums.txt with
# `sha256sum iam-token-service-* | grep -v '\.sha256$'`.
#
# This service is deployed exclusively as a container — these binaries are
# a convenience for local/non-Docker runs, not the primary distribution
# artifact (that's the signed GHCR image, which carries all three
# entrypoints already).
set -euo pipefail

: "${RELEASE_TAG:?RELEASE_TAG is required}"

TARGETS=(
  "linux   amd64"
  "linux   arm64"
  "darwin  amd64"
  "darwin  arm64"
  "windows amd64"
)

BINARIES=(
  "server   ./cmd/server"
  "consumer ./cmd/consumer"
  "rotator  ./cmd/rotator"
  "scheduler ./cmd/scheduler"
)

echo "Building release binaries for tag ${RELEASE_TAG}"

: > release-asset.name

for binary in "${BINARIES[@]}"; do
  read -r NAME PKG <<< "$binary"

  for target in "${TARGETS[@]}"; do
    read -r GOOS GOARCH <<< "$target"
    EXT=""
    [ "${GOOS}" = "windows" ] && EXT=".exe"
    ASSET_NAME="iam-token-service-${NAME}_${RELEASE_TAG}_${GOOS}_${GOARCH}${EXT}"

    echo "  → ${NAME} ${GOOS}/${GOARCH}"
    CGO_ENABLED=0 GOOS="${GOOS}" GOARCH="${GOARCH}" \
      go build -trimpath \
        -ldflags "-s -w -X main.buildVersion=${RELEASE_TAG}" \
        -o "${ASSET_NAME}" \
        "${PKG}"

    chmod +x "${ASSET_NAME}"
    sha256sum "${ASSET_NAME}" > "${ASSET_NAME}.sha256"
    echo "    ✔  ${ASSET_NAME}"
  done

  # Record the primary (linux/amd64) asset name per binary for downstream
  # steps that need a canonical file reference per entrypoint.
  echo "iam-token-service-${NAME}_${RELEASE_TAG}_linux_amd64" >> release-asset.name
done

echo "All binaries prepared."
