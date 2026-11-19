#!/usr/bin/env python3
"""Parity ledger: merge pytest results, ratchet regressions, render docs/parity.md.

The ledger is one file per service, parity/ledger/<service>.json, named as
TEST_DIRS names the service. Each maps a pytest nodeid of that service to
    {"localstack": <status>, "gostack": <status>, "reason": <text>}
where status is one of pass, fail, skip, xfail, excluded, or null (not run).
A nodeid's service comes from its test directory (TEST_DIRS); a nodeid whose
directory maps to no service, or that sits in another service's file, is an
error. The exclusions are one file per service too, parity/exclusions/<service>.yaml,
and each pattern sits in the file of the service whose tests it matches.

Commands (standard library only):
    merge  --column localstack|gostack --junit FILE --excluded FILE
           Write that column for the nodeids the run collected and leave every
           other row untouched, then rewrite docs/parity.md. Prints per-file
           counts.
    check  [--base REV]
           Compare the working-tree ledger with the one committed at REV
           (default HEAD; CI passes the merge base). It fails when:
           - a nodeid that was `pass` in the gostack column is anything else
             now (fail, skip, xfail, excluded, null, or a missing row) and
             parity/demotions.yaml does not list it. Each one is named;
           - parity/demotions.yaml lists a nodeid that is `pass` in the
             gostack column now (the test passes again; remove the entry);
           - a nodeid that was `pass` in the localstack column is `fail` now
             (any other change there only warns);
           - a service file the base has is gone from the working tree.
           The base is read in either layout: parity/ledger/*.json, or the
           single parity/ledger.json that came before it. Also checks every
           exclusions file. When parity/out/collected.json exists (written by
           prune), also warns about ledger rows that collection no longer
           found. Never a failure.
    prune  [--localstack DIR] [--dry-run] [--collected FILE]
           Collect the nodeids of the ledger's services with `pytest
           --collect-only -q` in the runner image (parity/run.sh collect, against
           the LocalStack checkout DIR) and remove every ledger row whose nodeid
           is gone. A removed row that was `pass` in the gostack column is
           written to parity/demotions.yaml as gone upstream at the pinned
           LocalStack sha. Prints the removed rows; --dry-run prints them and
           changes nothing. --collected FILE reads a saved `pytest
           --collect-only -q` listing instead of running Docker.
    docs   Rewrite docs/parity.md from the ledger.
    services [--changed [--range A...B]]
           Print the services the ledger covers, comma separated. With
           --changed, read NUL-separated changed paths from stdin (`git diff
           --name-only -z --no-renames`) and print the services that change can
           affect (empty when none). A path selects every service unless it
           is one service's own path or is in NO_IMAGE_EFFECT; see
           affected_services. --range names the change, so a change to
           cmd/gostack/services.go selects the services its changed lines
           name instead of every service.
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
LEDGER = ROOT / "parity" / "ledger"
EXCLUSIONS = ROOT / "parity" / "exclusions"
# The single-file layouts that came before the per-service files. A base
# revision may still hold the ledger; neither may come back in the working tree.
LEGACY_LEDGER = "ledger.json"
LEGACY_EXCLUSIONS = "exclusions.yaml"
DOCS = ROOT / "docs" / "parity.md"
DEMOTIONS = ROOT / "parity" / "demotions.yaml"
SOURCES_LOCK = ROOT / "specs" / "SOURCES.lock"
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

# The file that lists the registered services. A change to it selects the
# services its changed lines name, or every service when a line names none.
REGISTRATION_FILE = "cmd/gostack/services.go"

# A change under internal/service/<svc>/, where <svc> is in TEST_DIRS, is that
# service's own. Every other path selects every service, except these, which
# cannot change what the image does or how the suite runs against it.
SERVICE_DIR = "internal/service/"
NO_IMAGE_EFFECT = (
    # Outside the Docker build context (.dockerignore), and not read by parity/.
    "docs/",
    "tools/",
    ".claude/",
    # In the build context, but its own programs: nothing under cmd/ or
    # internal/ imports it, so it is not in the binary.
    "bench/",
    # Prose in the build context that nothing compiles, embeds or reads.
    "README.md",
    "AGENTS.md",
    "CLAUDE.md",
)
# Every workflow but this one: they run checks, they do not build the image
# parity runs against.
WORKFLOWS = ".github/workflows/"
PARITY_WORKFLOW = ".github/workflows/parity.yml"

# The services a service calls in this process, so a change to the callee can
# break the caller's tests. s3 delivers event notifications to sqs.
IN_PROCESS_CALLS = {
    "s3": ("sqs",),
}

# When one nodeid has several junit testcases (call plus a teardown error), the
# worst one wins.
RANK = {"pass": 0, "skip": 1, "xfail": 2, "fail": 3}


def service_of(nodeid: str) -> str:
    """The service whose test directory holds nodeid. A nodeid outside every
    TEST_DIRS directory is an error: its row would have no file to live in."""
    directory = nodeid[len(SERVICE_PREFIX) :].split("/", 1)[0] if nodeid.startswith(SERVICE_PREFIX) else ""
    if directory not in SERVICE_OF_DIR:
        raise SystemExit(f"{nodeid}: not under a known service's test directory (TEST_DIRS in parity/ledger.py)")
    return SERVICE_OF_DIR[directory]


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


# --- ledger files ----------------------------------------------------------


def service_files(directory: Path, suffix: str) -> dict[str, Path]:
    """service -> file for every <service><suffix> in directory. A file named
    for no known service is an error."""
    if not directory.is_dir():
        return {}
    files = {path.name[: -len(suffix)]: path for path in sorted(directory.glob("*" + suffix))}
    unknown = sorted(set(files) - set(TEST_DIRS))
    if unknown:
        raise SystemExit(f"{directory}: {', '.join(unknown)} name no service in TEST_DIRS (parity/ledger.py)")
    return files


def refuse_legacy(directory: Path, legacy: str) -> None:
    """The single file the per-service directory replaced must not come back:
    nothing reads it, so its rows would be silently ignored."""
    if (directory.parent / legacy).exists():
        raise SystemExit(f"{directory.parent / legacy}: the single file is replaced by {directory}/<service>; move its rows there")


def loads_unique(text: str, where: str) -> object:
    """json.loads that refuses a key listed twice in one object: the last one
    would win silently, so a `fail` row could hide behind a later `pass`."""

    def unique(pairs: list[tuple[str, object]]) -> dict:
        keys = Counter(key for key, _ in pairs)
        twice = sorted(key for key, count in keys.items() if count > 1)
        if twice:
            raise SystemExit(f"{where}: {', '.join(twice)} listed twice")
        return dict(pairs)

    return json.loads(text, object_pairs_hook=unique)


def parse_service_ledger(service: str, text: str, where: str) -> dict[str, dict]:
    rows = loads_unique(text, where)
    validate(rows, where)
    for nodeid in rows:
        if service_of(nodeid) != service:
            raise SystemExit(f"{where}: {nodeid} belongs to {service_of(nodeid)}, not {service}")
    return rows


def load_ledger(directory: Path) -> dict[str, dict]:
    """The union of the per-service files in directory."""
    refuse_legacy(directory, LEGACY_LEDGER)
    ledger: dict[str, dict] = {}
    for service, path in service_files(directory, ".json").items():
        ledger.update(parse_service_ledger(service, path.read_text(encoding="utf-8"), str(path)))
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


def split_by_service(ledger: dict[str, dict]) -> dict[str, dict[str, dict]]:
    by_service: dict[str, dict[str, dict]] = defaultdict(dict)
    for nodeid, entry in ledger.items():
        by_service[service_of(nodeid)][nodeid] = entry
    return dict(by_service)


def format_ledger(rows: dict[str, dict]) -> str:
    lines = [f"{json.dumps(nodeid)}: {json.dumps(rows[nodeid], sort_keys=True)}" for nodeid in sorted(rows)]
    return "{\n" + ",\n".join(lines) + "\n}\n" if lines else "{}\n"


def write_ledger(directory: Path, ledger: dict[str, dict]) -> None:
    """Write each service's rows, sorted, to its own file. Every nodeid is
    mapped before anything is written. A service file whose rows are all gone
    is kept as {} rather than deleted, so check still sees the service."""
    by_service = split_by_service(ledger)
    existing = service_files(directory, ".json")
    directory.mkdir(parents=True, exist_ok=True)
    for service in sorted(set(by_service) | set(existing)):
        (directory / f"{service}.json").write_text(format_ledger(by_service.get(service, {})), encoding="utf-8")


# --- exclusions files ------------------------------------------------------

# The one line shape an exclusions file may hold besides comments and blank
# lines: a double-quoted nodeid pattern and a plain reason. pytest_gostack.py
# reads the files with yaml.safe_load; ledger.py reads them with the standard
# library, so it accepts only this shape and refuses anything else. A plain
# reason may not start with a YAML indicator or hold ": " or " #", which YAML
# would read as a mapping or a comment, nor be a word YAML 1.1 resolves to
# another type (a bool, null, a number, .inf, .nan, a timestamp, = or <<). A
# tab is refused anywhere outside a comment's text: YAML refuses it as
# indentation and between tokens.
EXCLUSION_LINE = re.compile(r'^("(?:[^"\\]|\\.)*"): ([^\s"\'#&*!|>%@`{}\[\],?:-]\S*(?: +\S+)*) *$')
PLAIN_BREAKERS = re.compile(
    r": |[ \t]#|:$|\t"
    r"|^(?:true|false|yes|no|on|off|y|n|null|~|=|<<|\.?[0-9+.][0-9_.:eE+-]*"
    r"|[-+]?\.(?:inf|nan)|[-+]?0x[0-9a-f_]+|[-+]?0b[01_]+)$"
    r"|^[0-9]{4}-[0-9]{1,2}-[0-9]{1,2}(?:$|[Tt \t])",
    re.IGNORECASE,
)


def parse_reason_lines(text: str, where: str) -> dict[str, tuple[int, str]]:
    """key -> (line number, reason) for every `"key": reason` line. Comments
    and blank lines are skipped; any other line, and a key listed twice, is an
    error."""
    entries: dict[str, tuple[int, str]] = {}
    for number, line in enumerate(text.splitlines(), 1):
        if not line.strip() or line.lstrip(" ").startswith("#"):
            continue
        match = EXCLUSION_LINE.match(line)
        if not match or PLAIN_BREAKERS.search(match.group(2)):
            raise SystemExit(f'{where}:{number}: expected `"nodeid": reason`, got {line!r}')
        key, reason = json.loads(match.group(1)), match.group(2)
        if key in entries:
            raise SystemExit(f"{where}:{number}: {key} is listed twice")
        entries[key] = (number, reason)
    return entries


def parse_exclusions(service: str, text: str, where: str) -> dict[str, str]:
    patterns: dict[str, str] = {}
    for pattern, (number, reason) in parse_reason_lines(text, where).items():
        if service_of(pattern) != service:
            raise SystemExit(f"{where}:{number}: {pattern} matches {service_of(pattern)} tests, not {service}")
        patterns[pattern] = reason
    return patterns


def load_exclusions(directory: Path) -> dict[str, str]:
    """pattern -> reason over every per-service exclusions file. A pattern sits
    in the file of the service whose tests it matches."""
    refuse_legacy(directory, LEGACY_EXCLUSIONS)
    patterns: dict[str, str] = {}
    for service, path in service_files(directory, ".yaml").items():
        patterns.update(parse_exclusions(service, path.read_text(encoding="utf-8"), str(path)))
    return patterns


# --- demotions -------------------------------------------------------------

DEMOTIONS_HEADER = """\
# Reviewed demotions: gostack tests that passed and no longer do. `make
# parity-check` fails when a row that is `pass` in the gostack column at the
# base is anything else now, unless its nodeid is listed here with a one-line
# reason. An entry whose test passes again is refused: remove it. `make
# parity-prune` adds the passing rows it removes, as gone upstream. One exact
# nodeid per line, sorted; same line grammar as parity/exclusions/.
"""


def load_demotions(path: Path) -> dict[str, str]:
    """nodeid -> reason. No file lists nothing."""
    if not path.exists():
        return {}
    entries = parse_reason_lines(path.read_text(encoding="utf-8"), str(path))
    for nodeid, (number, _) in entries.items():
        if "::" not in nodeid:
            raise SystemExit(f"{path}:{number}: {nodeid} is not a nodeid")
        service_of(nodeid)
    return {nodeid: reason for nodeid, (_, reason) in entries.items()}


def write_demotions(path: Path, demotions: dict[str, str]) -> None:
    lines = "".join(f"{json.dumps(nodeid)}: {demotions[nodeid]}\n" for nodeid in sorted(demotions))
    path.write_text(DEMOTIONS_HEADER + lines, encoding="utf-8")


def pinned_localstack_sha() -> str:
    for line in SOURCES_LOCK.read_text(encoding="utf-8").splitlines():
        fields = line.split()
        if len(fields) == 2 and fields[0] == "localstack_sha":
            return fields[1]
    raise SystemExit(f"{SOURCES_LOCK}: no localstack_sha line")


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


def git(*argv: str) -> subprocess.CompletedProcess:
    return subprocess.run(["git", "-C", str(ROOT), *argv], capture_output=True, text=True, check=False)


def git_show(base: str, relative: str) -> str:
    shown = git("show", f"{base}:{relative}")
    if shown.returncode != 0:
        raise SystemExit(f"parity-check: cannot read {base}:{relative}: {shown.stderr.strip()}")
    return shown.stdout


def committed_ledger(base: str, directory: Path) -> tuple[dict[str, dict], set[str]] | None:
    """The ledger at base and the services it has rows or a file for, from the
    per-service files when base has them, else from the single ledger.json that
    came before them. None only when base has neither (or is no commit)."""
    relative = directory.resolve().relative_to(ROOT).as_posix()
    legacy = f"{relative.rsplit('/', 1)[0]}/{LEGACY_LEDGER}" if "/" in relative else LEGACY_LEDGER
    listed = git("ls-tree", "--name-only", f"{base}^{{tree}}", "--", f"{relative}/", legacy)
    if listed.returncode != 0:
        return None
    names = listed.stdout.splitlines()
    files = [n for n in names if n.startswith(f"{relative}/") and n.endswith(".json")]
    if legacy in names and files:
        raise SystemExit(f"parity-check: {base} holds both {legacy} and {relative}/; it has no one ledger to compare with")
    if files:
        ledger: dict[str, dict] = {}
        services = set()
        for name in files:
            service = name[len(relative) + 1 : -len(".json")]
            if service not in TEST_DIRS:
                raise SystemExit(f"parity-check: {base}:{name} names no service in TEST_DIRS")
            services.add(service)
            ledger.update(parse_service_ledger(service, git_show(base, name), f"{base}:{name}"))
        return ledger, services
    if legacy in names:
        ledger = loads_unique(git_show(base, legacy), f"{base}:{legacy}")
        validate(ledger, f"{base}:{legacy}")
        return ledger, {service_of(nodeid) for nodeid in ledger}
    return None


def stale_demotions(current: dict[str, dict], demotions: dict[str, str], where: str) -> list[str]:
    """An entry whose test passes in the working tree no longer excuses
    anything. An excluded, failing or absent row keeps its entry."""
    return [
        f"  {where}: {nodeid}: passes in the gostack column again; remove the entry"
        for nodeid in sorted(demotions)
        if nodeid in current and current[nodeid]["gostack"] == "pass"
    ]


def compare_rows(base: dict[str, dict], current: dict[str, dict], demotions: dict[str, str]) -> tuple[list[str], list[str], list[str]]:
    """(regressions, warnings, demoted) between the base and the working tree.
    gostack: pass to anything else is a regression unless demotions lists it.
    localstack: pass to fail is a regression and any other change warns; that
    column records LocalStack's own result and is not gated further."""
    regressions, warnings, demoted = [], [], []
    for nodeid, before in sorted(base.items()):
        after = current.get(nodeid)
        if before["gostack"] == "pass":
            now = after["gostack"] if after else "missing"
            if now != "pass":
                if nodeid in demotions:
                    demoted.append(f"  gostack: {nodeid}: pass -> {now} ({demotions[nodeid]})")
                else:
                    regressions.append(f"  gostack: {nodeid}: pass -> {now}")
        if before["localstack"] == "pass":
            now = after["localstack"] if after else "missing"
            if now == "fail":
                regressions.append(f"  localstack: {nodeid}: pass -> fail")
            elif now != "pass":
                warnings.append(f"  localstack: {nodeid}: pass -> {now}")
    return regressions, warnings, demoted


