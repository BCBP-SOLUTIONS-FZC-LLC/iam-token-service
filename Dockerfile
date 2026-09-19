# syntax=docker/dockerfile:1.7
#
# iam-token-service
#
# Multi-stage build producing a minimal, non-root, distroless runtime image
# carrying all four binaries this service ships (§3, §13, §build-order):
#   /iam-token-service-server     HTTP API (TS-1..TS-4) + the outbox.Runner
#                                  SNS-publish loop. The image's default
#                                  ENTRYPOINT.
#   /iam-token-service-consumer   the TenantMembershipsPurged offboarding
#                                  cascade (§7.1, §8.4) — enqueue-only, no
#                                  Glue/SNS credentials needed.
#   /iam-token-service-rotator    the §8.3/§8.6/§13.1 scheduled maintenance
#                                  jobs (overlap-expiry sweep, orphan-material
#                                  reconciler, outbox/processed_events prune)
#                                  — run-to-completion, invoked by a
#                                  Kubernetes CronJob.
#   /iam-token-service-scheduler  the §16 TSQ-6 Resolved automatic
#                                  cadence-driven rotation scan (TS-D14) —
#                                  run-to-completion, invoked by its own
#                                  Kubernetes CronJob.
#
# One image: the server/consumer Deployments run it unmodified; the rotator
# and scheduler CronJob templates (deploy/helm/templates/cronjobs.yaml)
# override `command` to invoke /iam-token-service-rotator or
# /iam-token-service-scheduler against the SAME image reference.
#
# NOTE: base image FROM lines below are tag-pinned, not digest-pinned — this
# repo has no verified digest to pin against yet (§13.6: nothing is deployed
# anywhere). Digest-pin these once a release pipeline exists to verify and
# refresh them, matching the sibling services' supply-chain posture.

########################################
# Stage: builder
########################################
FROM golang:1.26.6-bookworm AS builder

ARG BUILD_VERSION=dev
ARG SOURCE_DATE_EPOCH
ENV SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH}

WORKDIR /src

# Copy go.mod/go.sum first so dependency resolution is cached independently
# of application source changes.
COPY go.mod go.sum ./

# platform-gincommon, platform-pgcommon, and platform-events are private
# github.com/BCBP-SOLUTIONS-FZC-LLC modules (not vendored locally), fetched
# via git using a short-lived token — same secret-handling pattern as every
# sibling IAM service's Dockerfile.
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

ENV GOPRIVATE="github.com/BCBP-SOLUTIONS-FZC-LLC/*"
ENV GONOSUMDB="github.com/BCBP-SOLUTIONS-FZC-LLC/*"

RUN --mount=type=secret,id=go_private_token \
    TOKEN=$(cat /run/secrets/go_private_token 2>/dev/null || true) && \
    if [ -z "$TOKEN" ]; then echo "ERROR: go_private_token secret is missing or empty — pass --secret id=go_private_token,src=<token-file>"; exit 1; fi && \
    git config --global url."https://x-access-token:${TOKEN}@github.com/".insteadOf "https://github.com/" && \
    go mod download && \
    git config --global --unset url."https://x-access-token:${TOKEN}@github.com/".insteadOf

# Now copy the remainder of the source tree.
COPY . .

RUN CGO_ENABLED=0 GOOS=linux \
    GOPRIVATE="github.com/BCBP-SOLUTIONS-FZC-LLC/*" \
    go build \
    -trimpath \
    -ldflags="-s -w -X main.buildVersion=${BUILD_VERSION}" \
    -o /out/iam-token-service-server \
    ./cmd/server && \
    go build \
    -trimpath \
    -ldflags="-s -w -X main.buildVersion=${BUILD_VERSION}" \
    -o /out/iam-token-service-consumer \
    ./cmd/consumer && \
    go build \
    -trimpath \
    -ldflags="-s -w -X main.buildVersion=${BUILD_VERSION}" \
    -o /out/iam-token-service-rotator \
    ./cmd/rotator && \
    go build \
    -trimpath \
    -ldflags="-s -w -X main.buildVersion=${BUILD_VERSION}" \
    -o /out/iam-token-service-scheduler \
    ./cmd/scheduler

########################################
# Stage: runtime
########################################
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

ARG BUILD_VERSION=dev
ENV BUILD_VERSION=${BUILD_VERSION}

LABEL org.opencontainers.image.title="iam-token-service" \
      org.opencontainers.image.description="Token Service — credential lifecycle (issue/rotate/revoke) for the per-tenant platform-automation service-account principal (TS-1..TS-4)" \
      org.opencontainers.image.source="https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service" \
      org.opencontainers.image.vendor="BCBP Solutions" \
      org.opencontainers.image.licenses="Proprietary" \
      org.opencontainers.image.base.name="gcr.io/distroless/static-debian12:nonroot" \
      org.opencontainers.image.revision="${BUILD_VERSION}"

WORKDIR /

COPY --from=builder /out/iam-token-service-server /iam-token-service-server
COPY --from=builder /out/iam-token-service-consumer /iam-token-service-consumer
COPY --from=builder /out/iam-token-service-rotator /iam-token-service-rotator
COPY --from=builder /out/iam-token-service-scheduler /iam-token-service-scheduler

USER nonroot:nonroot

EXPOSE 8080 9090

ENTRYPOINT ["/iam-token-service-server"]
