"""Tests for parity/features.py. Standard library only:

    python3 -m unittest discover -s parity -p 'test_*.py'

The page itself is held to the code by `make features-check`, which needs the Go
facts; these tests hold the generator's rules and the committed page's links.
"""

from __future__ import annotations

import os
import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

import features
import ledger

PARITY = '# Parity\n\n| Service |\n|---|\n| <a id="sqs"></a>sqs |\n| <a id="sts"></a>sts |\n\n## Notes\n'
COMPAT = (
    "# Differences\n\n"
    "## SQS\n\n| Setting | Row |\n|---|---|\n| a | b |\n\n"
    "### DynamoDB\n\n"
    "## DynamoDB Streams\n\n"
    "## Request limits\n\n| Limit | Row |\n|---|---|\n| SQS bodies, DynamoDB items | 1 |\n| STS tokens | 2 |\n\n"
    "```\n## STS in a fence\n```\n"
)

SERVICES = [
    {"name": "acm"},
    {"name": "configservice", "ledger": "config"},
    {"name": "dynamodb", "compat": ["DynamoDB"]},
    {"name": "dynamodbstreams", "compat": ["DynamoDB Streams"]},
    {"name": "ecr", "providers_only": True},
    {"name": "events"},
    {"name": "opensearch", "deferred": "The engine is deferred."},
    {"name": "sqs", "compat": ["SQS"]},
    {"name": "sts", "compat": ["STS"]},
]


def fact(name: str, operations: list[str], implemented: list[str] | None, persist: bool = False) -> dict:
    return {"name": name, "operations": operations, "implemented": implemented, "persist": persist}


def row(localstack: str | None, gostack: str | None, reason: str = "") -> dict:
    return {"localstack": localstack, "gostack": gostack, "reason": reason}


def render(facts: list[dict], rows: dict[str, dict] | None = None, parity: str = PARITY, services: list[dict] | None = None) -> str:
    return features.render(
        "8b9a79f",
        services or SERVICES,
        {f["name"]: f for f in facts},
        rows or {},
        parity,
        COMPAT,
    )


def summary_row(page: str, name: str) -> list[str]:
    line = next(line for line in page.splitlines() if line.startswith(f"| [{name}]("))
    return [cell.strip() for cell in line.strip("|").split("|")]


def section(page: str, name: str) -> str:
    return page.split(f"### {name}\n")[1].split("\n## ")[0].split("\n### ")[0]


SQS_FACT = fact("sqs", ["CreateQueue", "DeleteQueue", "ListQueues"], ["CreateQueue", "ListQueues"], persist=True)
SQS_ALL = fact("sqs", ["CreateQueue", "ListQueues"], ["CreateQueue", "ListQueues"], persist=True)
SQS_TEST = "tests/aws/services/sqs/test_sqs.py::TestSqs::test_a"


def sqs_rows(*pairs: tuple[str | None, str | None, str]) -> dict[str, dict]:
    return {f"{SQS_TEST}{i}": row(localstack, gostack, reason) for i, (localstack, gostack, reason) in enumerate(pairs)}


def status_of(facts: list[dict], rows: dict[str, dict] | None = None) -> str:
    return summary_row(render(facts, rows), "sqs")[1]