def check(args: argparse.Namespace) -> None:
    current = load_ledger(args.ledger)
    exclusions = load_exclusions(args.exclusions)
    demotions = load_demotions(args.demotions)
    demotions_name = args.demotions.name
    stale_entries = stale_demotions(current, demotions, demotions_name)
    committed = committed_ledger(args.base, args.ledger)
    if committed is None:
        if stale_entries:
            print("parity-check: stale demotion(s):\n" + "\n".join(stale_entries), file=sys.stderr)
            raise SystemExit(1)
        print(
            f"parity-check: no committed ledger at {args.base}; {len(current)} entries, "
            f"{len(exclusions)} exclusion patterns, nothing to compare"
        )
        return
    base, base_services = committed
    regressions: list[str] = []
    # A service file deleted outright would otherwise read as every one of its
    # rows going missing.
    present = set(service_files(args.ledger, ".json"))
    for service in sorted(base_services - present):
        regressions.append(f"  {args.ledger.resolve().relative_to(ROOT).as_posix()}/{service}.json: deleted, {args.base} has it")
    rows, warnings, demoted = compare_rows(base, current, demotions)
    regressions += rows + stale_entries
    if demoted:
        print(f"parity-check: {len(demoted)} demotion(s) listed in {demotions_name}:")
        print("\n".join(demoted))
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
    services = sorted({service_of(n) for n in ledger})
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
    # A passing row that is removed stops passing, so check needs a reviewed
    # demotion for it; the reason says why.
    passing = [nodeid for nodeid in stale if ledger[nodeid]["gostack"] == "pass"]
    if passing:
        demotions = load_demotions(args.demotions)
        reason = f"gone upstream at {pinned_localstack_sha()}"
        demotions.update(dict.fromkeys(passing, reason))
        write_demotions(args.demotions, demotions)
    for nodeid in stale:
        del ledger[nodeid]
    write_ledger(args.ledger, ledger)
    write_docs(ledger, args.docs)


