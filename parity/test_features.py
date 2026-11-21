"""Tests for parity/features.py. Standard library only:

    python3 -m unittest discover -s parity -p 'test_*.py'

The page itself is held to the code by `make features-check`, which needs the Go
facts; these tests hold the generator's rules and the committed page's links.
"""

from __future__ import annotations

import re
import unittest

import features
import ledger

PARITY = "# Parity\n\n## Per service\n\n### sqs\n\n### sts\n"
COMPAT = "# Differences\n\n## SQS\n\n### DynamoDB\n\n## DynamoDB Streams\n\n## Request limits\n"

SERVICES = [
    {"name": "acm"},
    {"name": "dynamodb", "compat": ["DynamoDB"]},
    {"name": "dynamodbstreams", "compat": ["DynamoDB Streams"]},
    {"name": "opensearch", "deferred": "The engine is deferred."},
    {"name": "sqs", "compat": ["SQS"]},
    {"name": "sts"},
]


def fact(name: str, operations: list[str], implemented: list[str] | None, persist: bool = False) -> dict:
    return {"name": name, "operations": operations, "implemented": implemented, "persist": persist}


def sqs_row(localstack: str, gostack: str) -> dict:
    return {"localstack": localstack, "gostack": gostack, "reason": ""}


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
    line = next(line for line in page.splitlines() if line.startswith(f"| {name} |"))
    return [cell.strip() for cell in line.strip("|").split("|")]


SQS_FACT = fact("sqs", ["CreateQueue", "DeleteQueue", "ListQueues"], ["CreateQueue", "ListQueues"], persist=True)
SQS_TEST = "tests/aws/services/sqs/test_sqs.py::TestSqs::test_a"


class StatusTest(unittest.TestCase):
    def test_a_service_with_no_code_is_not_started(self) -> None:
        page = render([SQS_FACT])
        self.assertEqual(summary_row(page, "acm")[1:3], ["Not started", "none"])
        self.assertIn("- acm: No code yet.", page)

    def test_an_engine_backed_service_with_no_code_is_deferred(self) -> None:
        page = render([SQS_FACT])
        self.assertEqual(summary_row(page, "opensearch")[1], "Deferred: engine-backed")
        self.assertIn("- opensearch: The engine is deferred.", page)

    def test_code_and_every_localstack_pass_passing_is_parity_done(self) -> None:
        rows = {SQS_TEST: sqs_row("pass", "pass"), SQS_TEST + "b": sqs_row("fail", "fail")}
        self.assertEqual(summary_row(render([SQS_FACT], rows), "sqs")[1], "Parity done")

    def test_code_with_a_localstack_pass_gostack_fails_is_in_progress(self) -> None:
        rows = {SQS_TEST: sqs_row("pass", "pass"), SQS_TEST + "b": sqs_row("pass", "fail")}
        self.assertEqual(summary_row(render([SQS_FACT], rows), "sqs")[1], "In progress")

    def test_code_without_a_ledger_is_in_progress_with_no_parity_link(self) -> None:
        row = summary_row(render([SQS_FACT]), "sqs")
        self.assertEqual((row[1], row[3]), ("In progress", "none"))

    def test_a_ledger_without_code_does_not_make_a_service_started(self) -> None:
        rows = {"tests/aws/services/sts/test_sts.py::T::a": sqs_row("pass", "pass")}
        self.assertEqual(summary_row(render([SQS_FACT], rows), "sts")[1], "Not started")


