#!/usr/bin/env bash
# Image size + startup gate smoke tests for the CI-built Docker image.
# Invoked by ci.yml once per binary (server / consumer / rotator /
# scheduler — all four ship in the SAME image, per the repo-root Dockerfile) with
# IMAGE_TAG and BINARY set, and ENTRYPOINT set for the consumer/rotator/scheduler legs
# (server uses the image's default entrypoint) — keeps shell operators out
# of inline YAML run blocks.
set -euo pipefail

: "${IMAGE_TAG:?IMAGE_TAG is required}"
: "${BINARY:?BINARY is required}"
ENTRYPOINT_ARGS=()
if [ -n "${ENTRYPOINT:-}" ]; then
  ENTRYPOINT_ARGS=(--entrypoint "${ENTRYPOINT}")
fi

echo "::group::Image size check (${BINARY}, linux/amd64)"
# The limit is deliberately generous so it catches regressions (e.g.
# accidentally COPYing third_party build artefacts or embedding test
# assets), not normal arch variance.
MAX_MB=200
size=$(docker image inspect "${IMAGE_TAG}" --format='{{.Size}}')
mb=$((size / 1024 / 1024))
echo "Image size: ${mb} MB (limit: ${MAX_MB} MB)"
[ "${mb}" -le "${MAX_MB}" ] &
P1=$!
echo "::endgroup::"

echo "::group::Startup gate (${BINARY})"
# The binary must exit non-zero on missing required config, proving its
# config-loading validation actually fires (DATABASE_URL/OPENBAO_ADDR/
# SNS_TOPIC_ARN for the server; SQS_QUEUE_URL
# for the consumer; DATABASE_URL/OPENBAO_ADDR/REALM_PROVISIONER_BASE_URL for the
# rotator and scheduler).
# timeout 10s kills the container if it hangs instead of exiting; that
# exit code (124, or 137 after KILL) is a FAILURE, not the expected
# non-zero — a binary that hangs on missing config never reached validation.
exit_code=0
timeout 10s docker run --rm "${ENTRYPOINT_ARGS[@]}" "${IMAGE_TAG}" 2>/dev/null || exit_code=$?
echo "Container exit code: ${exit_code} (expected non-zero)"
[ "${exit_code}" -ne 0 ] && [ "${exit_code}" -ne 124 ] && [ "${exit_code}" -ne 137 ] &
P2=$!
echo "::endgroup::"

wait $P1 || {
  echo "::error title=Image size (${BINARY})::Image is ${mb} MB, exceeds ${MAX_MB} MB limit - check COPY instructions for accidental inclusions"
  exit 1
}
wait $P2 || {
  echo "::error title=Startup gate (${BINARY})::Binary exited ${exit_code} on missing required env vars - config loading must exit non-zero promptly (0 = no validation; 124/137 = hung until the 10s timeout)"
  exit 1
}

{
  echo "### Smoke test results - ${BINARY}"
  echo "- Startup gate: binary exits ${exit_code} on missing required env vars"
  echo "- Image size (linux/amd64): **${mb} MB** (limit: ${MAX_MB} MB)"
} >> "$GITHUB_STEP_SUMMARY"
