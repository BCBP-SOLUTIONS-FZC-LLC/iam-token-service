---
name: Feature request
about: Propose a change or enhancement to iam-token-service
title: "[FEATURE] "
labels: enhancement
assignees: ''
---

## Problem

<!-- What gap or pain point does this address? -->

## Proposed Solution

<!-- What would you like to happen? Reference the relevant LLD section if one exists, or note that this requires an LLD update. -->

## Component(s) Affected

- [ ] `cmd/server` — HTTP API
- [ ] `cmd/consumer` — offboarding cascade
- [ ] `cmd/rotator` — sweep / reconciler / prune
- [ ] Event contract (`api/asyncapi.yaml`, `internal/eventschema/`)
- [ ] OpenBao integration
- [ ] Database schema
- [ ] Deployment (Helm chart / CI)

## Alternatives Considered

## Additional Context

<!-- Anything that touches a frozen §25 name, a secret/RLS/OpenBao boundary, or a Keycloak-adjacent concern (TS-INV-1: this service has no Keycloak dependency and should not gain one) needs explicit sign-off before implementation. -->
