#!/usr/bin/env bash
# Copy this service's produced-event schema files into DEST, renamed to
# their PascalCase Glue name.
#
# schema-gov extract 0.4 writes snake_case filenames under
# internal/adapter/outbound/eventbus/schemas/ (§25's frozen PascalCase event names), but
# schema-gov diff/register need the PascalCase Glue name to match the name
# every other reference to these events uses (eventbus.ProducedSchemas'
# map keys, LLD §25, api/asyncapi.yaml) — so this hand-lists the
# filename->name mapping.
# A `case` statement is used instead of a bash 4+ associative array so this
# also runs under macOS's default /bin/bash 3.2, not just under CI's
# ubuntu-latest. A new schema needs one line added below.
#
# Unlike iam-realm-provisioner (shared iam-tenant-events registry with
# iam-org-membership), this service's iam-serviceaccount-events registry is
# single-producer — every schema in it belongs to this service, so there is
# no shared-registry exclusion list to carry here.
#
# Usage: stage-produced-event-schemas.sh DEST
set -euo pipefail

DEST="${1:?destination directory required}"
SRC="${SCHEMA_SRC:-internal/adapter/outbound/eventbus/schemas}"

name_for() {
  case "$1" in
    service_account_registered)          echo ServiceAccountRegistered ;;
    service_account_credential_issued)   echo ServiceAccountCredentialIssued ;;
    service_account_credential_rotated)  echo ServiceAccountCredentialRotated ;;
    service_account_credential_revoked)  echo ServiceAccountCredentialRevoked ;;
    service_account_revoked)             echo ServiceAccountRevoked ;;
    *)                                   echo "" ;;
  esac
}

mkdir -p "$DEST"

shopt -s nullglob
copied=0
for file in "$SRC"/*.json; do
  stem=$(basename "$file" .json)
  name=$(name_for "$stem")
  [ -n "$name" ] || continue
  cp "$file" "$DEST/${name}.json"
  copied=$((copied + 1))
done

if [ "$copied" -eq 0 ]; then
  echo "no produced schemas staged into $DEST" >&2
  exit 1
fi
echo "staged $copied produced schema(s) into $DEST"
