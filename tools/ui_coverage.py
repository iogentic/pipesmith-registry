#!/usr/bin/env python3
"""Writes testdata/ui/coverage.json from docs/ui/API.md and docs/ui/INVENTORY.md.

usage: python3 tools/ui_coverage.py [--check]

The file is the coverage of task 1 of the phase 6 plan, built by the rulings
U1 to U6 of leg 6-ui:

- rows: the 83 rows of INVENTORY.md in file order as S01 to S83, each with
  its capture number, its section, its status, the contract screens of a
  row with the status build (as ui:S01) and the groups of their requests.
- requests: every request of API.md, read from the column Request of the
  tables of the sections Session Endpoints, Screens, Frame and Areas, the
  query removed. The section Proposed Paths repeats them as a list and the
  section Mock is not part of the contract; both are left out.
- groups: one group per request. A request that the server refused when it
  first answered the contract has the area of that refusal (AREAS). Any
  other request has the group of its first screen in file order, where a row
  whose use is "choices of" a field does not count (SCREEN_GROUPS); each such
  choice is listed under "choices".
- served: the requests the route table serves up to this phase (SERVED),
  by group. The Go test of internal/server compares it with Routes(true).
- contradictions: what disagrees between the sources, found while reading.

sources holds the SHA-256 of both inputs and of this file, and digest the
SHA-256 of the output with the digest written as 64 zeros, so the Go test
can tell that the file is the output of this generator. --check writes
nothing and exits 1 when the file differs from what the generator writes.
"""
from __future__ import annotations

import hashlib
import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
API = "docs/ui/API.md"
INVENTORY = "docs/ui/INVENTORY.md"
GENERATOR = "tools/ui_coverage.py"
OUTPUT = "testdata/ui/coverage.json"
PLAN = "the plan of phase 6"

REQUEST = re.compile(r"`((?:GET|HEAD|POST|PUT|PATCH|DELETE) /[^`\s]*)`")
SCREEN = re.compile(r"\bS[0-9]{2}\b")
LEFT_OUT = {"Proposed Paths", "Mock"}