# --- services --------------------------------------------------------------


def registration_services(diff: str, names: set[str]) -> set[str] | None:
    """The services a `git diff -U0` of REGISTRATION_FILE changes: every service
    in names that an added or removed line names as a word, outside a `//`
    comment. None when a changed line of code names no service, so the change
    cannot be attributed line by line. A change to comments or blank lines
    alone names nothing and changes nothing."""
    pattern = re.compile(r"\b(?:" + "|".join(re.escape(name) for name in sorted(names)) + r")\b") if names else None
    found: set[str] = set()
    changed = False
    for line in diff.splitlines():
        if line.startswith(("+++", "---")) or not line.startswith(("+", "-")):
            continue
        changed = True
        # Cutting at the first `//` may cut a string such as "http://"; that only
        # loses names, so the line is attributed to fewer services or to none.
        code = line[1:].split("//", 1)[0]
        if not code.strip():
            continue
        named = set(pattern.findall(code)) if pattern else set()
        if not named:
            return None
        found |= named
    # The path list says the file changed; a diff without a changed line
    # disagrees, so nothing is attributed.
    return found if changed else None


def registration_diff(revisions: str) -> str | None:
    shown = subprocess.run(
        ["git", "-C", str(ROOT), "diff", "--no-ext-diff", "-U0", revisions, "--", REGISTRATION_FILE],
        capture_output=True,
        text=True,
        check=False,
    )
    return shown.stdout if shown.returncode == 0 else None


