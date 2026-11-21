#!/usr/bin/env python3
"""Generates docs/features.md: what each LocalStack Community service has built
in gostack, what is partial and what is not started.

    python3 parity/features.py write --facts FILE   rewrite docs/features.md
    python3 parity/features.py check --facts FILE   fail when docs/features.md differs

`make features` and `make features-check` run these with the facts file the Go
test Test_featureFacts (cmd/gostack) writes: for every registered service, its
operations, the ones it implements and whether it has state PERSIST_<SERVICE>
saves. The rest is read from the repository: parity/features-services.json (the
service directories of the pinned LocalStack checkout), parity/ledger/ (through
ledger.py) and docs/parity.md and docs/compat.md, which the page links to and
never copies. Standard library only.

The page states only what these inputs prove. Where they cannot tell, it says
less: an unknown operation count is "unknown", never zero, and a service with no
compat.md entry found is not said to have no differences.
"""

from __future__ import annotations

import argparse
import difflib
import json
import re
import sys
from collections import defaultdict
from pathlib import Path

import ledger

ROOT = ledger.ROOT
SERVICES_FILE = ROOT / "parity" / "features-services.json"
PAGE = ROOT / "docs" / "features.md"
PARITY_DOC = ledger.DOCS
COMPAT_DOC = ROOT / "docs" / "compat.md"

NOT_STARTED = "Not started"
IN_PROGRESS = "In progress"
PARITY_DONE = "Parity done"
COMPLETE = "Complete"

# The marks a row of the summary carries beside its status.
ENGINE_DEFERRED = "engine deferred"
PROVIDERS_ONLY = "CloudFormation resource providers only"

NOT_STARTED_REASON = "No code yet."
HEADING = re.compile(r"^(#{1,6})[ \t]+(.+?)[ \t#]*$")
FENCE = re.compile(r"^[ \t]*(```|~~~)")
HTML_ANCHOR = re.compile(r"""<a\s+(?:id|name)="([^"]+)"\s*>""")
FIRST_WORD = re.compile(r"^[A-Z][a-z0-9]*")
DEPENDS_ON = re.compile(r"depends-on:([\w-]+)")


class FeaturesError(Exception):
    """The inputs cannot produce a page."""


def slugs(markdown: str) -> list[tuple[int, str, str]]:
    """The headings of a markdown document as (level, text, anchor), the anchor
    as GitHub makes it: lower case, punctuation other than hyphen and
    underscore dropped, spaces to hyphens, and -1, -2 for a repeat."""
    found, seen, fenced = [], defaultdict(int), False
    for line in markdown.splitlines():
        if FENCE.match(line):
            fenced = not fenced
            continue
        match = None if fenced else HEADING.match(line)
        if not match:
            continue
        text = match.group(2).replace("`", "")
        base = re.sub(r"[^\w\- ]", "", text.lower()).replace(" ", "-")
        anchor = base if seen[base] == 0 else f"{base}-{seen[base]}"
        seen[base] += 1
        found.append((len(match.group(1)), text, anchor))
    return found


def anchors(markdown: str) -> set[str]:
    """Every anchor a link into the document can reach: its headings' and the
    ids of its `<a id="...">` tags, which GitHub keeps."""
    return {anchor for _, _, anchor in slugs(markdown)} | set(HTML_ANCHOR.findall(markdown))


def compat_mentions(services: list[dict], compat: str) -> dict[str, list[tuple[str, str, bool]]]:
    """For each service, the headings of docs/compat.md that hold a heading or
    a table row naming it, as (text, anchor, own): the heading the line is, or
    the nearest one above it, and whether a heading names it. A service is named by its `compat` titles, or by its name when it has
    none, as a whole word in any case. A name inside a longer service's title is
    that service's: "DynamoDB Streams" is not also a DynamoDB mention."""
    terms = sorted(
        ((title, entry["name"]) for entry in services for title in entry.get("compat", [entry["name"]])),
        key=lambda term: -len(term[0]),
    )
    patterns = [(re.compile(rf"(?<![\w-]){re.escape(title)}(?![\w-])", re.IGNORECASE), owner) for title, owner in terms]
    found: dict[str, list[tuple[str, str, bool]]] = {entry["name"]: [] for entry in services}
    headings = iter(slugs(compat))
    pending = next(headings, None)
    current: tuple[str, str] | None = None
    fenced = False
    for line in compat.splitlines():
        if FENCE.match(line):
            fenced = not fenced
            continue
        if fenced:
            continue
        match = HEADING.match(line)
        if match and pending is not None:
            current, pending = pending[1:], next(headings, None)
        if current is None or not (match or line.startswith("|")):
            continue
        rest = line
        for pattern, owner in patterns:
            if pattern.search(rest):
                known = found[owner]
                at = next((i for i, (text, anchor, _) in enumerate(known) if (text, anchor) == current), None)
                if at is None:
                    known.append((*current, bool(match)))
                elif match:
                    known[at] = (*current, True)
                rest = pattern.sub(" ", rest)
    return found