# The areas of the refusals with which the server first answered the
# contract at 1e6d884 (refusalTable in internal/server/secure.go of commit
# 0923869), in the spelling of the contract: {name+} is {name}, (a|b) one
# request per alternative. Today GET /api/areas names the areas of the
# requests that are still refused, with the same values.
AREAS = {
    "DELETE /service/rest/v1/assets/{id}": "content",
    "GET /service/rest/v1/formats/{format}/upload-specs": "content",
    "POST /service/rest/v1/components": "content",
    "POST /service/rest/v1/repositories/{segment}/{type}": "repositories",
    "PUT /service/rest/v1/repositories/{segment}/{type}/{name}": "repositories",
    "DELETE /service/rest/v1/repositories/{name}": "repositories",
    "POST /service/rest/v1/repositories/{name}/rebuild-index": "repositories",
    "POST /service/rest/v1/repositories/{name}/invalidate-cache": "repositories",
    "GET /api/cleanup-policies": "cleanup-policies",
    "POST /api/cleanup-policies": "cleanup-policies",
    "POST /api/cleanup-policies/preview": "cleanup-policies",
    "GET /api/cleanup-policies/{name}": "cleanup-policies",
    "PUT /api/cleanup-policies/{name}": "cleanup-policies",
    "DELETE /api/cleanup-policies/{name}": "cleanup-policies",
    "GET /service/rest/v1/routing-rules": "routing-rules",
    "POST /service/rest/v1/routing-rules": "routing-rules",
    "GET /service/rest/v1/routing-rules/{name}": "routing-rules",
    "PUT /service/rest/v1/routing-rules/{name}": "routing-rules",
    "DELETE /service/rest/v1/routing-rules/{name}": "routing-rules",
    "POST /api/routing-rules/test": "routing-rules",
    "POST /api/routing-rules/preview": "routing-rules",
    "POST /service/rest/v1/blobstores/file": "blob-stores",
    "PUT /service/rest/v1/blobstores/file/{name}": "blob-stores",
    "DELETE /service/rest/v1/blobstores/{name}": "blob-stores",
    "GET /api/blob-stores/{name}/deleted": "blob-stores",
    "POST /service/rest/v1/security/roles": "security",
    "PUT /service/rest/v1/security/roles/{id}": "security",
    "DELETE /service/rest/v1/security/roles/{id}": "security",
    "POST /service/rest/v1/security/users": "security",
    "PUT /service/rest/v1/security/users/{userId}": "security",
    "DELETE /service/rest/v1/security/users/{userId}": "security",
    "PUT /service/rest/v1/security/users/{userId}/change-password": "security",
    "PUT /service/rest/v1/security/anonymous": "security",
    "PUT /service/rest/v1/security/realms/active": "security",
    "POST /service/rest/v1/security/ldap": "ldap",
    "POST /service/rest/v1/security/ldap/change-order": "ldap",
    "PUT /service/rest/v1/security/ldap/{name}": "ldap",
    "DELETE /service/rest/v1/security/ldap/{name}": "ldap",
    "POST /api/ldap/clear-cache": "ldap",
    "POST /api/ldap/verify-connection": "ldap",
    "POST /api/ldap/verify-user-mapping": "ldap",
    "POST /api/ldap/verify-login": "ldap",
    "GET /api/tokens": "tokens",
    "POST /api/tokens": "tokens",
    "DELETE /api/tokens/{id}": "tokens",
    "PUT /api/account": "account",
    "POST /api/account/password": "account",
    "GET /api/logging/loggers": "logging",
    "PUT /api/logging/loggers/{name}": "logging",
    "DELETE /api/logging/loggers/{name}": "logging",
    "POST /api/logging/reset": "logging",
    "GET /api/logs": "logs",
    "GET /api/logs/{name}": "logs",
    "GET /service/rest/beta/system/information": "system",
    "GET /service/rest/v1/capabilities": "capabilities",
    "PUT /service/rest/v1/capabilities/{id}": "capabilities",
    "DELETE /service/rest/v1/capabilities/{id}": "capabilities",
    "GET /api/capabilities/support": "capabilities",
    "GET /service/rest/v1/system/node": "system",
    "POST /service/rest/v1/read-only/freeze": "system",
    "POST /service/rest/v1/read-only/release": "system",
    "POST /service/rest/v1/read-only/force-release": "system",
    "GET /service/rest/v1/tasks": "tasks",
    "GET /service/rest/v1/tasks/{id}": "tasks",
    "POST /service/rest/v1/tasks/{id}/run": "tasks",
    "POST /service/rest/v1/tasks/{id}/stop": "tasks",
    "POST /api/tasks": "tasks",
    "GET /api/tasks/{id}": "tasks",
    "PUT /api/tasks/{id}": "tasks",
    "DELETE /api/tasks/{id}": "tasks",
    "GET /api/task-types": "tasks",
    "GET /api/metrics/requests": "metrics",
    "GET /api/metrics/downloads": "metrics",
}

# The group of each place of the contract that holds requests, for the
# requests without an area: the area of the screen where one fits, else
# session, status, search or browse. The keys are the section of API.md
# (Session Endpoints, Frame, Areas) or the contract screen.
SCREEN_GROUPS = {
    "Session Endpoints": "session",
    "Frame": "session",
    "Areas": "session",
    "ui:S01": "session",
    "ui:S02": "status",
    "ui:S03": "search",
    "ui:S04": "browse",
    "ui:S05": "browse",
    "ui:S06": "content",
    "ui:S07": "content",
    "ui:S09": "repositories",
    "ui:S11": "repositories",
    "ui:S12": "blob-stores",
    "ui:S13": "blob-stores",
    "ui:S14": "cleanup-policies",
    "ui:S15": "cleanup-policies",
    "ui:S16": "routing-rules",
    "ui:S17": "routing-rules",
    "ui:S18": "routing-rules",
    "ui:S19": "security",
    "ui:S20": "security",
    "ui:S21": "security",
    "ui:S22": "security",
    "ui:S23": "security",
    "ui:S24": "security",
    "ui:S25": "security",
    "ui:S26": "ldap",
    "ui:S27": "ldap",
    "ui:S28": "security",
    "ui:S29": "tokens",
    "ui:S30": "logging",
    "ui:S31": "logs",
    "ui:S32": "logs",
    "ui:S33": "status",
    "ui:S34": "system",
    "ui:S35": "capabilities",
    "ui:S36": "capabilities",
    "ui:S37": "system",
    "ui:S38": "tasks",
    "ui:S39": "tasks",
    "ui:S40": "tasks",
    "ui:S41": "account",
    "ui:S42": "status",
    "ui:S43": "metrics",
}