class StatusTest(unittest.TestCase):
    """Every input combination falls in one row of the table in assess()."""

    def test_a_service_with_no_code_is_not_started(self) -> None:
        page = render([SQS_FACT])
        self.assertEqual(summary_row(page, "acm")[1:3], ["Not started", "n/a"])
        self.assertIn("- acm: No code yet.", page)

    def test_a_ledger_without_code_does_not_make_a_service_started(self) -> None:
        rows = {"tests/aws/services/sts/test_sts.py::T::a": row("pass", "pass")}
        self.assertEqual(summary_row(render([SQS_FACT], rows), "sts")[1], "Not started")

    def test_code_without_a_ledger_is_in_progress(self) -> None:
        page = render([SQS_ALL])
        self.assertEqual(status_of([SQS_ALL]), "In progress")
        self.assertEqual(summary_row(page, "sqs")[3:5], ["n/a", "no ledger"])
        self.assertIn("In progress because the service has no parity ledger.", page)

    def test_a_test_localstack_passes_that_gostack_fails_is_in_progress(self) -> None:
        rows = sqs_rows(("pass", "pass", ""), ("pass", "fail", ""))
        self.assertEqual(status_of([SQS_ALL], rows), "In progress")
        self.assertIn("1 of the 2 tests LocalStack passes do not pass on gostack", render([SQS_ALL], rows))

    def test_a_test_localstack_passes_that_gostack_did_not_run_is_in_progress(self) -> None:
        rows = sqs_rows(("pass", "pass", ""), ("pass", None, ""))
        self.assertEqual(status_of([SQS_ALL], rows), "In progress")

    def test_an_exclusion_that_waits_on_a_registered_service_is_in_progress(self) -> None:
        rows = sqs_rows(("pass", "pass", ""), ("excluded", "excluded", "depends-on:sts"))
        sts = fact("sts", ["GetCallerIdentity"], ["GetCallerIdentity"])
        page = render([SQS_ALL, sts], rows)
        self.assertEqual(summary_row(page, "sqs")[1], "In progress")
        self.assertIn("1 excluded test waits on sts, which gostack now serves", page)

    def test_an_exclusion_that_waits_on_a_service_gostack_does_not_serve_is_not_stale(self) -> None:
        rows = sqs_rows(("pass", "pass", ""), ("excluded", "excluded", "depends-on:lambda"))
        self.assertEqual(status_of([SQS_ALL], rows), "Complete")

    def test_an_exclusion_names_events_as_eventbridge_or_events(self) -> None:
        events = fact("events", ["PutEvents"], ["PutEvents"])
        for reason in ("depends-on:events", "depends-on:eventbridge"):
            rows = sqs_rows(("pass", "pass", ""), ("excluded", "excluded", reason))
            self.assertEqual(status_of([SQS_ALL, events], rows), "In progress", reason)

    def test_a_reason_with_text_after_the_service_still_names_it(self) -> None:
        self.assertEqual(features.depends_on(row("excluded", "excluded", "depends-on:kms (describe_key)")), "kms")

    def test_a_ledger_with_no_test_localstack_passes_is_in_progress(self) -> None:
        for rows in (sqs_rows(("fail", "pass", "")), sqs_rows(("excluded", "excluded", "x")), sqs_rows((None, "pass", ""))):
            self.assertEqual(status_of([SQS_ALL], rows), "In progress")

    def test_every_pass_with_operations_left_to_501_is_parity_done(self) -> None:
        self.assertEqual(status_of([SQS_FACT], sqs_rows(("pass", "pass", ""))), "Parity done")

    def test_every_pass_and_no_operation_left_to_501_is_complete(self) -> None:
        self.assertEqual(status_of([SQS_ALL], sqs_rows(("pass", "pass", ""))), "Complete")

    def test_a_service_whose_operations_are_unknown_is_not_complete(self) -> None:
        unknown = fact("sqs", ["CreateQueue"], None)
        self.assertEqual(status_of([unknown], sqs_rows(("pass", "pass", ""))), "Parity done")

    def test_rows_localstack_did_not_pass_or_excluded_count_toward_nothing(self) -> None:
        rows = sqs_rows(
            ("pass", "pass", ""),
            ("fail", "fail", ""),
            (None, "fail", ""),
            ("skip", "fail", ""),
            ("excluded", "excluded", "hard-skipped (x)"),
        )
        self.assertEqual(status_of([SQS_ALL], rows), "Complete")

    def test_the_legend_says_only_passing_tests_make_a_service_done_and_defines_complete_by_implementation(self) -> None:
        legend = render([SQS_FACT]).split("## Legend")[1].split("## Contents")[0]
        self.assertIn("Only tests LocalStack passes count toward Parity done and Complete.", legend)
        self.assertIn("never make a service either", legend)
        self.assertNotIn("count toward no status", legend)
        complete = next(line for line in legend.splitlines() if line.startswith("| Complete |"))
        self.assertIn("no operation is a declared stub", complete)
        self.assertNotIn("answers 501", complete)

    def test_the_excluded_tests_are_counted_and_linked_to_their_row(self) -> None:
        rows = sqs_rows(("pass", "pass", ""), ("excluded", "excluded", "x"), ("excluded", "excluded", "y"))
        cells = summary_row(render([SQS_ALL], rows), "sqs")
        self.assertEqual(cells[3:5], ["[2](parity.md#sqs)", "[row](parity.md#sqs)"])

    def test_engine_deferred_is_a_note_that_survives_the_service_being_built(self) -> None:
        opensearch = fact("opensearch", ["CreateDomain"], ["CreateDomain"])
        self.assertEqual(summary_row(render([SQS_FACT]), "opensearch")[1::4], ["Not started", "engine deferred"])
        built = render([opensearch])
        self.assertEqual(summary_row(built, "opensearch")[1], "In progress")
        self.assertEqual(summary_row(built, "opensearch")[5], "engine deferred")
        self.assertIn("The engine is deferred.", section(built, "opensearch"))

    def test_there_is_no_deferred_status(self) -> None:
        page = render([SQS_FACT])
        self.assertNotIn("Deferred", page)


