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
           (default HEAD; on a pull request CI passes HEAD^1 of GitHub's merge
           ref, the base the merged tree was made from). It fails when:
           - a nodeid that was `pass` in the gostack column is anything else
             now (fail, skip, xfail, excluded, null, or a missing row) and
             parity/demotions.yaml does not list it. Each one is named;
           - parity/demotions.yaml lists a nodeid that is `pass` in the
             gostack column now (the test passes again; remove the entry), or
             says gone upstream for a row that is still in the ledger;
           - a nodeid that was `pass` in the localstack column is `fail` now
             (any other change there only warns);
           - a service file the base has is gone from the working tree;
           - REV is no commit, or is the all-zero sha while HEAD has a parent.
             The all-zero sha with a root HEAD is the first commit, and a
             commit with no ledger has nothing to compare; both pass.
           The base is read in either layout: parity/ledger/*.json, or the
           single parity/ledger.json that came before it. Also checks every
           exclusions file. When parity/out/collected.json exists (written by
           prune), also warns about ledger rows that collection no longer
           found. Never a failure.
    uncommitted --services SERVICES --junit FILE --excluded FILE [--rev REV]
           Fail when the gostack column of a listed service in the working
           tree differs from the file committed at REV (default HEAD), in
           either direction, a row missing on one side included. Fail too on
           a row of a listed service committed at REV whose test the run did
           not collect (not in the run's junit report or excluded list),
           unless the row is `excluded` or parity/demotions.yaml lists it:
           merge leaves that row as it was, so the run never observed it.
           parity.yml runs it after `make parity`, so a pull request carries
           the ledger its run observed. Other services are not compared.
    merge-parent --head SHA
           Print HEAD's first parent when HEAD is GitHub's merge of the pull
           request head SHA into its base: two parents, the second SHA. Fail
           otherwise.
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
    services [--changed [--range A..B]]
           Print the services gostack runs: those the ledger covers that have
           a directory under internal/service/, comma separated. A service
           with ledger rows and no such directory (a LocalStack baseline gostack
           does not serve yet) is never printed, selected by any path. With
           --changed, read NUL-separated changed paths from stdin (`git diff
           --name-only -z --no-renames`) and print the services that change can
           affect (empty when none). A path selects every such service unless it
           is one service's own path (and that service is not in
           ROUTING_REACH), a Go test file, or is in NO_IMAGE_EFFECT; see
           affected_services. cmd/gostack/settings-<svc>.go is <svc>'s own
           path too, whether or not <svc> is in ROUTING_REACH, and so is
           internal/service/<svc>/developer-*.go, the service's developer
           endpoints (Registration.DeveloperMount). --range names
           the change, so a change to cmd/gostack/services.go that only edits
           whole registrations() entries and service imports selects the
           services those lines name instead of every service.
    test-dirs SERVICES
           Print the LocalStack test path (tests/aws/services/<dir>) of each
           comma separated service, one per line. parity/run.sh runs these.
    tables Print DEPENDS_ON, ROUTING_REACH, GATEWAY_NAMESPACES and the services
           of TEST_DIRS as JSON, for the Go test that checks them against the code
           (cmd/gostack, Test_parityTables).
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
# The source and API paths of every service in TEST_DIRS are seeded by name
# (own_service, settings_service), so a service branch adds no entry here:
# internal/service/<svc>/, internal/api/<svc>/ and cmd/gostack/settings-<svc>.go
# select <svc> as soon as the service is built.

# The file that lists the registered services. A changed line of it selects
# the services it names only when it is one whole element of the list
# registrations() returns, or an import of a service's package; any other
# changed line of code selects every service (registration_services).
REGISTRATION_FILE = "cmd/gostack/services.go"
# Each service's settings mapping is its own file, so a service branch does not
# edit the registration file for it: cmd/gostack/settings-<svc>.go, where <svc>
# is in TEST_DIRS, is that service's own path. A test file of it is not in the
# binary (GO_TEST_SUFFIX), and the shared cmd/gostack/settings.go is not named
# like one.
SETTINGS_DIR = "cmd/gostack/"
SETTINGS_PREFIX = "settings-"
SETTINGS_SUFFIX = ".go"
# A service's developer endpoints (Registration.DeveloperMount) live in files of
# its package named developer-*.go. The gateway scopes the router they get to
# the service's own prefix, so they hold no routing hook, and a change to one
# selects <svc> and its dependents, never every service, even when <svc> is in
# ROUTING_REACH. That holds only while the hooks run nothing these files
# declare: Test_developerFilesHoldNoRoutingHook fails when a hook function is
# declared in one, or a file of the package outside them names a constant, type
# or function they declare (a method only when its name is theirs alone), other
# than the DeveloperMount function itself. Put code both share in another file.
DEVELOPER_PREFIX = "developer-"
DEVELOPER_SUFFIX = ".go"
SERVICE_PACKAGE = "github.com/kylecannon/gostack/internal/service/"

# A change under internal/service/<svc>/ or internal/api/<svc>/ (generated from
# that service's model, imported by internal/service/<svc>/ alone, and own only
# once that directory exists), where <svc> is in TEST_DIRS, is that service's
# own. Every other path selects every
# service, except these, which cannot change what the image does or how the
# suite runs against it.
SERVICE_DIR = "internal/service/"
OWN_DIRS = (SERVICE_DIR, "internal/api/")
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
    "LICENSE",
)
# go build leaves out every file with this suffix, wherever it is, so none of
# them is in the binary. codegen/ stays shared: its non-test code makes
# internal/api.
GO_TEST_SUFFIX = "_test.go"

# The services whose Registration sets a hook that runs on requests meant for
# any service: Mount, ClaimHost, Claim, Fallback or Preprocess. A change to one
# of them can break another service's suite, so it selects every service.
# Test_parityTables in cmd/gostack walks registrations() and fails when this
# differs from the hooks the services set.
# - s3 claims its hosts, falls back to unsigned path-style calls and
#   preprocesses every request.
# - sqs mounts its queue-URL paths, /<account>/<queue>, which a path-style
#   call to another service can match.
# A Mount whose every route is under GATEWAY_NAMESPACES is not a hook of this
# kind, since no AWS call is made to those paths: dynamodb mounts DELETE
# /_gostack/services/dynamodb/expired and its /_aws name, and is not in the list.
# A DeveloperMount is not counted at all: the gateway gives it a router scoped to
# the service's own /_gostack/services/<svc> and /_aws/<svc> prefixes, which no
# other service's request reaches. sqs's developer endpoint is one, and sqs is in
# the list for its queue-URL Mount alone.
ROUTING_REACH = ("s3", "sqs")
# The path prefixes the gateway keeps for its own endpoints: /_gostack, and the
# /_localstack and /_aws names it also answers. Test_parityTables reads them
# from here, so a Mount confined to them is the one kind of Mount that does not
# reach another service's requests.
GATEWAY_NAMESPACES = ("/_aws/", "/_gostack/", "/_localstack/")
# Every workflow but this one: they run checks, they do not build the image
# parity runs against.
WORKFLOWS = ".github/workflows/"
PARITY_WORKFLOW = ".github/workflows/parity.yml"

# The services whose tests a change to another service can break: each key's
# tests depend on the services it lists, so a change to one of those selects
# the key too. EVERY_SERVICE as a key is every service the ledger covers. A
# key is selected only when it is served (a directory under internal/service/):
# rows in the ledger do not count, so a service with only a LocalStack baseline
# is never run against gostack.
# The seeded entries are for the services built next, so the branch that builds
# one edits no table.
# - s3 delivers event notifications to sqs in this process, and will call sns
#   and kms.
# - sns publishes to sqs in this process.
# - ssm reads secretsmanager parameters.
# - dynamodbstreams reads dynamodb's state, and dynamodb writes the stream
#   records dynamodbstreams serves: a change to either runs both. The dynamodb
#   entry is in no import, so it stays reviewed by hand (Test_parityTables).
# - LocalStack's session fixtures account_id and secondary_account_id call
#   sts.get_caller_identity in every service's test session, so a change to
#   sts can break any service's tests.
# Test_parityTables in cmd/gostack checks the keys other than EVERY_SERVICE
# against the code: every service whose package imports internal/inproc or
# another service's package is a key, and lists every service whose package it
# imports. Which service an inproc call reaches, and the EVERY_SERVICE entry,
# are not in the imports; they stay reviewed by hand.
EVERY_SERVICE = "*"
# The keys that are not callers yet, declared ahead of the code. A key with no
# call in its package selects its service when a dependency changes, which can
# only run a suite that did not need to run, where a caller with no key skips a
# suite its change broke. So Test_parityTables holds "every caller is a key"
# strictly and lets a key that is not a caller stand only when it is listed
# here: the first commit of a service (its API package imported, no in-process
# call written yet) then passes without an edit to a table.
# - sns, ssm: the services that will call sqs and secretsmanager.
# - dynamodb, dynamodbstreams: a pair that depends on each other through the
#   state they share, which is in no import.
# - secretsmanager: its package calls no service, but a test of its suite makes
#   a key with kms (test_cross_account_access_non_default_key).
# A key stays listed after its service becomes a real caller; its dependencies
# are still held to the services that exist.
DECLARED_AHEAD = ("dynamodb", "dynamodbstreams", "secretsmanager", "sns", "ssm")
DEPENDS_ON = {
    "dynamodb": ("dynamodbstreams", "kms"),
    # dynamodbstreams imports dynamodb, which imports kms for its header.
    "dynamodbstreams": ("dynamodb", "kms"),
    "s3": ("kms", "sns", "sqs"),
    # test_cross_account_access_non_default_key makes a key with kms.
    "secretsmanager": ("kms",),
    "sns": ("sqs",),
    "ssm": ("secretsmanager",),
    EVERY_SERVICE: ("sts",),
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


# The reason prune gives the passing rows it removes.
GONE_UPSTREAM = "gone upstream"


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
    came before them. None only when base has neither. base is a commit
    (resolve_base)."""
    relative = directory.resolve().relative_to(ROOT).as_posix()
    legacy = f"{relative.rsplit('/', 1)[0]}/{LEGACY_LEDGER}" if "/" in relative else LEGACY_LEDGER
    listed = git("ls-tree", "--name-only", f"{base}^{{tree}}", "--", f"{relative}/", legacy)
    if listed.returncode != 0:
        raise SystemExit(f"parity-check: cannot list {base}: {listed.stderr.strip()}")
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


# An all-zero sha is how GitHub names "no commit": github.event.before of a push
# that created the branch.
NULL_SHA = re.compile(r"0{40}|0{64}")


def resolve_base(base: str) -> str | None:
    """The commit base names, or None when there is genuinely nothing before
    this change: base is the all-zero sha and HEAD is a root commit, the first
    commit of the repository. An all-zero base with history behind HEAD, or a
    name that resolves to no commit, fails: comparing with nothing there
    would pass any regression."""
    if NULL_SHA.fullmatch(base):
        if git("rev-parse", "--verify", "--quiet", "HEAD^{commit}").returncode != 0:
            raise SystemExit("parity-check: the base is the all-zero sha and HEAD is no commit")
        if git("rev-parse", "--verify", "--quiet", "HEAD^").returncode == 0:
            raise SystemExit(
                f"parity-check: the base is the all-zero sha {base}, which names no commit, and HEAD has a parent; "
                "pass the commit to compare with"
            )
        return None
    resolved = git("rev-parse", "--verify", "--quiet", f"{base}^{{commit}}")
    if base.startswith("-") or resolved.returncode != 0:
        raise SystemExit(f"parity-check: the base {base!r} is not a commit in this repository")
    return resolved.stdout.strip()


def stale_demotions(current: dict[str, dict], demotions: dict[str, str], where: str) -> list[str]:
    """An entry whose test passes in the working tree no longer excuses
    anything. An excluded, failing or absent row keeps its entry, except that a
    gone upstream entry needs its row gone: prune deletes the row it writes the
    entry for, so a row that is still there was not pruned."""
    stale = []
    for nodeid in sorted(demotions):
        if nodeid not in current:
            continue
        if current[nodeid]["gostack"] == "pass":
            stale.append(f"  {where}: {nodeid}: passes in the gostack column again; remove the entry")
        elif demotions[nodeid].startswith(GONE_UPSTREAM):
            stale.append(f"  {where}: {nodeid}: says {GONE_UPSTREAM}, but its row is still in the ledger; give the real reason")
    return stale


def unserved_results(rows: dict[str, dict], served: set[str]) -> list[str]:
    """A service with gostack results and no internal/service/<svc>/ package.
    Selection runs a service's suite only while that package exists, so its
    committed passes would stop being run and still be believed. A service with
    only a LocalStack baseline (no gostack result) is not one."""
    ran: Counter = Counter()
    for nodeid, entry in rows.items():
        if entry["gostack"] not in (None, "excluded") and (service := service_of(nodeid)) not in served:
            ran[service] += 1
    return [
        f"  {service}: {count} gostack result(s) in the ledger and no internal/service/{service}/ package, "
        f"so its suite is no longer run; remove the rows (parity/ledger/{service}.json) or bring the service back"
        for service, count in sorted(ran.items())
    ]


# A reason names its services after depends-on: one, or several joined by + with
# no space (depends-on:kms+lambda). Text after the list, such as a note after a
# comma or in parentheses, is not read.
DEPENDS_ON_REASON = re.compile(r"^depends-on:([\w-]+(?:\+[\w-]+)*)")
# The reasons spell one service two ways: s3.yaml says eventbridge, ssm.yaml
# says events, and gostack knows it as events. No other reason in use is an
# alias.
DEPENDENCY_ALIASES = {"eventbridge": "events"}


def reason_services(reason: str) -> list[str]:
    """The services a depends-on reason names, as gostack names them."""
    named = DEPENDS_ON_REASON.match(reason)
    if not named:
        return []
    return [DEPENDENCY_ALIASES.get(name, name) for name in named.group(1).split("+")]


def served_dependencies(exclusions: dict[str, str], served: set[str]) -> list[str]:
    """An exclusion whose reason names a service with an
    internal/service/<svc>/ package after depends-on:. The reason said the test
    needs a service gostack lacks; once it has the service the test runs, and
    an exclusion left in place hides a row that passes or a gap that should show
    as a failure. A reason that names several fails for each one served: it
    should name what is still missing."""
    stale = []
    for pattern, reason in sorted(exclusions.items()):
        for name in reason_services(reason):
            if name in served:
                stale.append(
                    f"  parity/exclusions/{service_of(pattern)}.yaml: {pattern}: {reason}, but {name} is served "
                    f"(internal/service/{name}/); remove the exclusion and let the test run, or give the real reason"
                )
    return stale


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
    served = service_dirs()
    stale_entries = (
        stale_demotions(current, demotions, demotions_name)
        + unserved_results(current, served)
        + served_dependencies(exclusions, served)
    )
    commit = resolve_base(args.base)
    committed = committed_ledger(commit, args.ledger) if commit is not None else None
    if committed is None:
        if stale_entries:
            print("parity-check: problem(s) in the ledger:\n" + "\n".join(stale_entries), file=sys.stderr)
            raise SystemExit(1)
        where = f"at {args.base}" if commit is not None else f"before the first commit ({args.base})"
        print(
            f"parity-check: no committed ledger {where}; {len(current)} entries, "
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


# --- uncommitted -------------------------------------------------------------


def committed_service(rev: str, directory: Path, service: str) -> dict[str, dict]:
    """The rows of one service's file committed at rev; none when rev has no
    such file."""
    relative = f"{directory.resolve().relative_to(ROOT).as_posix()}/{service}.json"
    listed = git("ls-tree", "--name-only", f"{rev}^{{tree}}", "--", relative)
    if listed.returncode != 0:
        raise SystemExit(f"uncommitted: cannot list {rev}: {listed.stderr.strip()}")
    if not listed.stdout.strip():
        return {}
    return parse_service_ledger(service, git_show(rev, relative), f"{rev}:{relative}")


def gostack_value(rows: dict[str, dict], nodeid: str) -> str:
    if nodeid not in rows:
        return "missing"
    value = rows[nodeid]["gostack"]
    return "null" if value is None else value


def run_collected(junit: Path, excluded: Path) -> set[str]:
    """The nodeids a run collected, from what the run wrote: every testcase of
    its junit report, and every nodeid in the plugin's excluded list. The
    plugin skips an excluded test, so the report holds it as well; the list is
    the record merge writes excluded rows from."""
    for path in (junit, excluded):
        if not path.is_file():
            raise SystemExit(f"uncommitted: {path} is missing; the run left nothing to say which tests it collected")
    return set(read_junit(junit)) | set(json.loads(excluded.read_text(encoding="utf-8"))["excluded"])


def uncommitted(args: argparse.Namespace) -> None:
    """Fail when the gostack column a run wrote into the working tree differs
    from the ledger committed at --rev, for the services the run selected, or
    when a committed row of theirs is one the run never collected: merge
    leaves such a row as it is, so its committed value was not observed."""
    selected = sorted({name for name in args.services.split(",") if name})
    for service in selected:
        test_dir(service)
    refuse_legacy(args.ledger, LEGACY_LEDGER)
    files = service_files(args.ledger, ".json")
    collected = run_collected(args.junit, args.excluded)
    demotions = load_demotions(args.demotions)
    differences, unobserved = [], []
    for service in selected:
        committed = committed_service(args.rev, args.ledger, service)
        path = files.get(service)
        observed = parse_service_ledger(service, path.read_text(encoding="utf-8"), str(path)) if path else {}
        for nodeid in sorted(committed.keys() | observed.keys()):
            before, after = gostack_value(committed, nodeid), gostack_value(observed, nodeid)
            if before != after:
                differences.append(f"  {nodeid}: committed {before}, observed {after}")
            elif nodeid in committed and nodeid not in collected and before != "excluded" and nodeid not in demotions:
                unobserved.append(f"  {nodeid}: committed {before}, not collected by this run")
    if unobserved:
        print(
            f"parity: {len(unobserved)} row(s) committed at {args.rev} name a test this run did not collect:",
            file=sys.stderr,
        )
        print("\n".join(unobserved), file=sys.stderr)
        print(
            "A test that is gone upstream leaves the ledger with `make parity-prune`; a row for a test "
            f"that never existed is removed; either way a passing row needs its entry in {args.demotions.name}.",
            file=sys.stderr,
        )
    if differences:
        print(
            f"parity: the gostack column this run observed differs from the ledger committed at {args.rev} "
            f"in {len(differences)} row(s):",
            file=sys.stderr,
        )
        print("\n".join(differences), file=sys.stderr)
        print(
            "Commit the ledger this run wrote (parity/ledger/ in the parity artifact, or `make parity` "
            "locally): the committed ledger is what the ratchet protects. A test whose result changes "
            "from run to run is excluded with a reason and demoted, not left to flap.",
            file=sys.stderr,
        )
    if differences or unobserved:
        raise SystemExit(1)
    print(f"parity: the gostack column of {', '.join(selected) or 'no service'} matches the ledger committed at {args.rev}")


# --- merge-parent ------------------------------------------------------------


def merge_parent(args: argparse.Namespace) -> None:
    """Print HEAD's first parent when HEAD is the merge of the pull request
    head into its base, as GitHub's merge ref (refs/pull/N/merge) is: exactly
    two parents, the second being --head, and HEAD not --head itself. Anything
    else fails, so a run never falls back to comparing the head alone."""
    shown = git("rev-list", "--parents", "-n", "1", "HEAD")
    if shown.returncode != 0:
        raise SystemExit(f"merge-parent: cannot read HEAD: {shown.stderr.strip()}")
    head, *parents = shown.stdout.split()
    want = git("rev-parse", "--verify", "--quiet", f"{args.head}^{{commit}}") if args.head and not args.head.startswith("-") else None
    if want is None or want.returncode != 0:
        raise SystemExit(f"merge-parent: the pull request head {args.head!r} is not a commit in this checkout")
    pr_head = want.stdout.strip()
    if head == pr_head:
        raise SystemExit(f"merge-parent: HEAD is the pull request head {pr_head}, not GitHub's merge of it into the base")
    if len(parents) != 2:
        raise SystemExit(
            f"merge-parent: HEAD {head} has {len(parents)} parent(s), not 2: it is not GitHub's merge of the pull request "
            "into its base. GitHub makes no merge ref for a pull request that cannot merge; resolve the conflict."
        )
    if parents[1] != pr_head:
        raise SystemExit(f"merge-parent: HEAD {head} merges {parents[1]}, not the pull request head {pr_head}")
    print(parents[0])


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
        reason = f"{GONE_UPSTREAM} at {pinned_localstack_sha()}"
        demotions.update(dict.fromkeys(passing, reason))
        write_demotions(args.demotions, demotions)
    for nodeid in stale:
        del ledger[nodeid]
    write_ledger(args.ledger, ledger)
    write_docs(ledger, args.docs)


# --- services --------------------------------------------------------------


# Lines that start like a `//` comment and are read by the go command or the
# compiler: build constraints, //go:debug, //go:embed, //line and the rest.
DIRECTIVES = ("//go:", "//line ", "// +build")
REGISTRATIONS_FUNC = "func registrations("
REGISTRATIONS_LIST = "return []service.Registration{"
IMPORT_LINE = re.compile(r'(?:import\s+)?(?:[A-Za-z_]\w*\s+|[._]\s+)?"' + re.escape(SERVICE_PACKAGE) + r'(\w+)"')
HUNK = re.compile(r"@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@")
# A diff of REGISTRATION_FILE with this much context is the whole file on
# both sides, in one hunk.
WHOLE_FILE = 1_000_000


def go_code(line: str) -> tuple[str, str, bool]:
    """(code, blanked, closed) of one line of Go: code is the line without its
    `//` comment, blanked is code with the text of every string and rune
    literal removed, and closed says no literal runs past the line. A
    directive is code, whole."""
    if line.lstrip().startswith(DIRECTIVES):
        return line, line, True
    code: list[str] = []
    blanked: list[str] = []
    quote = None
    i = 0
    while i < len(line):
        char = line[i]
        if quote is None and line.startswith("//", i):
            break
        if quote is None:
            quote = char if char in "\"'`" else None
            blanked.append(char)
        elif char == quote:
            quote = None
            blanked.append(char)
        elif char == "\\" and quote != "`":
            code.append(line[i : i + 2])
            i += 2
            continue
        code.append(char)
        i += 1
    return "".join(code), "".join(blanked), quote is None


def block_comment_lines(lines: list[str]) -> set[int]:
    """The indexes of the lines of Go that open, close or lie inside a `/* */`
    comment. A `/*` in a string, a rune literal or a `//` comment opens
    nothing."""
    inside: set[int] = set()
    state = None  # None is code; "/*" a block comment; a quote char, a literal.
    for number, line in enumerate(lines):
        if state == "/*":
            inside.add(number)
        i = 0
        while i < len(line):
            two = line[i : i + 2]
            if state is None:
                if two == "//":
                    break
                if two == "/*":
                    state = "/*"
                    inside.add(number)
                    i += 2
                    continue
                if line[i] in "\"'`":
                    state = line[i]
            elif state == "/*":
                if two == "*/":
                    state = None
                    i += 2
                    continue
            elif line[i] == "\\" and state != "`":
                i += 2
                continue
            elif line[i] == state:
                state = None
            i += 1
        if state in ('"', "'"):
            state = None  # an interpreted string and a rune end with their line
    return inside