# The requests of the contract that the route table serves up to this
# phase, read from Routes(true) on 2026-11-20 at 50c4ee4, with the log and
# figure routes of phase 7 (597a364, 90a97ff) and the node id of F2. The Go
# test fails when this list and Routes(true) differ.
SERVED = {
    "DELETE /api/logging/loggers/{name}",
    "DELETE /api/session",
    "DELETE /service/rest/v1/assets/{id}",
    "DELETE /service/rest/v1/blobstores/{name}",
    "DELETE /service/rest/v1/capabilities/{id}",
    "DELETE /service/rest/v1/repositories/{name}",
    "DELETE /service/rest/v1/routing-rules/{name}",
    "DELETE /service/rest/v1/security/ldap/{name}",
    "DELETE /service/rest/v1/security/roles/{id}",
    "DELETE /service/rest/v1/security/users/{userId}",
    "GET /api/areas",
    "GET /api/asset/{repository}",
    "GET /api/blob-stores/{name}/deleted",
    "GET /api/browse/{repository}",
    "GET /api/capabilities/support",
    "GET /api/logging/loggers",
    "GET /api/logs",
    "GET /api/logs/{name}",
    "GET /api/metrics/downloads",
    "GET /api/metrics/requests",
    "GET /api/repositories",
    "GET /api/session",
    "GET /api/status",
    "GET /service/rest/beta/system/information",
    "GET /service/rest/v1/assets/{id}",
    "GET /service/rest/v1/blobstores",
    "GET /service/rest/v1/blobstores/file/{name}",
    "GET /service/rest/v1/blobstores/{name}/quota-status",
    "GET /service/rest/v1/capabilities",
    "GET /service/rest/v1/formats/{format}/upload-specs",
    "GET /service/rest/v1/read-only",
    "GET /service/rest/v1/repositories",
    "GET /service/rest/v1/repositories/{name}",
    "GET /service/rest/v1/repositories/{segment}/{type}/{name}",
    "GET /service/rest/v1/repositorySettings",
    "GET /service/rest/v1/routing-rules",
    "GET /service/rest/v1/routing-rules/{name}",
    "GET /service/rest/v1/search",
    "GET /service/rest/v1/security/anonymous",
    "GET /service/rest/v1/security/ldap",
    "GET /service/rest/v1/security/ldap/{name}",
    "GET /service/rest/v1/security/privileges",
    "GET /service/rest/v1/security/privileges/{name}",
    "GET /service/rest/v1/security/realms/active",
    "GET /service/rest/v1/security/realms/available",
    "GET /service/rest/v1/security/roles",
    "GET /service/rest/v1/security/roles/{id}",
    "GET /service/rest/v1/security/user-sources",
    "GET /service/rest/v1/security/users",
    "GET /service/rest/v1/status/check",
    "GET /service/rest/v1/status/writable",
    "GET /service/rest/v1/system/node",
    "GET /service/rest/v1/tasks",
    "GET /service/rest/v1/tasks/{id}",
    "POST /api/ldap/clear-cache",
    "POST /api/ldap/verify-connection",
    "POST /api/ldap/verify-login",
    "POST /api/ldap/verify-user-mapping",
    "POST /api/logging/reset",
    "POST /api/routing-rules/preview",
    "POST /api/routing-rules/test",
    "POST /api/session",
    "POST /service/rest/v1/blobstores/file",
    "POST /service/rest/v1/components",
    "POST /service/rest/v1/read-only/force-release",
    "POST /service/rest/v1/read-only/freeze",
    "POST /service/rest/v1/read-only/release",
    "POST /service/rest/v1/repositories/{name}/invalidate-cache",
    "POST /service/rest/v1/repositories/{name}/rebuild-index",
    "POST /service/rest/v1/repositories/{segment}/{type}",
    "POST /service/rest/v1/routing-rules",
    "POST /service/rest/v1/security/ldap",
    "POST /service/rest/v1/security/ldap/change-order",
    "POST /service/rest/v1/security/roles",
    "POST /service/rest/v1/security/users",
    "POST /service/rest/v1/tasks/{id}/run",
    "POST /service/rest/v1/tasks/{id}/stop",
    "PUT /api/logging/loggers/{name}",
    "PUT /service/rest/v1/blobstores/file/{name}",
    "PUT /service/rest/v1/capabilities/{id}",
    "PUT /service/rest/v1/repositories/{segment}/{type}/{name}",
    "PUT /service/rest/v1/routing-rules/{name}",
    "PUT /service/rest/v1/security/anonymous",
    "PUT /service/rest/v1/security/ldap/{name}",
    "PUT /service/rest/v1/security/realms/active",
    "PUT /service/rest/v1/security/roles/{id}",
    "PUT /service/rest/v1/security/users/{userId}",
    "PUT /service/rest/v1/security/users/{userId}/change-password",
}


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def cells(line: str) -> list[str]:
    return [cell.strip() for cell in line.strip().strip("|").split("|")]


