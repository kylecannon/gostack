#!/usr/bin/env python3
"""Parity ledger: merge pytest results, ratchet regressions, render docs/parity.md.

parity/ledger.json maps a pytest nodeid to
    {"localstack": <status>, "gostack": <status>, "reason": <text>}
where status is one of pass, fail, skip, xfail, excluded, or null (not run).

Commands (standard library only):
    merge  --column localstack|gostack --junit FILE --excluded FILE
           Write that column for the nodeids the run collected and leave every
           other row untouched, then rewrite docs/parity.md. Prints per-file
           counts.
    check  [--base REV]
           Fail when a nodeid that passed in the committed ledger (default
           HEAD:parity/ledger.json) fails in the working-tree ledger, in either
           column. A regression is never a deferral.
           When parity/out/collected.json exists (written by prune), also warns
           about ledger rows that collection no longer found. Never a failure.
    prune  [--localstack DIR] [--dry-run] [--collected FILE]
           Collect the nodeids of the ledger's services with `pytest
           --collect-only -q` in the runner image (parity/run.sh collect, against
           the LocalStack checkout DIR) and remove every ledger row whose nodeid
           is gone. Prints the removed rows; --dry-run prints them and changes
           nothing. --collected FILE reads a saved `pytest --collect-only -q`
           listing instead of running Docker.
    docs   Rewrite docs/parity.md from the ledger.
    services [--changed]
           Print the services the ledger covers, comma separated. With --changed,
           read changed file paths from stdin and print only the services that
           code change can affect (empty when none): internal/service/<svc>/
           selects <svc>, and a shared path selects every service. --range
           A...B lets a change to the registration list that only adds lines
           naming a changed service count as that service's alone.
    test-dirs SERVICES
           Print the LocalStack test path (tests/aws/services/<dir>) of each
           comma separated service, one per line. parity/run.sh runs these.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
import xml.etree.ElementTree as ET
from collections import Counter, defaultdict
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
LEDGER = ROOT / "parity" / "ledger.json"
DOCS = ROOT / "docs" / "parity.md"
COLLECTED = ROOT / "parity" / "out" / "collected.json"
RUN_SH = ROOT / "parity" / "run.sh"
COLUMNS = ("localstack", "gostack")
STATUSES = ("pass", "fail", "skip", "xfail", "excluded")
SERVICE_PREFIX = "tests/aws/services/"
REASON_LIMIT = 200

# The services under tests/aws/services/ at the pinned LocalStack checkout
# (8b9a79f), keyed by the name of the service's directory under internal/service/
# and valued by its test directory. Only lambda differs: `lambda` is a Python
# keyword, so LocalStack's tests live in lambda_. A new LocalStack pin that adds
# or renames a directory changes this map (`ls tests/aws/services` there).
TEST_DIRS = {
    "acm": "acm",
    "apigateway": "apigateway",
    "cloudcontrol": "cloudcontrol",
    "cloudformation": "cloudformation",
    "cloudwatch": "cloudwatch",
    "config": "config",
    "dynamodb": "dynamodb",
    "dynamodbstreams": "dynamodbstreams",
    "ec2": "ec2",
    "es": "es",
    "events": "events",
    "firehose": "firehose",
    "iam": "iam",
    "kinesis": "kinesis",
    "kms": "kms",
    "lambda": "lambda_",
    "logs": "logs",
    "opensearch": "opensearch",
    "redshift": "redshift",
    "resource_groups": "resource_groups",
    "resourcegroupstaggingapi": "resourcegroupstaggingapi",
    "route53": "route53",
    "route53resolver": "route53resolver",
    "s3": "s3",
    "s3control": "s3control",
    "scheduler": "scheduler",
    "secretsmanager": "secretsmanager",
    "ses": "ses",
    "sns": "sns",
    "sqs": "sqs",
    "ssm": "ssm",
    "stepfunctions": "stepfunctions",
    "sts": "sts",
    "support": "support",
    "swf": "swf",
    "transcribe": "transcribe",
}
SERVICE_OF_DIR = {directory: name for name, directory in TEST_DIRS.items()}

# The file that lists the registered services. Adding a service touches it, so
# it is shared unless the change only adds lines naming the services it adds.
REGISTRATION_FILE = "cmd/gostack/services.go"

# A change under internal/service/<svc>/ affects that service alone. A change
# under SHARED_PATHS affects every service. Anything else (docs/, bench/, tools/,
# the other workflows) cannot change what the image does.
SERVICE_DIR = "internal/service/"
SHARED_PATHS = (
    "internal/",  # gateway, protocol, route, persist, config, api and the rest
    "cmd/",
    "codegen/",
    "specs/",
    "docker/",
    "parity/",
    "go.mod",
    "go.sum",
    ".github/workflows/parity.yml",
)

# When one nodeid has several junit testcases (call plus a teardown error), the
# worst one wins.
RANK = {"pass": 0, "skip": 1, "xfail": 2, "fail": 3}


def service_of(nodeid: str) -> str:
    if nodeid.startswith(SERVICE_PREFIX):
        directory = nodeid[len(SERVICE_PREFIX) :].split("/", 1)[0]
        return SERVICE_OF_DIR.get(directory, directory)
    return "other"


def test_dir(service: str) -> str:
    """The LocalStack test path of a service name."""
    if service not in TEST_DIRS:
        raise SystemExit(f"no LocalStack test directory known for service '{service}' (TEST_DIRS in parity/ledger.py)")
    return SERVICE_PREFIX + TEST_DIRS[service]


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

    # Excluded tests are never in the run: the plugin skipped them.
    for nodeid in excluded:
        results.pop(nodeid, None)

    # Only the nodeids this run collected are written. Every other row keeps both
    # columns, however the run was narrowed (SERVICES, -k, a nodeid list). The
    # plugin leaves a collected-but-deselected nodeid out of both inputs.
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
    stale = stale_rows(current, args.collected)
    if stale:
        print(f"parity-check warning (not a failure): {len(stale)} ledger row(s) not in the last collection, run `make parity-prune`:")
        print("\n".join(f"  {nodeid}" for nodeid in stale))
    if regressions:
        print(f"parity-check: {len(regressions)} regression(s) against {args.base}:", file=sys.stderr)
        print("\n".join(regressions), file=sys.stderr)
        raise SystemExit(1)
    passing = sum(1 for e in current.values() for c in COLUMNS if e[c] == "pass")
    print(f"parity-check: ok against {args.base} ({len(current)} entries, {passing} column passes)")


# --- prune -----------------------------------------------------------------


def parse_collection(text: str) -> set[str]:
    """The nodeids in `pytest --collect-only -q` output. Other lines (the blank
    line, the summary, warnings) hold no `::` after a path, so they are skipped."""
    return {
        line.strip()
        for line in text.splitlines()
        if line.startswith(SERVICE_PREFIX) and "::" in line
    }


def stale_rows(ledger: dict[str, dict], collected_path: Path) -> list[str]:
    """Ledger nodeids of the collected services that the last collection lacks.
    Empty when no collection is cached."""
    if not collected_path.exists():
        return []
    saved = json.loads(collected_path.read_text(encoding="utf-8"))
    return find_stale(ledger, set(saved["services"]), set(saved["nodeids"]))


def find_stale(ledger: dict[str, dict], services: set[str], collected: set[str]) -> list[str]:
    return sorted(n for n in ledger if service_of(n) in services and n not in collected)


def collect(localstack: Path | None, services: list[str]) -> str:
    """Run `parity/run.sh collect`, which checks the checkout is the pinned one.
    Without a path run.sh looks for ../localstack, as it does for `make parity`."""
    env = {**os.environ, "SERVICES": ",".join(services)}
    if localstack:
        env["LOCALSTACK_DIR"] = str(localstack)
    run = subprocess.run([str(RUN_SH), "collect"], env=env, capture_output=True, text=True, check=False)
    if run.returncode != 0:
        sys.stderr.write(run.stderr)
        raise SystemExit(f"prune: `parity/run.sh collect` exited {run.returncode}")
    return run.stdout


def prune(args: argparse.Namespace) -> None:
    ledger = load_ledger(args.ledger)
    services = sorted({service_of(n) for n in ledger} - {"other"})
    if not services:
        print("prune: the ledger covers no service")
        return
    output = args.collected.read_text(encoding="utf-8") if args.collected else collect(args.localstack, services)
    collected = parse_collection(output)
    if not collected:
        raise SystemExit("prune: the collection holds no nodeids; refusing to remove every row")
    # A service whose directory collected nothing is a collection problem, not a
    # service whose tests all vanished.
    collected_services = {service_of(n) for n in collected}
    missing = [s for s in services if s not in collected_services]
    if missing:
        raise SystemExit(f"prune: nothing collected for {', '.join(missing)}; refusing to prune")
    stale = find_stale(ledger, set(services), collected)
    verb = "would remove" if args.dry_run else "removed"
    for nodeid in stale:
        print(f"{verb} {nodeid}")
    print(f"prune: {verb} {len(stale)} of {len(ledger)} ledger row(s); {len(collected)} nodeids collected")
    if not args.collected:
        args.cache.parent.mkdir(parents=True, exist_ok=True)
        args.cache.write_text(
            json.dumps({"services": services, "nodeids": sorted(collected)}), encoding="utf-8"
        )
    if args.dry_run or not stale:
        return
    for nodeid in stale:
        del ledger[nodeid]
    write_ledger(args.ledger, ledger)
    write_docs(ledger, args.docs)


# --- services --------------------------------------------------------------


def only_adds_service_lines(diff: str, changed: set[str]) -> bool:
    """True when a `git diff -U0` removes nothing and every added line names one
    of the changed services as a word (an import, a struct field, a registration
    line). Anything else, including a line edited in place or a bare `}`, is not
    provably the registration of a service."""
    if not changed:
        return False
    names = re.compile(r"\b(?:" + "|".join(re.escape(name) for name in sorted(changed)) + r")\b")
    added = 0
    for line in diff.splitlines():
        if line.startswith(("+++", "---")) or not line.startswith(("+", "-")):
            continue
        if line.startswith("-") or not names.search(line):
            return False
        added += 1
    return added > 0


def registration_diff(revisions: str) -> str | None:
    shown = subprocess.run(
        ["git", "-C", str(ROOT), "diff", "-U0", revisions, "--", REGISTRATION_FILE],
        capture_output=True,
        text=True,
        check=False,
    )
    return shown.stdout if shown.returncode == 0 else None


def affected_services(paths: list[str], known: set[str], registration_adds: bool = False) -> set[str]:
    """The services that a change to paths can affect. A service directory
    selects its service when the service has a LocalStack test directory; a shared
    path selects every known service. registration_adds says the change to
    REGISTRATION_FILE only adds lines naming the services of the changed
    directories, so the file is not shared for it."""
    affected: set[str] = set()
    shared = False
    registration_changed = False
    for raw in paths:
        path = raw.strip()
        service, _, rest = path.removeprefix(SERVICE_DIR).partition("/")
        if path.startswith(SERVICE_DIR) and rest:
            if service in TEST_DIRS:
                affected.add(service)
        elif path == REGISTRATION_FILE:
            registration_changed = True
        elif path.startswith(SHARED_PATHS):
            shared = True
    if shared or (registration_changed and not registration_adds):
        return set(known) | affected
    return affected


def services(args: argparse.Namespace) -> None:
    known = {service_of(nodeid) for nodeid in load_ledger(args.ledger)} - {"other"}
    if not args.changed:
        print(",".join(sorted(known)))
        return
    paths = sys.stdin.read().splitlines()
    adds = False
    if args.range and any(p.strip() == REGISTRATION_FILE for p in paths):
        diff = registration_diff(args.range)
        # Git failing to produce the diff keeps every service selected.
        adds = diff is not None and only_adds_service_lines(diff, affected_services(paths, set()))
    print(",".join(sorted(affected_services(paths, known, adds))))


def test_dirs(args: argparse.Namespace) -> None:
    for name in args.services.split(","):
        print(test_dir(name))


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
    check_parser.add_argument("--collected", type=Path, default=COLLECTED, help="cached collection from prune")
    check_parser.set_defaults(func=check)

    prune_parser = sub.add_parser("prune", help="remove ledger rows whose test no longer exists upstream")
    prune_parser.add_argument("--localstack", type=Path, help="the pinned LocalStack checkout (default: where parity/run.sh looks)")
    prune_parser.add_argument("--dry-run", action="store_true", help="print the rows, change nothing")
    prune_parser.add_argument("--collected", type=Path, help="a saved `pytest --collect-only -q` listing, no Docker")
    prune_parser.add_argument("--cache", type=Path, default=COLLECTED, help=argparse.SUPPRESS)
    prune_parser.set_defaults(func=prune)

    services_parser = sub.add_parser("services", help="print the ledger's services, or those a change affects")
    services_parser.add_argument("--changed", action="store_true", help="read changed paths from stdin")
    services_parser.add_argument("--range", help="git revisions A...B of the change; lets a registration-only edit of cmd/gostack/services.go not count as shared")
    services_parser.set_defaults(func=services)

    dirs_parser = sub.add_parser("test-dirs", help="print the LocalStack test path of each service")
    dirs_parser.add_argument("services", help="comma separated service names")
    dirs_parser.set_defaults(func=test_dirs)

    docs_parser = sub.add_parser("docs", help="rewrite docs/parity.md")
    docs_parser.set_defaults(func=docs)

    args = parser.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