def bracket_depths(code: str) -> tuple[int, int]:
    """(lowest, last) depth of the brackets in code, starting from 0."""
    depth = lowest = 0
    for char in code:
        if char in "([{":
            depth += 1
        elif char in ")]}":
            depth -= 1
            lowest = min(lowest, depth)
    return lowest, depth


def element_lines(lines: list[str]) -> set[int]:
    """The indexes of the lines that are each one whole element of the list
    registrations() returns: directly inside the list, brackets balanced,
    literals closed, ending in a comma. None when the file has no such list."""
    start = next((i for i, line in enumerate(lines) if line.startswith(REGISTRATIONS_FUNC)), None)
    if start is None:
        return set()
    opening = None
    for i in range(start + 1, len(lines)):
        if lines[i].startswith("}"):
            return set()
        if go_code(lines[i])[1].strip() == REGISTRATIONS_LIST:
            opening = i
            break
    if opening is None:
        return set()
    elements: set[int] = set()
    depth = 1
    for i in range(opening + 1, len(lines)):
        _, blanked, closed = go_code(lines[i])
        lowest, last = bracket_depths(blanked)
        item = blanked.strip()
        if depth == 1 and closed and lowest >= 0 and last == 0 and item.endswith(",") and item != ",":
            elements.add(i)
        depth += last
        if depth <= 0:
            return elements
    return set()


