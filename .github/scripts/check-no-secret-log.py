#!/usr/bin/env python3
"""TS-INV-2 gate: no credential/secret material — or a field named after it —
may reach a log, error or span-attribute sink anywhere in this module
(excluding tests).

This replaced a single-line `grep -E` gate that could miss two real
shapes: (1) a struct-field-style key on its own line inside a multi-line
map literal — the normal call style in this codebase (e.g.
`log.Warn(msg, map[string]any{"secret": v})` split across lines) — a
single-line regex never sees the log-call token and the forbidden field
name on the same line; and (2) a capitalized Go identifier such as
`Secret`/`ClientSecret` (this service's own `IssueOrRotateResult.Secret`
field name), since the old pattern was lowercase-only.

It finds each sink-call token, then walks its actual argument list by
counting matching parens/brackets/braces (not a fixed line/char window) so
a name anywhere inside the call — however many lines it spans — is caught,
case-insensitively.

Since EXT-6 the material is an RSA private key in PEM form, held in
variables like `pemStr`/`priv` that the original `secret|plaintext` names
never matched, and it can escape through more than a log line: an error
string (`fmt.Errorf`/`errors.New`, which callers log or return in a 5xx
body) or a span attribute (`attribute.String`, `SetAttributes`, exported to
the tracing backend). So:

  sinks     log calls (slog/zap/gincommon logger, fmt.Sprintf/Printf/
            Fprintf), error constructors (fmt.Errorf, errors.New, .Errorf)
            and span attributes (attribute.*, .SetAttributes)
  names     secret | plaintext | client_secret | pem | private |
            priv(ate)?_?key | key_?material, plus the bare word `priv` in
            an identifier

Within a sink call's arguments:

  1. a `-----BEGIN` literal anywhere is always an offense (PEM text);
  2. any Go identifier/selector outside string literals matching a name
     (`pemStr`, `res.Secret`, `privKey`) is an offense — that is a value
     flowing into the sink;
  3. any string literal in a KEY position matching a name is an offense:
     a map key (`"private_key": v`), a slog-style key argument, or the key
     of an `attribute.*` call;
  4. the leading MESSAGE literal of a log/error call (its prose) is only an
     offense when a name is directly followed by a value verb
     (`"pem=%s"`, `"secret: %v"`) — prose such as
     "OpenBao material is not PEM-encoded" or "no secret at path" names the
     concept, not the material, and must not fail the gate.

ALLOWED_IDENTIFIERS is the narrow escape hatch for an identifier whose
name matches but which can never hold material; each entry says why.
"""
import pathlib
import re
import sys

SINK_CALL = re.compile(
    r"(slog\.\w+\(|(?<!\w)log\.(?:Info|Warn|Error|Debug)\(|"
    r"fmt\.(?:Sprintf|Printf|Fprintf|Errorf)\(|errors\.New\(|"
    r"\.(?:Debug|Info|Warn|Error|Errorf)\(|"
    r"(?<!\w)attribute\.\w+\(|\.SetAttributes\()"
)
FORBIDDEN = re.compile(
    r"(secret|plaintext|client_secret|pem|private|priv(?:ate)?_?key|key_?material)",
    re.IGNORECASE,
)
PEM_ARMOR = re.compile(r"-----BEGIN")
# A name directly followed by a value verb inside a message literal:
# "pem=%s", "secret: %v", "private key %q". %w (a wrapped error) is not a
# value of the named thing, so it is not matched.
MESSAGE_LEAK = re.compile(
    r"(secret|plaintext|pem|private|priv(?:ate)?_?key|key_?material)"
    r"[\w ]{0,8}?\s*[:=]?\s*%[-+# 0-9.*]*[vsqxXT]",
    re.IGNORECASE,
)
IDENTIFIER = re.compile(r"[A-Za-z_][A-Za-z0-9_]*")
# The bare abbreviation `priv` as a whole camelCase/snake_case word of an
# identifier (`priv`, `rsaPriv`, `privPEM`, `priv_k`) — the usual Go name for
# an *rsa.PrivateKey. Case-sensitive word boundaries keep `privilege` out.
PRIV_WORD = re.compile(r"(?:^|_)priv(?=$|_|[A-Z0-9])|(?<=[a-z0-9])Priv(?=$|_|[A-Z0-9])|^Priv(?=$|_|[A-Z0-9])")