class OperationsTest(unittest.TestCase):
    def test_an_operation_the_service_leaves_to_501_is_listed_as_not_implemented(self) -> None:
        page = render([SQS_FACT])
        implemented, missing = section(page, "sqs").split("**Not implemented, answered 501 (1)**")
        self.assertIn("`CreateQueue`", implemented)
        self.assertNotIn("`DeleteQueue`", implemented)
        self.assertIn("- Delete (1): `DeleteQueue`", missing)
        self.assertEqual(summary_row(page, "sqs")[2], "2 of 3")

    def test_operations_are_grouped_by_their_first_word(self) -> None:
        self.assertEqual(
            features.groups(["PutObject", "GetObject", "GetObjectAcl", "ListObjectsV2"]),
            [("Get", ["GetObject", "GetObjectAcl"]), ("List", ["ListObjectsV2"]), ("Put", ["PutObject"])],
        )

    def test_a_service_known_to_implement_no_operation_says_0_of_n(self) -> None:
        page = render([fact("sqs", ["CreateQueue", "ListQueues"], [])])
        self.assertEqual(summary_row(page, "sqs")[2], "0 of 2")
        self.assertIn("**Implemented (0)**", page)
        self.assertIn("**Not implemented, answered 501 (2)**", page)
        self.assertNotIn("unknown", page)

    def test_a_service_whose_implemented_operations_are_unknown_says_so(self) -> None:
        page = render([fact("sqs", ["CreateQueue", "ListQueues"], None)])
        self.assertEqual(summary_row(page, "sqs")[2], "unknown of 2")
        self.assertIn("The operations this service implements are unknown; its API has 2.", page)

    def test_persistence_states_only_what_the_registration_proves(self) -> None:
        page = render([SQS_FACT, fact("sts", ["GetCallerIdentity"], ["GetCallerIdentity"])])
        self.assertIn("Persistence: state saved when `PERSIST_SQS` is on.", section(page, "sqs"))
        self.assertIn("Persistence: no snapshotter is registered, so `PERSIST_STS` saves nothing.", section(page, "sts"))

    def test_a_persist_variable_uses_underscores(self) -> None:
        self.assertEqual(features.persist_variable("cognito-idp"), "PERSIST_COGNITO_IDP")

    def test_a_registered_service_missing_from_the_list_is_refused(self) -> None:
        with self.assertRaisesRegex(features.FeaturesError, "kms"):
            render([fact("kms", ["Encrypt"], ["Encrypt"])])


class NamingTest(unittest.TestCase):
    def test_a_directory_named_otherwise_takes_the_facts_and_rows_of_its_service(self) -> None:
        config = fact("config", ["PutConfigRule"], ["PutConfigRule"])
        rows = {"tests/aws/services/config/test_config.py::T::a": row("pass", "pass")}
        page = features.render("8b9a79f", [{"name": "configservice", "ledger": "config"}], {"config": config}, rows, '<a id="config"></a>', COMPAT)
        self.assertEqual(summary_row(page, "configservice")[1:5], ["Complete", "1 of 1", "[0](parity.md#config)", "[row](parity.md#config)"])

    def test_a_directory_of_resource_providers_only_takes_no_facts_and_says_so(self) -> None:
        page = render([SQS_FACT])
        self.assertEqual(summary_row(page, "ecr")[1:6:4], ["Not started", "CloudFormation resource providers only"])
        self.assertIn("- ecr: CloudFormation resource providers only.", page)
        self.assertNotIn("- ecr: No code yet", page)
        with self.assertRaisesRegex(features.FeaturesError, "ecr"):
            render([fact("ecr", ["X"], ["X"])])

    def test_lambda_is_listed_by_its_directory_name_and_takes_the_ledger_of_lambda(self) -> None:
        services = [{"name": "lambda_", "ledger": "lambda"}]
        lambda_fact = fact("lambda", ["Invoke"], ["Invoke"])
        rows = {"tests/aws/services/lambda_/test_l.py::T::a": row("pass", "pass")}
        page = features.render("8b9a79f", services, {"lambda": lambda_fact}, rows, '<a id="lambda"></a>', COMPAT)
        self.assertEqual(summary_row(page, "lambda_")[1:5], ["Complete", "1 of 1", "[0](parity.md#lambda)", "[row](parity.md#lambda)"])
        self.assertEqual(page.count("### lambda_\n"), 1)