def is_rule(line: str) -> bool:
    return set(line.strip()) <= set("|- ")


def request_rows(text: str) -> list[dict]:
    """Every row of a table with a column Request, in file order."""
    rows, place, column = [], None, None
    for number, line in enumerate(text.splitlines(), 1):
        if line.startswith("#"):
            title = line.lstrip("#").strip()
            match = re.match(r"(S[0-9]{2}) ", title)
            place = "ui:" + match.group(1) if match else title
            column = None
            continue
        if not line.startswith("|"):
            column = None
            continue
        row = cells(line)
        if column is None:
            column = row.index("Request") if "Request" in row else -1
            continue
        if column < 0 or is_rule(line) or place in LEFT_OUT:
            continue
        found = REQUEST.findall(row[column])
        if len(found) != 1:
            raise SystemExit(f"{API}:{number}: {len(found)} requests in the column Request")
        use = row[column + 1] if column + 1 < len(row) else ""
        rows.append({"line": number, "place": place, "request": found[0].split("?")[0], "use": use})
    return rows


def inventory_rows(text: str) -> list[dict]:
    """The rows of the capture tables of INVENTORY.md, in file order."""
    rows, section, capture_table = [], None, False
    for line in text.splitlines():
        if line.startswith("## "):
            section, capture_table = line[3:].strip(), False
            continue
        if not line.startswith("|"):
            capture_table = False
            continue
        row = cells(line)
        if row[0] == "Capture":
            capture_table = True
            continue
        if capture_table and not is_rule(line):
            capture, _function, status, cell = row
            screens = ["ui:" + s for s in SCREEN.findall(cell)] if status == "build" else []
            rows.append({"capture": capture, "section": section, "status": status, "screens": screens})
    return rows


