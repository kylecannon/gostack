#!/usr/bin/env python3
"""Parity ledger: merge pytest results, ratchet regressions, render docs/parity.md.

parity/ledger.json maps a pytest nodeid to
    {"localstack": <status>, "gostack": <status>, "reason": <text>}
where status is one of pass, fail, skip, xfail, excluded, or null (not run).

Commands (standard library only):
    merge  --column localstack|gostack --junit FILE --excluded FILE
           Replace that column for every service the run covered, then rewrite
           docs/parity.md. Prints per-file counts.
    check  [--base REV]
           Fail when a nodeid that passed in the committed ledger (default
           HEAD:parity/ledger.json) fails in the working-tree ledger, in either
           column. A regression is never a deferral.
    docs   Rewrite docs/parity.md from the ledger.
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
import xml.etree.ElementTree as ET
from collections import Counter, defaultdict
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
LEDGER = ROOT / "parity" / "ledger.json"
DOCS = ROOT / "docs" / "parity.md"
COLUMNS = ("localstack", "gostack")
STATUSES = ("pass", "fail", "skip", "xfail", "excluded")
SERVICE_PREFIX = "tests/aws/services/"
REASON_LIMIT = 200

# When one nodeid has several junit testcases (call plus a teardown error), the
# worst one wins.
RANK = {"pass": 0, "skip": 1, "xfail": 2, "fail": 3}


def service_of(nodeid: str) -> str:
    if nodeid.startswith(SERVICE_PREFIX):
        return nodeid[len(SERVICE_PREFIX) :].split("/", 1)[0]
    return "other"


def file_of(nodeid: str) -> str:
    return nodeid.split("::", 1)[0]


def one_line(text: str | None) -> str:
    first = (text or "").strip().splitlines()[0] if (text or "").strip() else ""
    return first[:REASON_LIMIT]


# --- junit -----------------------------------------------------------------


def nodeid_of(case: ET.Element) -> str:
    """Rebuild the pytest nodeid from an xunit1 testcase (it carries `file`)."""
    path = case.get("file")
    if not path:
        raise SystemExit(
            "junit testcase has no `file` attribute; run pytest with -o junit_family=xunit1"
        )
    module = path[: -len(".py")].replace("/", ".") if path.endswith(".py") else path
    classname = case.get("classname", "")
    classes = classname[len(module) + 1 :] if classname.startswith(module + ".") else ""
    parts = [path, *([c for c in classes.split(".") if c] if classes else []), case.get("name", "")]
    return "::".join(parts)


def read_junit(path: Path) -> dict[str, tuple[str, str]]:
    """nodeid -> (status, reason)."""
    results: dict[str, tuple[str, str]] = {}
    for case in ET.parse(path).getroot().iter("testcase"):
        nodeid = nodeid_of(case)
        status, reason = "pass", ""
        if case.find("failure") is not None or case.find("error") is not None:
            node = case.find("failure") if case.find("failure") is not None else case.find("error")
            status, reason = "fail", one_line(node.get("message"))
        elif case.find("skipped") is not None:
            node = case.find("skipped")
            xfail = node.get("type") == "pytest.xfail"
            status, reason = ("xfail" if xfail else "skip"), one_line(node.get("message"))
        previous = results.get(nodeid)
        if previous is None or RANK[status] > RANK[previous[0]]:
            results[nodeid] = (status, reason)
    return results


# --- ledger file -----------------------------------------------------------


def load_ledger(path: Path) -> dict[str, dict]:
    if not path.exists():
        return {}
    ledger = json.loads(path.read_text(encoding="utf-8"))
    validate(ledger, str(path))
    return ledger


def validate(ledger: object, where: str) -> None:
    if not isinstance(ledger, dict):
        raise SystemExit(f"{where}: expected a JSON object of nodeid to entry")
    for nodeid, entry in ledger.items():
        if not isinstance(entry, dict) or set(entry) != {"localstack", "gostack", "reason"}:
            raise SystemExit(f"{where}: {nodeid}: entry must have localstack, gostack and reason")
        for column in COLUMNS:
            if entry[column] is not None and entry[column] not in STATUSES:
                raise SystemExit(f"{where}: {nodeid}: bad {column} status {entry[column]!r}")


def write_ledger(path: Path, ledger: dict[str, dict]) -> None:
    lines = [
        f"{json.dumps(nodeid)}: {json.dumps(ledger[nodeid], sort_keys=True)}" for nodeid in sorted(ledger)
    ]
    path.write_text("{\n" + ",\n".join(lines) + "\n}\n" if lines else "{}\n", encoding="utf-8")


# --- merge -----------------------------------------------------------------


def merge(args: argparse.Namespace) -> None:
    ledger = load_ledger(args.ledger)
    results = read_junit(args.junit)
    excluded_doc = json.loads(args.excluded.read_text(encoding="utf-8"))
    excluded: dict[str, str] = excluded_doc["excluded"]

    # Excluded tests are never in the run: pytest skipped them or `-m` dropped them.
    for nodeid in excluded:
        results.pop(nodeid, None)

    run_nodeids = set(results) | set(excluded)
    services = {service_of(n) for n in run_nodeids}

    # The run replaces this column for every service it covered.
    for nodeid, entry in list(ledger.items()):
        if service_of(nodeid) in services and nodeid not in run_nodeids:
            entry[args.column] = None
            if all(entry[c] is None for c in COLUMNS):
                del ledger[nodeid]

    for nodeid, (status, reason) in results.items():
        entry = ledger.setdefault(nodeid, {"localstack": None, "gostack": None, "reason": ""})
        entry[args.column] = status
        # Failure text is stored for the baseline only: it is stable. gostack
        # failures change run to run and live in the junit artifact.
        if args.column == "localstack":
            entry["reason"] = reason if status != "pass" else ""
        elif entry["localstack"] is None and status != "pass":
            entry["reason"] = reason

    for nodeid, reason in excluded.items():
        ledger[nodeid] = {"localstack": "excluded", "gostack": "excluded", "reason": reason}

    write_ledger(args.ledger, ledger)
    write_docs(ledger, args.docs)
    print_run_counts(args.column, results, excluded)


def print_run_counts(column: str, results: dict[str, tuple[str, str]], excluded: dict[str, str]) -> None:
    per_file: dict[str, Counter] = defaultdict(Counter)
    for nodeid, (status, _) in results.items():
        per_file[file_of(nodeid)][status] += 1
    for nodeid in excluded:
        per_file[file_of(nodeid)]["excluded"] += 1
    total: Counter = Counter()
    print(f"\n{column} results per file")
    print(f"{'file':<78} {'pass':>5} {'fail':>5} {'skip':>5} {'xfail':>5} {'excl':>5}")
    for name in sorted(per_file):
        c = per_file[name]
        total.update(c)
        print(
            f"{name:<78} {c['pass']:>5} {c['fail']:>5} {c['skip']:>5} {c['xfail']:>5} {c['excluded']:>5}"
        )
    print(
        f"{'TOTAL':<78} {total['pass']:>5} {total['fail']:>5} {total['skip']:>5} "
        f"{total['xfail']:>5} {total['excluded']:>5}"
    )
    failed = sorted(n for n, (s, _) in results.items() if s == "fail")
    if failed:
        print(f"\n{column}: {len(failed)} failing test(s)")
        for nodeid in failed:
            print(f"  FAIL {nodeid}: {results[nodeid][1]}")


# --- check -----------------------------------------------------------------


def committed_ledger(base: str, path: Path) -> dict[str, dict] | None:
    relative = path.resolve().relative_to(ROOT).as_posix()
    shown = subprocess.run(
        ["git", "-C", str(ROOT), "show", f"{base}:{relative}"], capture_output=True, text=True, check=False
    )
    if shown.returncode != 0:
        return None
    ledger = json.loads(shown.stdout)
    validate(ledger, f"{base}:{relative}")
    return ledger


def check(args: argparse.Namespace) -> None:
    current = load_ledger(args.ledger)
    base = committed_ledger(args.base, args.ledger)
    if base is None:
        print(f"parity-check: no committed ledger at {args.base}; {len(current)} entries, nothing to compare")
        return
    regressions, warnings = [], []
    for nodeid, before in sorted(base.items()):
        after = current.get(nodeid)
        for column in COLUMNS:
            if before[column] != "pass":
                continue
            now = after[column] if after else None
            if now == "fail":
                regressions.append(f"  {column}: {nodeid}: pass -> fail")
            elif now != "pass" and after is not None:
                warnings.append(f"  {column}: {nodeid}: pass -> {now}")
            elif after is None:
                warnings.append(f"  {column}: {nodeid}: pass -> missing")
    for line in warnings:
        print(f"parity-check warning (not a failure):\n{line}" if line is warnings[0] else line)
    if regressions:
        print(f"parity-check: {len(regressions)} regression(s) against {args.base}:", file=sys.stderr)
        print("\n".join(regressions), file=sys.stderr)
        raise SystemExit(1)
    passing = sum(1 for e in current.values() for c in COLUMNS if e[c] == "pass")
    print(f"parity-check: ok against {args.base} ({len(current)} entries, {passing} column passes)")


# --- docs ------------------------------------------------------------------


def write_docs(ledger: dict[str, dict], path: Path) -> None:
    by_service: dict[str, list[dict]] = defaultdict(list)
    for nodeid, entry in ledger.items():
        by_service[service_of(nodeid)].append(entry)

    header = (
        "| Service | Tests | Excluded | Skipped | LocalStack pass | LocalStack fail "
        "| gostack pass | Parity |\n|---|---:|---:|---:|---:|---:|---:|---:|\n"
    )
    rows, totals = [], Counter()
    for service in sorted(by_service):
        entries = by_service[service]
        counts = Counter()
        counts["tests"] = len(entries)
        counts["excluded"] = sum(1 for e in entries if e["localstack"] == "excluded")
        counts["skipped"] = sum(1 for e in entries if e["localstack"] in ("skip", "xfail"))
        counts["ls_pass"] = sum(1 for e in entries if e["localstack"] == "pass")
        counts["ls_fail"] = sum(1 for e in entries if e["localstack"] == "fail")
        counts["gs_pass"] = sum(1 for e in entries if e["gostack"] == "pass")
        counts["gs_run"] = sum(1 for e in entries if e["gostack"] not in (None, "excluded"))
        counts["both"] = sum(1 for e in entries if e["localstack"] == "pass" and e["gostack"] == "pass")
        totals.update(counts)
        rows.append(format_row(service, counts))
    rows.append(format_row("**total**", totals))

    path.write_text(
        "# Parity\n\n"
        "Generated by `parity/ledger.py` from `parity/ledger.json`. Do not edit.\n\n"
        "gostack runs LocalStack's own pytest suite unchanged. Parity is the share of tests "
        "that LocalStack passes that gostack also passes in the same runner. Tests LocalStack "
        "fails are recorded and not required. Excluded tests are listed with a reason in "
        "`parity/exclusions.yaml`.\n\n" + header + "\n".join(rows) + "\n",
        encoding="utf-8",
    )


def format_row(label: str, c: Counter) -> str:
    if c["gs_run"] == 0:
        parity = "n/a"
    else:
        parity = f"{100 * c['both'] / c['ls_pass']:.1f}%" if c["ls_pass"] else "n/a"
    gostack = str(c["gs_pass"]) if c["gs_run"] else "not run"
    return (
        f"| {label} | {c['tests']} | {c['excluded']} | {c['skipped']} | {c['ls_pass']} "
        f"| {c['ls_fail']} | {gostack} | {parity} |"
    )


def docs(args: argparse.Namespace) -> None:
    write_docs(load_ledger(args.ledger), args.docs)
    print(f"wrote {args.docs}")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--ledger", type=Path, default=LEDGER)
    parser.add_argument("--docs", type=Path, default=DOCS)
    sub = parser.add_subparsers(dest="command", required=True)

    merge_parser = sub.add_parser("merge", help="merge a run into the ledger")
    merge_parser.add_argument("--column", choices=COLUMNS, required=True)
    merge_parser.add_argument("--junit", type=Path, required=True)
    merge_parser.add_argument("--excluded", type=Path, required=True)
    merge_parser.set_defaults(func=merge)

    check_parser = sub.add_parser("check", help="fail on pass -> fail against the committed ledger")
    check_parser.add_argument("--base", default="HEAD", help="git revision holding the committed ledger")
    check_parser.set_defaults(func=check)

    docs_parser = sub.add_parser("docs", help="rewrite docs/parity.md")
    docs_parser.set_defaults(func=docs)

    args = parser.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