def service_dirs() -> set[str]:
    """The services with a directory under internal/service/ in the working
    tree: the names a registration line can register."""
    root = ROOT / SERVICE_DIR
    return {p.name for p in root.iterdir() if p.is_dir() and p.name in TEST_DIRS} if root.is_dir() else set()


def parity_file_service(path: str) -> str | None:
    """The service of a per-service ledger or exclusions file, or None. Any
    other path under parity/ is shared."""
    for directory, suffix in (("parity/ledger/", ".json"), ("parity/exclusions/", ".yaml")):
        name = path.removeprefix(directory)
        if path.startswith(directory) and "/" not in name and name.endswith(suffix):
            service = name[: -len(suffix)]
            return service if service in TEST_DIRS else None
    return None


def own_service(path: str) -> str | None:
    """The service a path under internal/service/<svc>/ belongs to, when <svc>
    is in TEST_DIRS. A file directly under internal/service/, or a directory
    there that names no service, is shared."""
    if not path.startswith(SERVICE_DIR):
        return None
    service, slash, _ = path[len(SERVICE_DIR) :].partition("/")
    return service if slash and service in TEST_DIRS else None


def with_callers(services: set[str]) -> set[str]:
    """services and every service that calls one of them in this process,
    following IN_PROCESS_CALLS to its end."""
    selected = set(services)
    while True:
        callers = {caller for caller, callees in IN_PROCESS_CALLS.items() if selected.intersection(callees)}
        if callers <= selected:
            return selected
        selected |= callers


