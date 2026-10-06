#!/usr/bin/env bash
# TS-INV-2: no plaintext secret is ever persisted in Postgres or logged.
# Secret material exists only in OpenBao and transiently in the TS-1
# response body (§2.5, §3.2, §11.4). This gate fails the build if a
# credential/secret/private-key name (or a `-----BEGIN` PEM literal)
# reaches a structured-log, fmt, error-constructor or span-attribute sink
# anywhere in the module (excluding tests, which may legitimately assert
# what a real logger call would NOT contain).
#
# Production logs go through platform-gincommon's Zap logger
# (port.Logger Debug/Info/Warn/Error), not slog — both call shapes are
# matched so a Zap field map cannot smuggle a secret key past this gate.
#
# The actual matching (paren-matched, multi-line, case-insensitive) lives
# in check-no-secret-log.py — a single-line `grep -E` here would miss a
# field name split across lines inside a multi-line map-literal call
# argument (this codebase's normal logging style) or a capitalized Go
# identifier such as `Secret`.
set -euo pipefail

echo "Checking TS-INV-2 (no secret material/name in a log, error or span-attribute sink)..."
python3 "$(dirname "${BASH_SOURCE[0]}")/test_check_no_secret_log.py" >/dev/null 2>&1 || {
  echo "check-no-secret-log.py self-test failed — run python3 .github/scripts/test_check_no_secret_log.py" >&2
  exit 1
}
python3 "$(dirname "${BASH_SOURCE[0]}")/check-no-secret-log.py"
