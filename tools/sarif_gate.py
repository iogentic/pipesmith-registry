#!/usr/bin/env python3
"""Gate over a SARIF file at level error, with the level rules of the
report stage of Pipesmith CI: the level of a result, else the default level
of its rule, else warning. Suppressed results count as notes.

usage: tools/sarif_gate.py <file.sarif> [--packages <list>] [--summary <text report>]

--packages names a file with one package directory of this module per line,
relative to the module root, as `go list -f {{.Dir}} ./...` lists them. Every
result must lie in a file directly in one of them; a result anywhere else
(the module cache, a worktree below work/) fails the gate, and so does an
empty list. --summary names the text report of the same gosec run; it must
say that gosec checked at least one file per listed package.

Prints every result at level error as path:line rule message, every result
outside the packages, then the counts per level. Exit code 1 when a check
fails, 0 otherwise, 2 when a file cannot be read.
"""
from __future__ import annotations

import argparse
import json
import posixpath
import re
import sys
from collections import Counter

FILES = re.compile(r"^\s*Files\s*:\s*(\d+)\s*$", re.MULTILINE)


def location(result: dict) -> tuple[str, str]:
    locations = result.get("locations") or []
    if not locations:
        return "?", "?"
    physical = locations[0].get("physicalLocation", {})
    return physical.get("artifactLocation", {}).get("uri", "?"), str(physical.get("region", {}).get("startLine", "?"))


def read(path: str) -> str:
    with open(path, encoding="utf-8") as f:
        return f.read()


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(prog="sarif_gate.py", add_help=True)
    parser.add_argument("sarif")
    parser.add_argument("--packages")
    parser.add_argument("--summary")
    args = parser.parse_args(argv[1:])
    try:
        data = json.loads(read(args.sarif))
        packages = None
        if args.packages is not None:
            packages = {line.strip().strip("/") for line in read(args.packages).splitlines() if line.strip()}
        summary = read(args.summary) if args.summary is not None else None
    except (OSError, ValueError) as err:
        print(f"sarif_gate: {err}", file=sys.stderr)
        return 2

    counts: Counter[str] = Counter()
    errors = []
    outside = []
    for run in data.get("runs", []):
        driver = run.get("tool", {}).get("driver", {})
        rules = {r.get("id"): r for r in driver.get("rules", [])}
        for result in run.get("results", []):
            uri, line = location(result)
            if packages is not None and posixpath.dirname(uri) not in packages:
                outside.append(uri)
            if result.get("suppressions"):
                counts["suppressed"] += 1
                continue
            rule = result.get("ruleId", "")
            level = (result.get("level")
                     or rules.get(rule, {}).get("defaultConfiguration", {}).get("level")
                     or "warning")
            counts[level] += 1
            if level == "error":
                errors.append(f"{uri}:{line} {rule} {result.get('message', {}).get('text', '')}")
    for text in sorted(errors):
        print(text)
    failed = bool(errors)

    if packages is not None:
        for uri in sorted(set(outside)):
            print(f"sarif_gate: a result outside the packages of this module: {uri}")
        if outside:
            failed = True
        if not packages:
            print("sarif_gate: no package of this module was listed")
            failed = True
    if summary is not None:
        found = FILES.search(summary)
        if not found:
            print(f"sarif_gate: no file count in {args.summary}")
            failed = True
        else:
            files = int(found.group(1))
            if files == 0:
                print("sarif_gate: gosec checked 0 files")
                failed = True
            elif packages and files < len(packages):
                print(f"sarif_gate: gosec checked {files} files for {len(packages)} packages")
                failed = True

    name = args.sarif.rsplit("/", 1)[-1]
    tally = ", ".join(f"{counts[k]} {k}" for k in ("error", "warning", "note", "none", "suppressed") if counts[k])
    scope = f", {len(packages)} packages" if packages is not None else ""
    print(f"sarif_gate: {name}: {tally or 'no results'}{scope}")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