def no_image_effect(path: str) -> bool:
    if path.startswith(WORKFLOWS) and path != PARITY_WORKFLOW:
        return True
    return path in NO_IMAGE_EFFECT or path.startswith(tuple(p for p in NO_IMAGE_EFFECT if p.endswith("/")))


def affected_services(paths: list[str], known: set[str], registration: set[str] | None = None) -> set[str]:
    """The services that a change to paths can affect. Fail-safe: a path
    selects every known service unless it is
    - under internal/service/<svc>/: <svc> and the services that call it in
      process (IN_PROCESS_CALLS);
    - parity/ledger/<svc>.json or parity/exclusions/<svc>.yaml: <svc>;
    - REGISTRATION_FILE, when registration holds the services its changed
      lines name (registration_services): those and their callers. None means
      the change could not be attributed;
    - in NO_IMAGE_EFFECT or another workflow than parity.yml: nothing."""
    code: set[str] = set()
    files: set[str] = set()
    shared = False
    for path in paths:
        service = own_service(path)
        ledger_file = parity_file_service(path)
        if service is not None:
            code.add(service)
        elif ledger_file is not None:
            files.add(ledger_file)
        elif path == REGISTRATION_FILE and registration is not None:
            code |= registration
        elif not no_image_effect(path):
            shared = True
    affected = with_callers(code) | files
    return set(known) | affected if shared else affected


