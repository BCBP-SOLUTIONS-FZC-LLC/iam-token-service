#!/usr/bin/env bash
# TS-INV-1: this service never writes Keycloak — it has no gocloak
# dependency and no Keycloak Admin credential (§2.5, §3.2). This is the
# inverse of the Realm Provisioner's gate (RP *requires* gocloak; this
# service *forbids* it), making TS-INV-1 structural rather than
# conventional.
set -euo pipefail

echo "Checking TS-INV-1 (no Keycloak writer)..."
offenders=$(grep -rl -e "gocloak" -e "Nerzal/gocloak" --include="*.go" . 2>/dev/null || true)
if [ -n "$offenders" ]; then
  echo "FAIL: gocloak/Keycloak-Admin import found (TS-INV-1 violation) in:"
  echo "$offenders"
  exit 1
fi
echo "OK: no gocloak/Keycloak-Admin import anywhere in the module"