def diff_sides(diff: str) -> tuple[list[str], list[str], list[tuple[str, int]]] | None:
    """The old file, the new file and each changed line, as ("-", index in the
    old file) or ("+", index in the new one), of a diff that holds the whole
    file in one hunk. None for any other diff."""
    old: list[str] = []
    new: list[str] = []
    changed: list[tuple[str, int]] = []
    hunks = 0
    for line in diff.split("\n"):
        if line.startswith("@@"):
            hunks += 1
            starts = HUNK.match(line)
            if hunks > 1 or starts is None or int(starts.group(1)) > 1 or int(starts.group(2)) > 1:
                return None
        elif hunks and line.startswith(" "):
            old.append(line[1:])
            new.append(line[1:])
        elif hunks and line.startswith("-"):
            changed.append(("-", len(old)))
            old.append(line[1:])
        elif hunks and line.startswith("+"):
            changed.append(("+", len(new)))
            new.append(line[1:])
    return (old, new, changed) if hunks else None


def registration_services(diff: str, names: set[str]) -> set[str] | None:
    """The services a whole-file diff of REGISTRATION_FILE (registration_diff)
    changes, from its changed lines:
    - blank and comment lines name nothing and select nothing; a directive
      (//go:build, //go:debug) is code;
    - an import of the package of a service in names names that service;
    - a line that is one whole element of the list registrations() returns,
      on the side of the diff it is on, names every service in names that it
      names as a word outside a comment or a literal.
    None when any other line of code changed, or a line above names no
    service: a signature, a call of registrations() or a helper changes every
    registration, so the change cannot be attributed line by line. A changed
    line that opens, closes or lies inside a `/* */` comment is code: the block
    can comment out or bring back an element that did not change. None too
    for a diff with no changed line or not in the whole-file form."""
    sides = diff_sides(diff)
    if sides is None or not sides[2]:
        return None
    old, new, changed = sides
    elements = {"-": element_lines(old), "+": element_lines(new)}
    blocks = {"-": block_comment_lines(old), "+": block_comment_lines(new)}
    pattern = re.compile(r"\b(?:" + "|".join(re.escape(name) for name in sorted(names)) + r")\b") if names else None
    found: set[str] = set()
    for side, index in changed:
        if index in blocks[side]:
            return None
        code, blanked, closed = go_code((old if side == "-" else new)[index])
        if not code.strip():
            continue
        imported = IMPORT_LINE.fullmatch(code.strip())
        if imported and closed:
            named = {imported.group(1)} & names
        elif index in elements[side]:
            named = set(pattern.findall(blanked)) if pattern else set()
        else:
            return None
        if not named:
            return None
        found |= named
    return found