def depends_on(row: dict) -> str | None:
    match = DEPENDS_ON.fullmatch(row["reason"] or "") if row["localstack"] == "excluded" else None
    return match.group(1) if match else None


def missing_operations(fact: dict) -> list[str] | None:
    """The operations of the service's API it does not implement, which answer
    501; None when its implemented operations are not known."""
    if fact["implemented"] is None:
        return None
    implemented = set(fact["implemented"])
    return [operation for operation in fact["operations"] if operation not in implemented]


def assess(fact: dict | None, rows: list[dict] | None, registered: set[str]) -> tuple[str, list[str]]:
    """The status of a service and, for In progress, what keeps it there.

    - Not started: no code.
    - In progress: code, and any of: a test LocalStack passes that gostack does
      not pass; an exclusion that depends on a service gostack now registers;
      no ledger; no test LocalStack passes.
    - Parity done: code, every test LocalStack passes also passes, and no such
      exclusion.
    - Complete: Parity done, and no operation answers 501.

    A test LocalStack does not pass, or excluded, counts toward none of these.
    A service whose implemented operations are not known is not Complete."""
    if fact is None:
        return NOT_STARTED, []
    if rows is None:
        return IN_PROGRESS, ["the service has no parity ledger"]
    reasons = []
    passed = [row for row in rows if row["localstack"] == "pass"]
    failing = [row for row in passed if row["gostack"] != "pass"]
    if failing:
        reasons.append(f"{len(failing)} of the {len(passed)} tests LocalStack passes do not pass on gostack")
    if not passed:
        reasons.append("the ledger holds no test LocalStack passes")
    waiting = sorted({dep for row in rows if (dep := depends_on(row)) in registered})
    if waiting:
        count = sum(1 for row in rows if depends_on(row) in waiting)
        reasons.append(f"{count} excluded {'test waits' if count == 1 else 'tests wait'} on {', '.join(waiting)}, which gostack now serves")
    if reasons:
        return IN_PROGRESS, reasons
    missing = missing_operations(fact)
    return (COMPLETE if missing == [] else PARITY_DONE), []


def compat_lines(name: str, mentions: list[tuple[str, str, bool]]) -> list[str]:
    def links(own: bool) -> str:
        return ", ".join(f"[{text}](compat.md#{anchor})" for text, anchor, is_own in mentions if is_own == own)

    lines = []
    if any(own for *_, own in mentions):
        lines += [f"Headings of [compat.md](compat.md) that name {name}: {links(True)}.", ""]
    if any(not own for *_, own in mentions):
        lines += [f"Other headings of compat.md with a table row under them that names {name}: {links(False)}.", ""]
    return lines or [f"No heading or table row of [compat.md](compat.md) names {name}.", ""]


def groups(operations: list[str]) -> list[tuple[str, list[str]]]:
    """Operations grouped by the first word of their name, in name order."""
    by_verb: dict[str, list[str]] = defaultdict(list)
    for operation in sorted(operations):
        match = FIRST_WORD.match(operation)
        by_verb[match.group(0) if match else operation].append(operation)
    return sorted(by_verb.items())


def operation_lines(label: str, operations: list[str], empty: str) -> list[str]:
    lines = [f"**{label} ({len(operations)})**", ""]
    if not operations:
        return [*lines, empty, ""]
    for verb, names in groups(operations):
        lines.append(f"- {verb} ({len(names)}): " + ", ".join(f"`{name}`" for name in names))
    return [*lines, ""]