# Identifiers whose name matches FORBIDDEN but which cannot hold material.
ALLOWED_IDENTIFIERS = {
    # openbao/client.go: the constant FIELD NAME ("secret") the material is
    # stored under in the KV entry, quoted in its "field missing" error.
    "secretDataKey",
}


def call_args(text: str, open_paren_index: int) -> str:
    """Return the substring between open_paren_index's '(' and its
    matching ')', honoring nested parens/brackets/braces so a multi-line
    map-literal argument is included in full."""
    depth = 0
    i = open_paren_index
    while i < len(text):
        c = text[i]
        if c in "\"'`":
            i = _skip_literal(text, i)
            continue
        if c in "([{":
            depth += 1
        elif c in ")]}":
            depth -= 1
            if depth == 0:
                return text[open_paren_index + 1 : i]
        i += 1
    # Unterminated (shouldn't happen in valid Go) — fail open to safety by
    # scanning to EOF rather than silently returning an empty match.
    return text[open_paren_index + 1 :]


def _skip_literal(text: str, start: int) -> int:
    """Index just past the Go string/rune literal starting at start."""
    quote = text[start]
    i = start + 1
    while i < len(text):
        c = text[i]
        if c == "\\" and quote != "`":
            i += 2
            continue
        if c == quote:
            return i + 1
        if c == "\n" and quote != "`":
            return i  # malformed; stop at end of line
        i += 1
    return len(text)


def split_args(args: str) -> tuple[list[tuple[str, int]], str]:
    """Return (string literals with their end offset, code with literal
    contents blanked out)."""
    literals: list[tuple[str, int]] = []
    code = []
    i = 0
    while i < len(args):
        c = args[i]
        if c in "\"`":
            end = _skip_literal(args, i)
            literals.append((args[i:end], end))
            code.append(" " * (end - i))
            i = end
            continue
        if c == "'":
            end = _skip_literal(args, i)
            code.append(" " * (end - i))
            i = end
            continue
        if c == "/" and args.startswith("//", i):
            end = args.find("\n", i)
            end = len(args) if end == -1 else end
            code.append(" " * (end - i))
            i = end
            continue
        code.append(c)
        i += 1
    return literals, "".join(code)


def offenses_in(call: str, args: str) -> list[str]:
    found = []
    if PEM_ARMOR.search(args):
        found.append("a PEM '-----BEGIN' literal")
    literals, code = split_args(args)

    for ident in IDENTIFIER.findall(code):
        if ident in ALLOWED_IDENTIFIERS:
            continue
        if FORBIDDEN.search(ident) or PRIV_WORD.search(ident):
            found.append(f"identifier {ident!r}")

    is_attribute = call.startswith("attribute.") or call.endswith("SetAttributes(")
    leading = len(args) - len(args.lstrip())
    for idx, (lit, end) in enumerate(literals):
        is_message = (
            idx == 0
            and not is_attribute
            and end - len(lit) == leading  # the literal is the first argument
        )
        if is_message:
            if MESSAGE_LEAK.search(lit):
                found.append(f"message {lit!r} formats a secret-named value")
            continue
        if FORBIDDEN.search(lit):
            found.append(f"key {lit!r}")
    return found


def check_file(path: pathlib.Path) -> list[str]:
    text = path.read_text(encoding="utf-8", errors="replace")
    offenses = []
    for m in SINK_CALL.finditer(text):
        open_idx = m.end() - 1  # index of the call's '('
        args = call_args(text, open_idx)
        for what in offenses_in(m.group(0), args):
            line = text.count("\n", 0, m.start()) + 1
            offenses.append(f"{path}:{line}: {m.group(0)!r} call carries {what}")
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
        print("FAIL: credential/secret material or a secret-named field reached a log/error/span sink (TS-INV-2 violation):")
        for o in offenders:
            print(" ", o)
        return 1
    print("OK: no credential material or secret-named field found in a log/error/span sink")
    return 0


if __name__ == "__main__":
    sys.exit(main())