def registration_diff(revisions: str) -> str | None:
    """REGISTRATION_FILE's diff over revisions, the whole file as context, so
    each changed line can be placed in the file it belongs to."""
    shown = subprocess.run(
        ["git", "-C", str(ROOT), "diff", "--no-ext-diff", f"-U{WHOLE_FILE}", revisions, "--", REGISTRATION_FILE],
        capture_output=True,
        text=True,
        check=False,
    )
    return shown.stdout if shown.returncode == 0 else None


def service_dirs() -> set[str]:
    """The services with a Go package under internal/service/ in the working
    tree: the names a registration line can register. A directory counts only
    when it holds a non-test .go file, as the Go test counts a package: one
    holding only a test or a README is no service yet."""
    root = ROOT / SERVICE_DIR
    if not root.is_dir():
        return set()
    return {p.name for p in root.iterdir() if p.is_dir() and p.name in TEST_DIRS and holds_go_package(p)}


def holds_go_package(directory: Path) -> bool:
    return any(f.is_file() and f.suffix == ".go" and not f.name.endswith(GO_TEST_SUFFIX) for f in directory.iterdir())


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
    """The service a path under internal/service/<svc>/ or internal/api/<svc>/
    belongs to, when <svc> is in TEST_DIRS. A file directly under either
    directory, or a directory there that names no service, is shared. So is
    internal/api/<svc>/ while internal/service/<svc>/ is not in the tree: a
    generated package no service uses yet would otherwise select a service
    the image does not serve."""
    for directory in OWN_DIRS:
        if path.startswith(directory):
            service, slash, _ = path[len(directory) :].partition("/")
            if not slash or service not in TEST_DIRS:
                return None
            return service if directory == SERVICE_DIR or service in service_dirs() else None
    return None