def build() -> bytes:
    api_bytes = (ROOT / API).read_bytes()
    inventory_bytes = (ROOT / INVENTORY).read_bytes()
    api = api_bytes.decode("utf-8")
    inventory = inventory_bytes.decode("utf-8")

    rows = request_rows(api)
    places: dict[str, list[str]] = {}
    first: dict[str, str] = {}
    for row in rows:
        places.setdefault(row["request"], [])
        if row["place"] not in places[row["request"]]:
            places[row["request"]].append(row["place"])
    for row in rows:
        if not row["use"].startswith("choices of"):
            first.setdefault(row["request"], row["place"])
    for request, where in places.items():
        first.setdefault(request, where[0])

    requests = sorted(places)
    unknown = sorted(set(AREAS) - set(requests)) + sorted(set(SERVED) - set(requests))
    if unknown:
        raise SystemExit(f"not requests of {API}: {', '.join(unknown)}")
    group_of, choices = {}, []
    for request in requests:
        if request in AREAS:
            group_of[request] = AREAS[request]
            continue
        place = first[request]
        if place not in SCREEN_GROUPS:
            raise SystemExit(f"{request}: {place} has no group in SCREEN_GROUPS")
        group_of[request] = SCREEN_GROUPS[place]
        choices.append({"request": request, "screen": place, "group": SCREEN_GROUPS[place]})

    groups: dict[str, list[str]] = {}
    for request in requests:
        groups.setdefault(group_of[request], []).append(request)
    groups = dict(sorted(groups.items()))
    screen_groups: dict[str, set[str]] = {}
    for request, where in places.items():
        for place in where:
            screen_groups.setdefault(place, set()).add(group_of[request])

    captures = inventory_rows(inventory)
    coverage_rows = []
    for index, row in enumerate(captures, 1):
        found = set()
        for screen in row["screens"]:
            found |= screen_groups.get(screen, set())
        coverage_rows.append({"id": f"S{index:02d}", "capture": row["capture"], "section": row["section"],
                              "status": row["status"], "screens": row["screens"], "groups": sorted(found)})

    served: dict[str, list[str]] = {}
    for request in sorted(SERVED):
        served.setdefault(group_of[request], []).append(request)

    document = {
        "about": "Coverage of the contract of the interface (task 1 of the phase 6 plan), written by "
                 "tools/ui_coverage.py; do not edit.",
        "sources": {API: digest(api_bytes), INVENTORY: digest(inventory_bytes),
                    GENERATOR: digest((ROOT / GENERATOR).read_bytes())},
        "digest": "0" * 64,
        "rows": coverage_rows,
        "requests": [{"request": r, "group": group_of[r], "area": AREAS.get(r), "screens": places[r],
                      "rows": sum(1 for row in rows if row["request"] == r)} for r in requests],
        "requestRows": len(rows),
        "groups": groups,
        "counts": {name: len(members) for name, members in groups.items()},
        "total": len(requests),
        "served": dict(sorted(served.items())),
        "choices": choices,
        "contradictions": contradictions(api, inventory, coverage_rows, captures),
    }
    text = json.dumps(document, indent=1, ensure_ascii=False) + "\n"
    return text.replace('"digest": "' + "0" * 64 + '"', '"digest": "' + digest(text.encode("utf-8")) + '"', 1).encode("utf-8")