class LinksTest(unittest.TestCase):
    def test_a_parity_link_resolves_to_the_row_anchor_of_parity_md(self) -> None:
        page = render([SQS_FACT], sqs_rows(("pass", "pass", "")))
        links = re.findall(r"\(parity\.md#([^)]+)\)", page)
        self.assertIn("sqs", links)
        for anchor in links:
            self.assertIn(anchor, features.anchors(PARITY))

    def test_a_parity_link_with_no_anchor_is_refused(self) -> None:
        with self.assertRaisesRegex(features.FeaturesError, "no anchor 'sqs'"):
            render([SQS_FACT], sqs_rows(("pass", "pass", "")), parity="# Parity\n")

    def test_anchors_are_headings_and_html_ids(self) -> None:
        self.assertEqual(features.anchors('# A b\n\n| <a id="x1"></a>x1 |\n<a name="y"></a>'), {"a-b", "x1", "y"})

    def test_anchors_follow_github(self) -> None:
        text = "# A, b (c)\n\n## Same\n\n## Same\n\n```\n# not a heading\n```\n\n### `Code` & more_\n"
        self.assertEqual(
            [anchor for _, _, anchor in features.slugs(text)],
            ["a-b-c", "same", "same-1", "code--more_"],
        )

    def test_compat_headings_that_name_a_service_come_before_the_rows_that_do(self) -> None:
        page = render([SQS_FACT])
        text = section(page, "sqs")
        self.assertIn("Headings of [compat.md](compat.md) that name sqs: [SQS](compat.md#sqs).", text)
        self.assertIn("[Request limits](compat.md#request-limits)", text)

    def test_a_row_under_a_heading_that_does_not_name_the_service_is_found(self) -> None:
        mentions = features.compat_mentions(SERVICES, COMPAT)
        self.assertEqual(mentions["sts"], [("Request limits", "request-limits", False)])
        self.assertEqual(mentions["sqs"], [("SQS", "sqs", True), ("Request limits", "request-limits", False)])

    def test_a_longer_compat_title_takes_its_heading_and_its_rows(self) -> None:
        mentions = features.compat_mentions(SERVICES, COMPAT)
        self.assertEqual(
            mentions["dynamodb"],
            [("DynamoDB", "dynamodb", True), ("Request limits", "request-limits", False)],
        )
        self.assertEqual(mentions["dynamodbstreams"], [("DynamoDB Streams", "dynamodb-streams", True)])

    def test_a_service_no_line_names_is_not_said_to_have_no_differences(self) -> None:
        page = render([fact("sts", ["X"], ["X"])], services=[{"name": "sts", "compat": ["Security Token Service"]}])
        self.assertIn("No heading or table row of [compat.md](compat.md) names sts.", page)
        self.assertNotIn("none recorded", page)

    def test_a_fenced_line_names_nothing(self) -> None:
        self.assertNotIn(("STS in a fence", "sts-in-a-fence", True), features.compat_mentions(SERVICES, COMPAT)["sts"])


class CommittedFilesTest(unittest.TestCase):
    def setUp(self) -> None:
        self.commit, self.services = features.load_services(features.SERVICES_FILE)

    def test_the_list_holds_the_39_directories_of_the_pinned_checkout(self) -> None:
        self.assertEqual(self.commit, ledger.pinned_localstack_sha()[: len(self.commit)])
        self.assertEqual([service["name"] for service in self.services], LOCALSTACK_SERVICE_DIRECTORIES)

    def test_every_directory_maps_to_a_ledger_test_directory_or_has_no_tests(self) -> None:
        for service in self.services:
            if service.get("providers_only") and "ledger" not in service:
                self.assertNotIn(features.lookup_name(service), ledger.TEST_DIRS, service["name"])
                continue
            self.assertIn(features.lookup_name(service), ledger.TEST_DIRS, f"{service['name']} has no ledger.TEST_DIRS key")

    def test_the_directories_whose_name_differs_from_their_tests_are_mapped(self) -> None:
        by_name = {service["name"]: service for service in self.services}
        self.assertEqual(features.lookup_name(by_name["configservice"]), "config")
        self.assertEqual(features.lookup_name(by_name["kinesisfirehose"]), "firehose")

    def test_the_four_directories_of_resource_providers_only_are_marked(self) -> None:
        marked = [service["name"] for service in self.services if service.get("providers_only")]
        self.assertEqual(marked, ["cdk", "certificatemanager", "ecr", "kinesisfirehose"])

    def test_every_service_an_exclusion_waits_on_is_listed(self) -> None:
        listed = {features.lookup_name(service) for service in self.services}
        targets = {features.depends_on(entry) for entry in ledger.load_ledger(ledger.LEDGER).values()} - {None}
        self.assertLessEqual(targets, listed)

    def test_every_service_of_the_ledger_is_listed(self) -> None:
        listed = {features.lookup_name(service) for service in self.services}
        self.assertLessEqual(set(ledger.split_by_service(ledger.load_ledger(ledger.LEDGER))), listed)

    def test_the_committed_page_links_only_to_anchors_that_exist(self) -> None:
        page = features.PAGE.read_text(encoding="utf-8")
        for document, path in (("parity", features.PARITY_DOC), ("compat", features.COMPAT_DOC)):
            known = features.anchors(path.read_text(encoding="utf-8"))
            links = re.findall(rf"\({document}\.md#([^)]+)\)", page)
            self.assertTrue(links, f"the page links to {document}.md")
            for anchor in links:
                self.assertIn(anchor, known, f"{document}.md has no anchor #{anchor}")