def settings_service(path: str) -> str | None:
    """The service of cmd/gostack/settings-<svc>.go, when <svc> is in
    TEST_DIRS, or None. Any other file of cmd/gostack, settings.go included, is
    shared."""
    name = path.removeprefix(SETTINGS_DIR)
    if not path.startswith(SETTINGS_DIR) or "/" in name:
        return None
    if not (name.startswith(SETTINGS_PREFIX) and name.endswith(SETTINGS_SUFFIX)):
        return None
    service = name[len(SETTINGS_PREFIX) : -len(SETTINGS_SUFFIX)]
    return service if service in TEST_DIRS else None


def developer_service(path: str) -> str | None:
    """The service of internal/service/<svc>/developer-*.go, when <svc> is in
    TEST_DIRS, or None. A file deeper in the package, or one without the
    prefix, is the service's own path like any other (own_service)."""
    if not path.startswith(SERVICE_DIR):
        return None
    service, slash, name = path[len(SERVICE_DIR) :].partition("/")
    if not slash or "/" in name or service not in TEST_DIRS:
        return None
    return service if name.startswith(DEVELOPER_PREFIX) and name.endswith(DEVELOPER_SUFFIX) else None


def with_dependents(services: set[str], known: set[str], served: set[str]) -> set[str]:
    """services and every service whose tests depend on one of them, following
    DEPENDS_ON to its end. A dependent is added only when it is served: it has
    a directory under internal/service/. A seeded entry for a service nobody
    has built selects nothing, and ledger rows do not make a service built: the
    baseline rows of a service gostack does not serve yet have no gostack run to
    select. A dependency of EVERY_SERVICE adds every known service that is
    served."""
    selected = set(services)
    while True:
        dependents: set[str] = set()
        for service, dependencies in DEPENDS_ON.items():
            if selected.intersection(dependencies):
                dependents |= known if service == EVERY_SERVICE else {service}
        dependents &= served
        if dependents <= selected:
            return selected
        selected |= dependents


