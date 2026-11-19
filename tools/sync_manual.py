#!/usr/bin/env python3
"""Copies the manual set from docs/ into internal/ui/manual/, which the binary embeds.

usage: python3 tools/sync_manual.py [--check]

The manual set follows the rule manualSet of internal/ui/manual.go, read from
that file at run time: "*.ext" is a file directly in docs/ with that ending,
"dir/*.ext" a file directly in docs/dir/ with that ending, "dir/**" every file
below docs/dir/. Names that start with "." or "_" are left out, as go:embed
leaves them out. A file whose ending has no content type in manualTypes of
internal/ui/manual.go is refused with its path, because the server does not
start with such a file embedded; nothing is copied then. The copy removes files of
internal/ui/manual/ that are not in the set. --check changes nothing, prints
every path that is missing, extra or different, and exits 1 when there is one.
"""
from __future__ import annotations

import fnmatch
import re
import shutil
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DOCS = ROOT / "docs"
TARGET = ROOT / "internal" / "ui" / "manual"
MANUAL_GO = ROOT / "internal" / "ui" / "manual.go"


def go_strings(name: str) -> list[str]:
    """The quoted values of `var <name> = []string{...}` in internal/ui/manual.go."""
    text = MANUAL_GO.read_text(encoding="utf-8")
    found = re.search(rf"^var {name} = \[\]string\{{([^}}]*)\}}", text, re.MULTILINE)
    if found is None:
        raise SystemExit(f"{MANUAL_GO.relative_to(ROOT)} holds no var {name} = []string{{...}}")
    return re.findall(r'"([^"]*)"', found.group(1))


def content_types() -> set[str]:
    """The endings of manualTypes in internal/ui/manual.go."""
    text = MANUAL_GO.read_text(encoding="utf-8")
    found = re.search(r"^var manualTypes = map\[string\]string\{(.*?)^\}", text, re.MULTILINE | re.DOTALL)
    if found is None:
        raise SystemExit(f"{MANUAL_GO.relative_to(ROOT)} holds no var manualTypes = map[string]string{{...}}")
    return set(re.findall(r'^\s*"(\.[A-Za-z0-9]+)":', found.group(1), re.MULTILINE))


def untyped(source: dict[str, Path]) -> list[str]:
    endings = content_types()
    return [name for name in source if Path(name).suffix not in endings]


def hidden(relative: Path) -> bool:
    return any(part.startswith((".", "_")) for part in relative.parts)


def manual_set(docs: Path = DOCS) -> dict[str, Path]:
    """The files of the set by their path below docs/, with "/" as separator."""
    found: dict[str, Path] = {}
    for rule in go_strings("manualSet"):
        if rule.endswith("/**"):
            folder = docs / rule[:-3]
            paths = folder.rglob("*") if folder.is_dir() else []
        else:
            folder_name, _, glob = rule.rpartition("/")
            folder = docs / folder_name if folder_name else docs
            paths = (path for path in folder.iterdir() if fnmatch.fnmatchcase(path.name, glob)) if folder.is_dir() else []
        for path in paths:
            relative = path.relative_to(docs)
            if path.is_file() and not hidden(relative):
                found[relative.as_posix()] = path
    return dict(sorted(found.items()))


def copied(target: Path = TARGET) -> dict[str, Path]:
    if not target.is_dir():
        return {}
    return {path.relative_to(target).as_posix(): path for path in sorted(target.rglob("*")) if path.is_file()}


def differences(source: dict[str, Path], copy: dict[str, Path]) -> list[str]:
    lines = [f"missing: {name}" for name in source if name not in copy]
    lines += [f"extra: {name}" for name in copy if name not in source]
    lines += [f"differs: {name}" for name in source
              if name in copy and source[name].read_bytes() != copy[name].read_bytes()]
    return sorted(lines, key=lambda line: line.split(": ", 1)[1])


def main(argv: list[str]) -> int:
    if argv not in ([], ["--check"]):
        print(__doc__)
        return 2
    source, copy = manual_set(DOCS), copied(TARGET)
    refused = untyped(source)
    if refused:
        for name in refused:
            print(f"refused: docs/{name}: its ending has no content type in manualTypes of internal/ui/manual.go")
        return 1
    found = differences(source, copy)
    if argv == ["--check"]:
        for line in found:
            print(line)
        return 1 if found else 0
    for name, path in source.items():
        target = TARGET / name
        if name in copy and copy[name].read_bytes() == path.read_bytes():
            continue
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(path, target)
    for name, path in copy.items():
        if name not in source:
            path.unlink()
    for folder in sorted((p for p in TARGET.rglob("*") if p.is_dir()), key=lambda p: len(p.parts), reverse=True):
        if not any(folder.iterdir()):
            folder.rmdir()
    for line in found:
        print(line)
    print(f"{len(source)} files in {TARGET.relative_to(ROOT)}, {len(found)} changed")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
