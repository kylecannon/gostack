#!/usr/bin/env python3
"""Generates docs/features.md: what each LocalStack Community service has built
in gostack, what is partial and what is not started.

    python3 parity/features.py write --facts FILE   rewrite docs/features.md
    python3 parity/features.py check --facts FILE   fail when docs/features.md differs

`make features` and `make features-check` run these with the facts file the Go
test Test_featureFacts (cmd/gostack) writes: for every registered service, its
operations, the ones it implements and whether PERSIST_<SERVICE> saves it. The
rest is read from the repository: parity/features-services.json (the services
of the pinned LocalStack checkout), parity/ledger/ (through ledger.py) and the
headings of docs/parity.md and docs/compat.md, which the page links to and
never copies. Standard library only.
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

PARITY_DONE = "Parity done"
IN_PROGRESS = "In progress"
NOT_STARTED = "Not started"
DEFERRED = "Deferred: engine-backed"

NOT_STARTED_REASON = "No code yet."
HEADING = re.compile(r"^(#{1,6})[ \t]+(.+?)[ \t#]*$")
FENCE = re.compile(r"^[ \t]*(```|~~~)")
FIRST_WORD = re.compile(r"^[A-Z][a-z0-9]*")


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


def compat_links(services: list[dict], compat: str) -> dict[str, list[tuple[str, str]]]:
    """For each service, the headings of docs/compat.md that name it: a
    heading is the service's when it is, or starts with, one of the service's
    `compat` titles and a space. A heading two services' titles both match
    belongs to the one with the longer title, so "DynamoDB Streams" is not
    also a DynamoDB entry."""
    titles = [(title, service["name"]) for service in services for title in service.get("compat", [])]
    links: dict[str, list[tuple[str, str]]] = {service["name"]: [] for service in services}
    for level, text, anchor in slugs(compat):
        if level not in (2, 3):
            continue
        owners = [(title, name) for title, name in titles if text == title or text.startswith(title + " ")]
        if owners:
            links[max(owners, key=lambda owner: len(owner[0]))[1]].append((text, anchor))
    return links


def status_of(registered: bool, deferred: bool, rows: list[dict]) -> str:
    """A service with code is Parity done when it has a ledger in which
    LocalStack passes something and every test LocalStack passes, gostack
    passes; any other service with code is In progress. A service with no code
    is Deferred when the list says it is engine-backed, and Not started
    otherwise."""
    if not registered:
        return DEFERRED if deferred else NOT_STARTED
    passed = [row for row in rows if row["localstack"] == "pass"]
    if passed and all(row["gostack"] == "pass" for row in passed):
        return PARITY_DONE
    return IN_PROGRESS


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
        return "none"
    if fact["implemented"] is None:
        return f"unknown of {len(fact['operations'])}"
    return f"{len(fact['implemented'])} of {len(fact['operations'])}"


def load_facts(path: Path) -> dict[str, dict]:
    facts = json.loads(path.read_text(encoding="utf-8"))
    return {fact["name"]: fact for fact in facts}


def load_services(path: Path) -> tuple[str, list[dict]]:
    document = json.loads(path.read_text(encoding="utf-8"))
    names = [service["name"] for service in document["services"]]
    if len(set(names)) != len(names) or names != sorted(names):
        raise FeaturesError(f"{path}: services must be unique and in name order")
    return document["localstack_commit"], document["services"]


def render(commit: str, services: list[dict], facts: dict[str, dict], rows: dict[str, dict], parity: str, compat: str) -> str:
    """The text of docs/features.md. It raises FeaturesError when a registered
    service is missing from the list, or a parity link would not resolve."""
    names = {service["name"] for service in services}
    unlisted = sorted(set(facts) - names)
    if unlisted:
        raise FeaturesError(f"registered but not in {SERVICES_FILE.name}: {', '.join(unlisted)}")
    parity_anchors = {anchor for _, _, anchor in slugs(parity)}
    ledger_rows: dict[str, list[dict]] = defaultdict(list)
    for nodeid, entry in rows.items():
        ledger_rows[ledger.service_of(nodeid)].append(entry)
    compat_by_service = compat_links(services, compat)

    entries = []
    for service in services:
        name = service["name"]
        fact = facts.get(name)
        status = status_of(fact is not None, "deferred" in service, ledger_rows.get(name, []))
        has_ledger = name in ledger_rows
        if has_ledger and name not in parity_anchors:
            raise FeaturesError(f"{PARITY_DOC.name} has no heading {name!r} for the parity link")
        entries.append((service, fact, status, has_ledger))

    coded = [entry for entry in entries if entry[1] is not None]
    deferred = [entry for entry in entries if entry[2] == DEFERRED]
    not_started = [entry for entry in entries if entry[2] == NOT_STARTED]

    out = [
        "# Features",
        "",
        "Generated by `parity/features.py`. Do not edit. Run `make features` to rewrite it; "
        "`make features-check` fails when it is stale.",
        "",
        f"It lists the {len(services)} services of LocalStack Community at `{commit}` (`parity/features-services.json`) "
        "and what gostack has built of each. Operation counts come from the registered services, the same source "
        "as `GET /_gostack/services`. Parity and known differences are links: the numbers live in "
        "[parity.md](parity.md) and the differences in [compat.md](compat.md).",
        "",
        "## Legend",
        "",
        "| Status | Meaning |",
        "|---|---|",
        f"| {PARITY_DONE} | The service has code and a parity ledger, and every test LocalStack passes, gostack passes. |",
        f"| {IN_PROGRESS} | The service has code and is not Parity done. |",
        f"| {NOT_STARTED} | The service has no code. |",
        f"| {DEFERRED} | The service has no code and needs a real engine, which is deferred. |",
        "",
        "An operation the service does not implement answers 501.",
        "",
        "## Contents",
        "",
        "- [Summary](#summary)",
    ]
    out += [f"- [{service['name']}](#{service['name']})" for service, *_ in coded]
    out += ["- [Not started and deferred](#not-started-and-deferred)", "", "## Summary", ""]
    out += ["| Service | Status | Operations | Parity | Section |", "|---|---|---:|---|---|"]
    for service, fact, status, has_ledger in entries:
        name = service["name"]
        parity_cell = f"[row](parity.md#{name})" if has_ledger else "none"
        section = f"[{name}](#{name})" if fact is not None else "[not started and deferred](#not-started-and-deferred)"
        out.append(f"| {name} | {status} | {operations_cell(fact)} | {parity_cell} | {section} |")
    out.append("")

    out += ["## Services with code", ""]
    for service, fact, status, has_ledger in coded:
        name = service["name"]
        out += [f"### {name}", ""]
        out.append(f"Status: {status}. Operations implemented: {operations_cell(fact)}.")
        out.append("")
        persist = "yes" if fact["persist"] else "no"
        out.append(f"Persistence: `PERSIST_{name.upper()}`: {persist}.")
        out.append("")
        if has_ledger:
            out += [f"Parity: [row in parity.md](parity.md#{name}).", ""]
        links = compat_by_service[name]
        if links:
            out += ["Known differences: " + ", ".join(f"[{text}](compat.md#{anchor})" for text, anchor in links) + ".", ""]
        else:
            out += ["Known differences: none recorded in compat.md.", ""]
        if fact["implemented"] is None:
            out += [f"The operations this service implements are unknown; its API has {len(fact['operations'])}.", ""]
            continue
        implemented = set(fact["implemented"])
        missing = [operation for operation in fact["operations"] if operation not in implemented]
        out += operation_lines("Implemented", fact["implemented"], "None.")
        out += operation_lines("Not implemented, answered 501", missing, "None.")

    out += ["## Not started and deferred", ""]
    out += ["### Deferred: engine-backed", ""]
    out += [f"- {service['name']}: {service['deferred']}" for service, *_ in deferred] or ["None."]
    out += ["", "### Not started", ""]
    out += [
        f"- {service['name']}: {NOT_STARTED_REASON}" + (f" {service['note']}" if "note" in service else "")
        for service, *_ in not_started
    ] or ["None."]
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
