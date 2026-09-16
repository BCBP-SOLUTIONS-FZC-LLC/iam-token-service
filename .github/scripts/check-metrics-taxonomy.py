#!/usr/bin/env python3
"""Enterprise Platform Observability Standard — static naming-convention
gate. Runs against every prometheus.{Counter,Histogram,Gauge}Opts literal
in the module (excluding tests) and enforces:

  1. Namespace classification: every metric name this service DEFINES must
     start with "platform_" (Tier 1), "iam_" (Tier 2/3 — this covers both
     "iam_<metric>" and "iam_token_service_<metric>", since Tier 3 is
     "iam_" with the service name appended), or be a metric this service
     re-exports from a shared platform-* library it does not itself
     register (out of this gate's scope — it only walks CounterOpts/
     HistogramOpts/GaugeOpts literals, which only exist in code that
     constructs its OWN metric).
  2. Naming rules #4/#5: a name built from CounterOpts must end in
     "_total"; a name built from HistogramOpts must end in "_seconds".
  3. Naming rule #1/#2: platform_*/iam_* metric names must never embed
     this service's own name ("token_service"/"token-service") — that is
     exactly what labels are for (rule #3), not the metric name.

This is a structural/static check, not a live-registry one — it cannot see
which labels a metric actually carries at runtime (ConstLabels are applied
in Go code, not in the Opts literal's static text in every case). The
required-label contract (domain/service/environment per tier) is verified
by internal/adapter/outbound/metrics/metrics_test.go's
TestTier1Labels_CarryDomainServiceEnvironment /
TestTier2Labels_CarryServiceEnvironmentOnly instead, against the real
registered collectors.
"""
import pathlib
import re
import sys
from typing import Iterator

OPTS_RE = re.compile(
    r"prometheus\.(Counter|Histogram|Gauge)Opts\{"
    r"(?P<body>(?:[^{}]|\{[^{}]*\})*)"
    r"\}",
    re.S,
)
NAME_RE = re.compile(r'Name:\s*(?:tier1Prefix|tier2Prefix|tier3Prefix|prefix)?\s*\+?\s*"([^"]*)"')
# Matches `Name: prefix + "foo"` (this file's own convention) as well as a
# bare `Name: "platform_foo"` literal — either way, the check below
# reconstructs the intended full name using the const each file defines.

VALID_PREFIXES = ("platform_", "iam_")
FORBIDDEN_NAME_SUBSTRINGS = ("token_service", "token-service")


def full_names_in(path: pathlib.Path, body: str) -> Iterator[str]:
    """Resolve `Name: <prefixConst> + "suffix"` to a full literal name using
    this file's own `const <prefixConst> = "..."` definitions, so the check
    doesn't need to hardcode every service's prefix constant name."""
    text = path.read_text(encoding="utf-8", errors="replace")
    consts: dict[str, str] = {}
    for m in re.finditer(r'(\w+Prefix)\s*=\s*"([^"]*)"', text):
        consts[m.group(1)] = m.group(2)
    for m in re.finditer(r'Name:\s*([\w]+)\s*\+\s*"([^"]*)"', body):
        prefix_const, suffix = m.group(1), m.group(2)
        yield consts.get(prefix_const, "") + suffix
    for m in re.finditer(r'Name:\s*"([^"]+)"\s*,', body):
        # A bare literal name with no "+" concatenation.
        if "+" not in body[max(0, m.start() - 20) : m.start()]:
            yield m.group(1)


def check_file(path: pathlib.Path) -> list[str]:
    text = path.read_text(encoding="utf-8", errors="replace")
    offenses = []
    for m in OPTS_RE.finditer(text):
        kind = m.group(1)  # Counter | Histogram | Gauge
        body = m.group("body")
        for name in full_names_in(path, body):
            if not name:
                continue
            line = text.count("\n", 0, m.start()) + 1
            if not name.startswith(VALID_PREFIXES):
                offenses.append(f"{path}:{line}: metric {name!r} does not start with an approved tier prefix ({'/'.join(VALID_PREFIXES)})")
            if kind == "Counter" and not name.endswith("_total"):
                offenses.append(f"{path}:{line}: counter {name!r} must end in _total (rule #4)")
            if kind == "Histogram" and not name.endswith("_seconds"):
                offenses.append(f"{path}:{line}: histogram {name!r} must end in _seconds (rule #5)")
            if name.startswith(VALID_PREFIXES) and any(sub in name for sub in FORBIDDEN_NAME_SUBSTRINGS) and not name.startswith("iam_token_service_"):
                offenses.append(f"{path}:{line}: metric {name!r} appears to embed a service name into a shared (platform_/iam_) metric — use a label instead (rules #1/#2)")
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
        print("FAIL: metric naming does not comply with the Enterprise Platform Observability Standard:")
        for o in offenders:
            print(" ", o)
        return 1
    print("OK: every defined metric name complies with the platform/domain/service-specific naming rules")
    return 0


if __name__ == "__main__":
    sys.exit(main())