WARNING = "warning: docs/features.md was not refreshed; run make features"


class RefreshTest(unittest.TestCase):
    """The features refresh after a parity run (Makefile FEATURES_REFRESH)."""

    def dry_run(self, target: str, ci: str | None) -> str:
        env = {k: v for k, v in os.environ.items() if k != "CI"}
        if ci is not None:
            env["CI"] = ci
        done = subprocess.run(["make", "-n", target, "DRY_RUN="], cwd=ledger.ROOT, env=env, capture_output=True, text=True, check=False)
        return done.stdout

    def setUp(self) -> None:
        if shutil.which("make") is None:
            self.skipTest("make is not installed")

    def test_only_ci_true_skips_the_refresh(self) -> None:
        for target in ("parity", "parity-baseline", "parity-prune"):
            for ci in (None, "false", "1", ""):
                self.assertIn(WARNING, self.dry_run(target, ci), f"{target} CI={ci!r}")
            self.assertNotIn(WARNING, self.dry_run(target, "true"), f"{target} CI=true")

    def test_a_dry_run_of_parity_prune_does_not_refresh(self) -> None:
        env = {k: v for k, v in os.environ.items() if k != "CI"}
        done = subprocess.run(["make", "-n", "parity-prune", "DRY_RUN=1"], cwd=ledger.ROOT, env=env, capture_output=True, text=True, check=False)
        self.assertNotIn(WARNING, done.stdout)

    def test_a_failing_refresh_prints_one_warning_and_exits_0(self) -> None:
        refresh = next(line for line in self.dry_run("parity", None).splitlines() if WARNING in line)
        with tempfile.TemporaryDirectory() as tmp:
            fake_go = Path(tmp) / "go"
            fake_go.write_text("#!/bin/sh\nexit 1\n", encoding="utf-8")
            fake_go.chmod(0o755)
            env = {**os.environ, "PATH": f"{tmp}{os.pathsep}{os.environ['PATH']}"}
            done = subprocess.run(["sh", "-c", refresh], cwd=ledger.ROOT, env=env, capture_output=True, text=True, check=False)
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual((done.stdout + done.stderr).strip().splitlines(), [WARNING])


class ReadmeTest(unittest.TestCase):
    def test_the_readme_links_the_features_page(self) -> None:
        self.assertIn("(docs/features.md)", (ledger.ROOT / "README.md").read_text(encoding="utf-8"))


# The 39 directories under localstack-core/localstack/services at 8b9a79f.
LOCALSTACK_SERVICE_DIRECTORIES = [
    "acm",
    "apigateway",
    "cdk",
    "certificatemanager",
    "cloudformation",
    "cloudwatch",
    "configservice",
    "dynamodb",
    "dynamodbstreams",
    "ec2",
    "ecr",
    "es",
    "events",
    "firehose",
    "iam",
    "kinesis",
    "kinesisfirehose",
    "kms",
    "lambda_",
    "logs",
    "opensearch",
    "redshift",
    "resource_groups",
    "resourcegroupstaggingapi",
    "route53",
    "route53resolver",
    "s3",
    "s3control",
    "scheduler",
    "secretsmanager",
    "ses",
    "sns",
    "sqs",
    "ssm",
    "stepfunctions",
    "sts",
    "support",
    "swf",
    "transcribe",
]


if __name__ == "__main__":
    unittest.main()
