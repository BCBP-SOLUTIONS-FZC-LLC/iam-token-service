#!/usr/bin/env python3
"""TS-INV-2 gate: no credential/secret field name may reach a slog/zap/fmt
log sink anywhere in this module (excluding tests).

This replaced a single-line `grep -E` gate that could miss two real
shapes: (1) a struct-field-style key on its own line inside a multi-line
map literal — the normal call style in this codebase (e.g.
`log.Warn(msg, map[string]any{"secret": v})` split across lines) — a
single-line regex never sees the log-call token and the forbidden field
name on the same line; and (2) a capitalized Go identifier such as
`Secret`/`ClientSecret` (this service's own `IssueOrRotateResult.Secret`
field name), since the old pattern was lowercase-only.

This version finds each log-call token, then walks its actual argument
list by counting matching parens/brackets/braces (not a fixed line/char
window) so a field name anywhere inside the call — however many lines it
spans — is caught, and matches case-insensitively.
"""
import pathlib
import re
import sys

LOG_CALL = re.compile(
    r"(slog\.\w+\(|(?<!\w)log\.(?:Info|Warn|Error|Debug)\(|"
    r"fmt\.(?:Sprintf|Printf|Fprintf)\(|\.(?:Debug|Info|Warn|Error)\()"
)
FORBIDDEN = re.compile(r"(secret|plaintext|client_secret)", re.IGNORECASE)


def call_args(text: str, open_paren_index: int) -> str:
    """Return the substring between open_paren_index's '(' and its
    matching ')', honoring nested parens/brackets/braces so a multi-line
    map-literal argument is included in full."""
    depth = 0
    for i in range(open_paren_index, len(text)):
        c = text[i]
        if c in "([{":
            depth += 1
        elif c in ")]}":
            depth -= 1
            if depth == 0:
                return text[open_paren_index + 1 : i]
    # Unterminated (shouldn't happen in valid Go) — fail open to safety by
    # scanning to EOF rather than silently returning an empty match.
    return text[open_paren_index + 1 :]


def check_file(path: pathlib.Path) -> list[str]:
    text = path.read_text(encoding="utf-8", errors="replace")
    offenses = []
    for m in LOG_CALL.finditer(text):
        open_idx = m.end() - 1  # index of the call's '('
        args = call_args(text, open_idx)
        if FORBIDDEN.search(args):
            line = text.count("\n", 0, m.start()) + 1
            offenses.append(f"{path}:{line}: {m.group(0)!r} call carries a secret-named field")
    return offenses


def main() -> int:
    offenders: list[str] = []
    for path in pathlib.Path(".").rglob("*.go"):
        if path.name.endswith("_test.go"):
            continue
        if any(part in (".git", "vendor") for part in path.parts):
            continue
        offenders.extend(check_file(path))

    if offenders:
        print("FAIL: a credential/secret field name reached a slog/zap/fmt sink (TS-INV-2 violation):")
        for o in offenders:
            print(" ", o)
        return 1
    print("OK: no credential field name found as a slog/zap/fmt log attribute")
    return 0


if __name__ == "__main__":
    sys.exit(main())