def read_paths(text: str) -> list[str]:
    """The names `git diff --name-only -z` printed. Names are NUL-terminated
    and kept byte for byte: a name may hold spaces, quotes or a newline."""
    if text and "\0" not in text:
        raise SystemExit("services --changed: expected NUL-separated names from `git diff --name-only -z --no-renames`")
    return [name for name in text.split("\0") if name]


def services(args: argparse.Namespace) -> None:
    known = {service_of(nodeid) for nodeid in load_ledger(args.ledger)}
    if not args.changed:
        print(",".join(sorted(known)))
        return
    paths = read_paths(sys.stdin.read())
    registration = None
    if args.range and REGISTRATION_FILE in paths:
        diff = registration_diff(args.range)
        # Git failing to produce the diff keeps every service selected.
        registration = registration_services(diff, service_dirs()) if diff is not None else None
    print(",".join(sorted(affected_services(paths, known, registration))))


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
        "Generated by `parity/ledger.py` from `parity/ledger/`. Do not edit.\n\n"
        "gostack runs LocalStack's own pytest suite unchanged. Parity is the share of tests "
        "that LocalStack passes that gostack also passes in the same runner. Tests LocalStack "
        "fails are recorded and not required. Excluded tests are listed with a reason in "
        "`parity/exclusions/`.\n\n" + header + "\n".join(rows) + "\n",
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
    parser.add_argument("--ledger", type=Path, default=LEDGER, help="the per-service ledger directory")
    parser.add_argument("--exclusions", type=Path, default=EXCLUSIONS, help="the per-service exclusions directory")
    parser.add_argument("--docs", type=Path, default=DOCS)
    parser.add_argument("--demotions", type=Path, default=DEMOTIONS, help="the reviewed demotions file")
    sub =parser.add_subparsers(dest="command", required=True)

    merge_parser = sub.add_parser("merge", help="merge a run into the ledger")
    merge_parser.add_argument("--column", choices=COLUMNS, required=True)
    merge_parser.add_argument("--junit", type=Path, required=True)
    merge_parser.add_argument("--excluded", type=Path, required=True)
    merge_parser.set_defaults(func=merge)

    check_parser = sub.add_parser("check", help="fail when a test that passed at the base stops passing")
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
    services_parser.add_argument("--changed", action="store_true", help="read NUL-separated changed paths from stdin")
    services_parser.add_argument("--range", help="git revisions A...B of the change; a change to cmd/gostack/services.go then selects the services its changed lines name")
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