def operations_cell(fact: dict | None) -> str:
    if fact is None:
        return "n/a"
    if fact["implemented"] is None:
        return f"unknown of {len(fact['operations'])}"
    return f"{len(fact['implemented'])} of {len(fact['operations'])}"


def persist_variable(name: str) -> str:
    return "PERSIST_" + name.upper().replace("-", "_")


def lookup_name(entry: dict) -> str:
    """The name the registered services and the ledger know the entry by: the
    entry's `ledger` key when its directory is named otherwise (LocalStack's
    configservice directory serves the config service)."""
    return entry.get("ledger", entry["name"])


def load_facts(path: Path) -> dict[str, dict]:
    facts = json.loads(path.read_text(encoding="utf-8"))
    return {fact["name"]: fact for fact in facts}


def load_services(path: Path) -> tuple[str, list[dict]]:
    document = json.loads(path.read_text(encoding="utf-8"))
    names = [service["name"] for service in document["services"]]
    if len(set(names)) != len(names) or names != sorted(names):
        raise FeaturesError(f"{path}: services must be unique and in name order")
    return document["localstack_commit"], document["services"]


def marks(entry: dict) -> list[str]:
    found = []
    if "deferred" in entry:
        found.append(ENGINE_DEFERRED)
    if entry.get("providers_only"):
        found.append(PROVIDERS_ONLY)
    return found


def mark_sentences(entry: dict) -> list[str]:
    name = entry["name"]
    found = []
    if "deferred" in entry:
        found.append(entry["deferred"])
    if entry.get("providers_only"):
        found.append(f"LocalStack's `{name}` directory holds only CloudFormation resource providers.")
    return found