def no_image_effect(path: str) -> bool:
    if path.startswith(WORKFLOWS) and path != PARITY_WORKFLOW:
        return True
    if path.endswith(GO_TEST_SUFFIX):
        return True
    return path in NO_IMAGE_EFFECT or path.startswith(tuple(p for p in NO_IMAGE_EFFECT if p.endswith("/")))


def affected_services(paths: list[str], known: set[str], registration: set[str] | None = None) -> set[str]:
    """The services that a change to paths can affect, as a gostack run:
    only a service gostack serves, one with a directory under internal/service/
    (service_dirs), is ever selected, whatever selects it. A service with
    ledger rows and no directory (its LocalStack baseline is committed, gostack
    does not answer it) is left out, so the image is never asked for a suite it
    does not serve. Fail-safe: a path selects every known served service unless
    it is
    - in NO_IMAGE_EFFECT, a Go test file anywhere, or another workflow than
      parity.yml: nothing;
    - under internal/service/<svc>/ or internal/api/<svc>/: <svc> and the
      services whose tests depend on it (DEPENDS_ON), or every service when
      <svc> is in ROUTING_REACH;
    - cmd/gostack/settings-<svc>.go: <svc> and its dependents, as for its
      directory, but never every service on that account: the file maps
      configuration onto <svc>'s settings and holds none of its routing
      hooks, so ROUTING_REACH does not apply to it;
    - internal/service/<svc>/developer-*.go: the same, because the file holds
      the service's developer endpoints, which no other service's request
      reaches (Registration.DeveloperMount);
    - parity/ledger/<svc>.json or parity/exclusions/<svc>.yaml: <svc>;
    - REGISTRATION_FILE, when registration holds the services its changed
      lines name (registration_services): those, as if their directories had
      changed. None means the change could not be attributed."""
    code: set[str] = set()
    contained: set[str] = set()
    files: set[str] = set()
    shared = False
    for path in paths:
        if no_image_effect(path):
            continue
        # A developer file is its service's own path too, but one that holds no
        # routing hook, so it is looked up first and never joins code.
        contained_file = settings_service(path) or developer_service(path)
        service = None if contained_file else own_service(path)
        ledger_file = parity_file_service(path)
        if service is not None:
            code.add(service)
        elif contained_file is not None:
            contained.add(contained_file)
        elif ledger_file is not None:
            files.add(ledger_file)
        elif path == REGISTRATION_FILE and registration is not None:
            code |= registration
        else:
            shared = True
    served = service_dirs()
    # A settings or developer file of a service nobody serves yet changes
    # nothing gostack does, so its dependents are not selected for it either. A
    # service directory that is gone (its files are in the diff, it is not in
    # the tree) still selects its dependents: they called it.
    affected = with_dependents(code | (contained & served), known, served) | files
    selected = set(known) | affected if shared or code.intersection(ROUTING_REACH) else affected
    return selected & served