class OperationsTest(unittest.TestCase):
    def test_an_operation_the_service_leaves_to_501_is_listed_as_not_implemented(self) -> None:
        page = render([SQS_FACT])
        section = page.split("### sqs\n")[1].split("## Not started")[0]
        implemented, missing = section.split("**Not implemented, answered 501 (1)**")
        self.assertIn("`CreateQueue`", implemented)
        self.assertNotIn("`DeleteQueue`", implemented)
        self.assertIn("- Delete (1): `DeleteQueue`", missing)
        self.assertEqual(summary_row(page, "sqs")[2], "2 of 3")

    def test_operations_are_grouped_by_their_first_word(self) -> None:
        self.assertEqual(
            features.groups(["PutObject", "GetObject", "GetObjectAcl", "ListObjectsV2"]),
            [("Get", ["GetObject", "GetObjectAcl"]), ("List", ["ListObjectsV2"]), ("Put", ["PutObject"])],
        )

    def test_a_service_whose_implemented_operations_are_unknown_says_so(self) -> None:
        page = render([fact("sqs", ["CreateQueue", "ListQueues"], None)])
        self.assertEqual(summary_row(page, "sqs")[2], "unknown of 2")
        self.assertIn("The operations this service implements are unknown; its API has 2.", page)

    def test_persistence_is_stated_per_service(self) -> None:
        page = render([SQS_FACT, fact("sts", ["GetCallerIdentity"], ["GetCallerIdentity"])])
        self.assertIn("Persistence: `PERSIST_SQS`: yes.", page)
        self.assertIn("Persistence: `PERSIST_STS`: no.", page)

    def test_a_registered_service_missing_from_the_list_is_refused(self) -> None:
        with self.assertRaisesRegex(features.FeaturesError, "kms"):
            render([fact("kms", ["Encrypt"], ["Encrypt"])])


class LinksTest(unittest.TestCase):
    def test_a_parity_link_resolves_to_a_heading_of_parity_md(self) -> None:
        rows = {SQS_TEST: sqs_row("pass", "pass")}
        page = render([SQS_FACT], rows)
        self.assertIn("(parity.md#sqs)", page)
        anchors = {anchor for _, _, anchor in features.slugs(PARITY)}
        for anchor in re.findall(r"\(parity\.md#([^)]+)\)", page):
            self.assertIn(anchor, anchors)

    def test_a_parity_link_with_no_heading_is_refused(self) -> None:
        rows = {SQS_TEST: sqs_row("pass", "pass")}
        with self.assertRaisesRegex(features.FeaturesError, "no heading 'sqs'"):
            render([SQS_FACT], rows, parity="# Parity\n")

    def test_a_longer_compat_title_takes_its_heading(self) -> None:
        links = features.compat_links(SERVICES, COMPAT)
        self.assertEqual(links["dynamodb"], [("DynamoDB", "dynamodb")])
        self.assertEqual(links["dynamodbstreams"], [("DynamoDB Streams", "dynamodb-streams")])
        self.assertEqual(links["sqs"], [("SQS", "sqs")])
        self.assertEqual(links["sts"], [])

    def test_anchors_follow_github(self) -> None:
        text = "# A, b (c)\n\n## Same\n\n## Same\n\n```\n# not a heading\n```\n\n### `Code` & more_\n"
        self.assertEqual(
            [anchor for _, _, anchor in features.slugs(text)],
            ["a-b-c", "same", "same-1", "code--more_"],
        )


class CommittedFilesTest(unittest.TestCase):
    def setUp(self) -> None:
        self.commit, self.services = features.load_services(features.SERVICES_FILE)

    def test_the_list_holds_the_39_services_of_the_pinned_checkout(self) -> None:
        self.assertEqual(len(self.services), 39)
        self.assertEqual(self.commit, ledger.pinned_localstack_sha()[: len(self.commit)])

    def test_every_service_of_the_ledger_is_listed(self) -> None:
        listed = {service["name"] for service in self.services}
        self.assertLessEqual(set(ledger.split_by_service(ledger.load_ledger(ledger.LEDGER))), listed)

    def test_the_committed_page_links_only_to_headings_that_exist(self) -> None:
        page = features.PAGE.read_text(encoding="utf-8")
        for document, path in (("parity", features.PARITY_DOC), ("compat", features.COMPAT_DOC)):
            anchors = {anchor for _, _, anchor in features.slugs(path.read_text(encoding="utf-8"))}
            links = re.findall(rf"\({document}\.md#([^)]+)\)", page)
            self.assertTrue(links, f"the page links to {document}.md")
            for anchor in links:
                self.assertIn(anchor, anchors, f"{document}.md has no heading for #{anchor}")


if __name__ == "__main__":
    unittest.main()