def render(commit: str, services: list[dict], facts: dict[str, dict], rows: dict[str, dict], parity: str, compat: str) -> str:
    """The text of docs/features.md. It raises FeaturesError when a registered
    service is missing from the list, or a parity link would not resolve."""
    served = {lookup_name(entry) for entry in services if not entry.get("providers_only")}
    unlisted = sorted(set(facts) - served)
    if unlisted:
        raise FeaturesError(f"registered but not in {SERVICES_FILE.name}: {', '.join(unlisted)}")
    parity_anchors = anchors(parity)
    ledger_rows: dict[str, list[dict]] = defaultdict(list)
    for nodeid, entry in rows.items():
        ledger_rows[ledger.service_of(nodeid)].append(entry)
    mentions = compat_mentions(services, compat)
    registered = set(facts)

    entries = []
    for service in services:
        key = lookup_name(service)
        fact = None if service.get("providers_only") else facts.get(key)
        service_rows = None if service.get("providers_only") else ledger_rows.get(key)
        if service_rows is not None and key not in parity_anchors:
            raise FeaturesError(f"{PARITY_DOC.name} has no anchor {key!r} for the parity link")
        status, reasons = assess(fact, service_rows, registered)
        entries.append({"service": service, "key": key, "fact": fact, "rows": service_rows, "status": status, "reasons": reasons})

    coded = [entry for entry in entries if entry["fact"] is not None]
    uncoded = [entry for entry in entries if entry["fact"] is None]

    out = [
        "# Features",
        "",
        "Generated by `parity/features.py`. Do not edit. Run `make features` to rewrite it; "
        "`make features-check` fails when it is stale.",
        "",
        f"The services are the {len(services)} directories under `localstack-core/localstack/services` in LocalStack "
        f"Community at `{commit}`, from `parity/features-services.json`. That is the directory list, not LocalStack's "
        "provider list: a directory marked "
        f"\"{PROVIDERS_ONLY}\" has no API of its own. Operation counts are what the registered services report to "
        "`GET /_gostack/services`. Parity numbers are in [parity.md](parity.md) and known differences in "
        "[compat.md](compat.md).",
        "",
        "## Legend",
        "",
        "| Status | Meaning |",
        "|---|---|",
        f"| {NOT_STARTED} | The service has no code. |",
        f"| {IN_PROGRESS} | The service has code and any of: a test LocalStack passes that gostack does not pass; "
        "an excluded test that waits on a service gostack now serves; no parity ledger; no test LocalStack passes. |",
        f"| {PARITY_DONE} | The service has code, every test LocalStack passes also passes on gostack, and no excluded test "
        "waits on a service gostack serves. Some operations may still answer 501. |",
        f"| {COMPLETE} | {PARITY_DONE}, and no operation answers 501. |",
        "",
        "Excluded tests and tests LocalStack does not pass count toward no status. "
        f"A service marked \"{ENGINE_DEFERRED}\" has its control-plane API in scope and its engine not "
        "(`docs/architecture.md`, Scope); the mark stays once the service is built. "
        "An operation a service does not implement answers 501.",
        "",
        "## Contents",
        "",
        "- [Summary](#summary)",
    ]
    out += [f"- [{entry['service']['name']}](#{entry['service']['name']})" for entry in coded]
    out += ["- [Services with no code](#services-with-no-code)", "", "## Summary", ""]
    out += ["| Service | Status | Operations | Excluded tests | Parity | Note |", "|---|---|---:|---:|---|---|"]
    for entry in entries:
        name, key = entry["service"]["name"], entry["key"]
        target = f"#{name}" if entry["fact"] is not None else "#services-with-no-code"
        has_ledger = entry["rows"] is not None
        excluded = sum(1 for row in entry["rows"] or [] if row["localstack"] == "excluded")
        excluded_cell = f"[{excluded}](parity.md#{key})" if has_ledger else "n/a"
        parity_cell = f"[row](parity.md#{key})" if has_ledger else "no ledger"
        out.append(
            f"| [{name}]({target}) | {entry['status']} | {operations_cell(entry['fact'])} | {excluded_cell} "
            f"| {parity_cell} | {'; '.join(marks(entry['service']))} |"
        )
    out.append("")

    out += ["## Services with code", ""]
    for entry in coded:
        service, fact, key = entry["service"], entry["fact"], entry["key"]
        name = service["name"]
        out += [f"### {name}", ""]
        out.append(f"Status: {entry['status']}. Operations implemented: {operations_cell(fact)}.")
        out.append("")
        if entry["reasons"]:
            out += ["In progress because " + "; ".join(entry["reasons"]) + ".", ""]
        if mark_sentences(service):
            out += [" ".join(mark_sentences(service)), ""]
        if entry["rows"] is not None:
            out += [f"Parity: [{key} in parity.md](parity.md#{key}), which counts its excluded tests.", ""]
        if fact["persist"]:
            out += [f"Persistence: state saved when `{persist_variable(key)}` is on.", ""]
        else:
            out += [f"Persistence: no snapshotter is registered, so `{persist_variable(key)}` saves nothing.", ""]
        out += compat_lines(name, mentions[name])
        missing = missing_operations(fact)
        if missing is None:
            out += [f"The operations this service implements are unknown; its API has {len(fact['operations'])}.", ""]
            continue
        out += operation_lines("Implemented", fact["implemented"], "None.")
        out += operation_lines("Not implemented, answered 501", missing, "None.")

    out += ["## Services with no code", ""]
    for entry in uncoded:
        service = entry["service"]
        extra = mark_sentences(service)
        out.append(f"- {service['name']}: {NOT_STARTED_REASON}" + (" " + " ".join(extra) if extra else ""))
    if not uncoded:
        out.append("None.")
    return "\n".join(out) + "\n"


def build(facts_path: Path) -> str:
    commit, services = load_services(SERVICES_FILE)
    return render(
        commit,
        services,
        load_facts(facts_path),
        ledger.load_ledger(ledger.LEDGER),
        PARITY_DOC.read_text(encoding="utf-8"),
        COMPAT_DOC.read_text(encoding="utf-8"),
    )


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("command", choices=("write", "check"))
    parser.add_argument("--facts", type=Path, required=True, help="the registered services' facts, from Test_featureFacts")
    parser.add_argument("--page", type=Path, default=PAGE)
    args = parser.parse_args()
    try:
        text = build(args.facts)
    except FeaturesError as error:
        raise SystemExit(f"features: {error}") from error
    if args.command == "write":
        args.page.write_text(text, encoding="utf-8")
        print(f"wrote {args.page}")
        return
    current = args.page.read_text(encoding="utf-8") if args.page.exists() else ""
    if current != text:
        sys.stderr.writelines(difflib.unified_diff(current.splitlines(True), text.splitlines(True), "docs/features.md", "generated"))
        raise SystemExit("features: docs/features.md is stale: run `make features` and commit it")


if __name__ == "__main__":
    main()
