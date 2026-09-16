---
name: Bug report
about: Report a defect in iam-token-service
title: "[BUG] "
labels: bug
assignees: ''
---

## Summary

<!-- A clear, concise description of the bug. -->

## Component

- [ ] `cmd/server` — HTTP API (TS-1..TS-4)
- [ ] `cmd/consumer` — offboarding cascade (`TenantMembershipsPurged`)
- [ ] `cmd/rotator` — overlap-expiry sweep / orphan-material reconciler / prune
- [ ] `internal/adapter/outbound/openbao/` — OpenBao KV v2 client
- [ ] `internal/adapter/outbound/postgres/` — repositories / migrations
- [ ] `internal/adapter/outbound/eventbus/` — outbox / SNS / Glue codec
- [ ] Deployment (Helm chart / CI)
- [ ] Other:

## Steps to Reproduce

1.
2.
3.

## Expected Behavior

## Actual Behavior

## Environment

- Deployment environment (dev/staging/production):
- Image tag / git SHA:
- Relevant log excerpt (redact any credential/secret value, TS-INV-2):

## Additional Context

<!-- LLD section reference, related tenant/principal id (no secret material), screenshots, etc. -->