def contradictions(api: str, inventory: str, rows: list[dict], captures: list[dict]) -> list[dict]:
    found: list[tuple[str, str]] = []

    # U1 against the plan: the plan numbers the captures in their own order
    moved = [row for row in rows if row["capture"] != plan_capture(int(row["id"][1:]))]
    if moved:
        example = next(row for row in moved if row["id"] == "S02") if any(r["id"] == "S02" for r in moved) else moved[0]
        found.append((f"{PLAN}, section Screens; {INVENTORY}",
                      f"The plan takes S01 to S55 as the captures 01 to 55 and S56 to S83 as the captures 57 to 84. "
                      f"INVENTORY.md lists its rows by section, so under ruling U1 {len(moved)} of the {len(rows)} ids "
                      f"name another capture than in the plan: {example['id']} is capture {example['capture']} here "
                      f"and capture {plan_capture(int(example['id'][1:]))} in the plan."))

    # the stated counts of INVENTORY.md against its rows
    stated = re.search(r"([0-9]+) rows, ([0-9]+) build, ([0-9]+) left out, ([0-9]+) outside", inventory)
    counted = (len(captures), *(sum(1 for c in captures if c["status"] == s) for s in ("build", "left out", "outside")))
    if stated is None or tuple(int(n) for n in stated.groups()) != counted:
        found.append((f"{INVENTORY}, section Method",
                      f"The file states {stated.group(0) if stated else 'no count'}; its tables hold "
                      f"{counted[0]} rows, {counted[1]} build, {counted[2]} left out, {counted[3]} outside."))

    # contract screens said to have no capture against the rows that name them
    said = re.search(r"^((?:S[0-9]{2}(?:, | and ))*S[0-9]{2}) have no capture", inventory, re.M)
    if said:
        for screen in SCREEN.findall(said.group(1)):
            holders = [row for row in rows if "ui:" + screen in row["screens"]]
            if holders:
                found.append((f"{INVENTORY}, sections Frame and Screens",
                              f"Section Screens says that {screen} has no capture, but the row of capture "
                              f"{holders[0]['capture']} ({holders[0]['id']} here) builds it."))

    # the proposed paths and section Areas against the served requests
    proposed = proposed_requests(api)
    served_proposed = sorted(proposed & SERVED)
    if served_proposed:
        found.append((f"{API}, section Proposed Paths; internal/server route table",
                      f"The section lists the paths the server does not have yet, but {len(served_proposed)} of its "
                      f"{len(proposed)} requests are served: {', '.join(served_proposed)}."))
    if "GET /api/areas" in SERVED and "the server does not have it yet" in api:
        found.append((f"{API}, section Areas; internal/server route table",
                      "The section says that the server does not have GET /api/areas yet; the server answers it "
                      "since commit 0923869 of 2026-11-12."))

    # the screens of the contract against the screens served here
    found.append((f"{API}, section Kinds; internal/ui/LOCAL_CHANGES.md",
                  "The contract names the screens S01 to S45; internal/ui/web/js/routes.js adds S46, the viewer of "
                  "Browse, which the contract does not list."))
    found.append((f"{API}, section Preview; internal/ui/LOCAL_CHANGES.md",
                  "The section says that the probe of every served read leaves the browser; after the local change "
                  "of js/preview.js and js/app.js no probe leaves it while GET /api/areas answers."))

    # the groups of the plan against ruling U5
    found.append((f"{PLAN}, section Screens; ruling U5",
                  "The plan names the groups of a screen session, port, read, system, figures, content, "
                  "repositories, stores, policies, rules, cache, security, account, logs and tasks. Under ruling U5 "
                  "the groups are the areas of the refusals, so coverage.json uses other names "
                  "(blob-stores, cleanup-policies, routing-rules, ldap, tokens, logging, capabilities, metrics) "
                  "and session, status, search and browse for screens without an area."))
    found.append(("internal/server/secure.go, refusalTable; ruling U5",
                  "GET /api/areas names only the areas of the requests that are still refused. The areas of the "
                  "requests served since are taken from the refusal table of commit 0923869, the first answer "
                  "of the server to the contract at 1e6d884."))
    return [{"id": f"K{i}", "source": source, "text": text} for i, (source, text) in enumerate(found, 1)]


def plan_capture(number: int) -> str:
    """The capture of a neutral id as section Screens of the plan counts it."""
    return f"{number if number <= 55 else number + 1:02d}"


def proposed_requests(api: str) -> set[str]:
    """The requests of section Proposed Paths; a bare method takes the path that follows it."""
    section = api.split("## Proposed Paths", 1)[1].split("\n## ", 1)[0]
    found = set()
    for line in section.splitlines():
        if not line.startswith("|") or is_rule(line) or cells(line)[0].startswith("field"):
            continue
        pending = []
        for part in re.findall(r"`([^`]+)`", cells(line)[0]):
            if re.fullmatch(r"GET|POST|PUT|DELETE|PATCH|HEAD", part):
                pending.append(part)
                continue
            match = re.fullmatch(r"(GET|POST|PUT|DELETE|PATCH|HEAD) (/\S*)", part)
            if match:
                for method in pending + [match.group(1)]:
                    found.add(f"{method} {match.group(2)}")
                pending = []
    return found


def main(argv: list[str]) -> int:
    if argv not in ([], ["--check"]):
        print(__doc__.split("\n\n")[1], file=sys.stderr)
        return 2
    data = build()
    target = ROOT / OUTPUT
    if argv == ["--check"]:
        if not target.is_file() or target.read_bytes() != data:
            print(f"{OUTPUT} is not the output of {GENERATOR}; run python3 {GENERATOR}", file=sys.stderr)
            return 1
        print(f"{OUTPUT} is current")
        return 0
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes(data)
    document = json.loads(data)
    print(f"{OUTPUT}: {len(document['rows'])} rows, {document['total']} requests in {len(document['groups'])} groups, "
          f"{sum(len(v) for v in document['served'].values())} served, {len(document['contradictions'])} contradictions")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
