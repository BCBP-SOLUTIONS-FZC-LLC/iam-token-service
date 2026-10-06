#!/usr/bin/env bash
# Creates the GitHub release with versioned assets attached.
#
# This service ships FOUR binaries (see prepare-release-binary.sh), so
# release-asset.name holds one line per binary (the linux/amd64 build of
# each), and every platform build for all four binaries is attached via
# the shared `iam-token-service-*` glob.
set -euo pipefail

: "${RELEASE_TAG:?RELEASE_TAG is required}"
: "${GH_TOKEN:?GH_TOKEN is required}"
: "${IMAGE_DIGEST:?IMAGE_DIGEST is required}"

test -f release-asset.name || {
  echo "::error::release-asset.name missing — run prepare-release-binary.sh first"
  exit 1
}

mapfile -t ASSET_NAMES < release-asset.name
[ "${#ASSET_NAMES[@]}" -gt 0 ] || {
  echo "::error::release-asset.name is empty — run prepare-release-binary.sh first"
  exit 1
}

for f in release-notes.md provenance.slsa.json sbom.cyclonedx.json; do
  test -f "$f" || {
    echo "::error::Release asset missing: ${f}"
    exit 1
  }
done
for ASSET_NAME in "${ASSET_NAMES[@]}"; do
  for f in "$ASSET_NAME" "${ASSET_NAME}.sha256"; do
    test -f "$f" || {
      echo "::error::Release asset missing: ${f}"
      exit 1
    }
  done
done

# Aggregate per-binary checksums (server, consumer, rotator, scheduler, all platforms)
# into a single verifiable file.
# Format matches `sha256sum --check checksums.txt`.
BINARY_FILES=()
for f in iam-token-service-*; do
  case "$f" in
    *.sha256) continue ;;
    *) BINARY_FILES+=("$f") ;;
  esac
done
sha256sum "${BINARY_FILES[@]}" > checksums.txt

# A '-' suffix (v1.1.0-rc.1) is a pre-release: release.yml skips its
# production schema registration and deploy for the same test.
PRERELEASE_FLAG=""
if echo "$RELEASE_TAG" | grep -q -- '-'; then
  PRERELEASE_FLAG="--prerelease"
fi

DIGEST_SHORT="${IMAGE_DIGEST#sha256:}"
DIGEST_SHORT="${DIGEST_SHORT:0:12}"
RELEASE_TITLE="${RELEASE_TAG} · sha256:${DIGEST_SHORT}"

gh release create "$RELEASE_TAG" \
  --title "$RELEASE_TITLE" \
  --notes-file release-notes.md \
  "${BINARY_FILES[@]}" \
  checksums.txt \
  provenance.slsa.json \
  sbom.cyclonedx.json \
  $PRERELEASE_FLAG