def read_paths(text: str) -> list[str]:
    """The names `git diff --name-only -z` printed. Names are NUL-terminated
    and kept byte for byte: a name may hold spaces, quotes or a newline."""
    if text and "\0" not in text:
        raise SystemExit("services --changed: expected NUL-separated names from `git diff --name-only -z --no-renames`")
    return [name for name in text.split("\0") if name]


def services(args: argparse.Namespace) -> None:
    known = {service_of(nodeid) for nodeid in load_ledger(args.ledger)}
    if not args.changed:
        print(",".join(sorted(known & service_dirs())))
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


def tables(args: argparse.Namespace) -> None:
    """Print the hand-kept tables Test_parityTables in cmd/gostack checks
    against the code, as JSON."""
    print(
        json.dumps(
            {
                "declared_ahead": list(DECLARED_AHEAD),
                "depends_on": {service: list(dependencies) for service, dependencies in DEPENDS_ON.items()},
                "every_service": EVERY_SERVICE,
                "gateway_namespaces": list(GATEWAY_NAMESPACES),
                "routing_reach": list(ROUTING_REACH),
                "test_dirs": sorted(TEST_DIRS),
            },
            sort_keys=True,
        )
    )


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
        # The share is over the LocalStack passes of services gostack ran. A
        # baseline-only service has none, and would otherwise drag the total down.
        counts["ls_pass_run"] = counts["ls_pass"] if counts["gs_run"] else 0
        totals.update(counts)
        # An anchor on the row, so docs/features.md can link to it: a table row has no heading.
        rows.append(format_row(f'<a id="{service}"></a>{service}', counts))
    rows.append(format_row("**total**", totals, show_share_of=True))

    path.write_text(
        "# Parity\n\n"
        "Generated by `parity/ledger.py` from `parity/ledger/`. Do not edit.\n\n"
        "gostack runs LocalStack's own pytest suite unchanged. Parity is the share of tests "
        "that LocalStack passes that gostack also passes in the same runner. Tests LocalStack "
        "fails are recorded and not required. Excluded tests are listed with a reason in "
        "`parity/exclusions/`.\n\n" + header + "\n".join(rows) + "\n",
        encoding="utf-8",
    )


def format_row(label: str, c: Counter, show_share_of: bool = False) -> str:
    if c["gs_run"] == 0:
        parity = "n/a"
    else:
        parity = f"{100 * c['both'] / c['ls_pass_run']:.1f}%" if c["ls_pass_run"] else "n/a"
        # The LocalStack pass column of the total counts the passes of services
        # gostack did not run; the share does not, so say what it is of.
        if show_share_of and c["ls_pass_run"] and c["ls_pass_run"] != c["ls_pass"]:
            parity += f" ({c['both']} of {c['ls_pass_run']})"
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
    sub = parser.add_subparsers(dest="command", required=True)

    merge_parser = sub.add_parser("merge", help="merge a run into the ledger")
    merge_parser.add_argument("--column", choices=COLUMNS, required=True)
    merge_parser.add_argument("--junit", type=Path, required=True)
    merge_parser.add_argument("--excluded", type=Path, required=True)
    merge_parser.set_defaults(func=merge)

    check_parser = sub.add_parser("check", help="fail when a test that passed at the base stops passing")
    check_parser.add_argument("--base", default="HEAD", help="git revision holding the committed ledger")
    check_parser.add_argument("--collected", type=Path, default=COLLECTED, help="cached collection from prune")
    check_parser.set_defaults(func=check)

    uncommitted_parser = sub.add_parser("uncommitted", help="fail when a run's gostack column differs from the committed ledger")
    uncommitted_parser.add_argument("--services", required=True, help="comma separated services the run selected")
    uncommitted_parser.add_argument("--rev", default="HEAD", help="git revision holding the committed ledger")
    uncommitted_parser.add_argument("--junit", type=Path, required=True, help="the run's junit report")
    uncommitted_parser.add_argument("--excluded", type=Path, required=True, help="the run's excluded.json, from pytest_gostack")
    uncommitted_parser.set_defaults(func=uncommitted)

    merge_parent_parser = sub.add_parser("merge-parent", help="print HEAD^1 when HEAD is GitHub's merge of --head into its base")
    merge_parent_parser.add_argument("--head", required=True, help="the pull request head sha")
    merge_parent_parser.set_defaults(func=merge_parent)

    prune_parser = sub.add_parser("prune", help="remove ledger rows whose test no longer exists upstream")
    prune_parser.add_argument("--localstack", type=Path, help="the pinned LocalStack checkout (default: where parity/run.sh looks)")
    prune_parser.add_argument("--dry-run", action="store_true", help="print the rows, change nothing")
    prune_parser.add_argument("--collected", type=Path, help="a saved `pytest --collect-only -q` listing, no Docker")
    prune_parser.add_argument("--cache", type=Path, default=COLLECTED, help=argparse.SUPPRESS)
    prune_parser.set_defaults(func=prune)

    services_parser = sub.add_parser("services", help="print the ledger's services, or those a change affects")
    services_parser.add_argument("--changed", action="store_true", help="read NUL-separated changed paths from stdin")
    services_parser.add_argument("--range", help="git revisions A..B of the change; a change to cmd/gostack/services.go then selects the services its changed lines name")
    services_parser.set_defaults(func=services)

    dirs_parser = sub.add_parser("test-dirs", help="print the LocalStack test path of each service")
    dirs_parser.add_argument("services", help="comma separated service names")
    dirs_parser.set_defaults(func=test_dirs)

    tables_parser = sub.add_parser("tables", help="print DEPENDS_ON, ROUTING_REACH and GATEWAY_NAMESPACES as JSON")
    tables_parser.set_defaults(func=tables)

    docs_parser = sub.add_parser("docs", help="rewrite docs/parity.md")
    docs_parser.set_defaults(func=docs)

    args = parser.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
