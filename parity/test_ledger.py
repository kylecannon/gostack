"""Tests for parity/ledger.py merge. Standard library only:

    python3 -m unittest discover -s parity -p 'test_ledger.py'
"""

from __future__ import annotations

import argparse
import contextlib
import io
import json
import tempfile
import unittest
from pathlib import Path

import ledger

STS = "tests/aws/services/sts/test_sts.py::TestSts"
SQS = "tests/aws/services/sqs/test_sqs.py::TestSqs"


def row(localstack: str | None, gostack: str | None, reason: str = "") -> dict:
    return {"localstack": localstack, "gostack": gostack, "reason": reason}


def junit_for(results: dict[str, str]) -> str:
    cases = []
    for nodeid, status in results.items():
        path, cls, name = nodeid.split("::")
        classname = path[: -len(".py")].replace("/", ".") + "." + cls
        body = {"pass": "", "fail": '<failure message="boom"/>', "skip": '<skipped message="why"/>'}[status]
        cases.append(f'<testcase classname="{classname}" name="{name}" file="{path}">{body}</testcase>')
    return f"<testsuites><testsuite>{''.join(cases)}</testsuite></testsuites>"


class MergeTest(unittest.TestCase):
    def setUp(self) -> None:
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)

    def merge(self, before: dict, column: str, results: dict[str, str], excluded: dict[str, str] | None = None) -> dict:
        (self.dir / "ledger.json").write_text(json.dumps(before), encoding="utf-8")
        (self.dir / "junit.xml").write_text(junit_for(results), encoding="utf-8")
        (self.dir / "excluded.json").write_text(json.dumps({"excluded": excluded or {}}), encoding="utf-8")
        args = argparse.Namespace(
            column=column,
            junit=self.dir / "junit.xml",
            excluded=self.dir / "excluded.json",
            ledger=self.dir / "ledger.json",
            docs=self.dir / "parity.md",
        )
        with contextlib.redirect_stdout(io.StringIO()):
            ledger.merge(args)
        return json.loads((self.dir / "ledger.json").read_text(encoding="utf-8"))

    def test_full_run_replaces_every_row(self) -> None:
        before = {
            f"{STS}::a": row("pass", "pass"),
            f"{STS}::b": row("pass", "pass"),
            f"{SQS}::c": row("pass", "fail"),
        }
        after = self.merge(before, "gostack", {f"{STS}::a": "fail", f"{STS}::b": "skip", f"{SQS}::c": "pass"})
        self.assertEqual(after[f"{STS}::a"]["gostack"], "fail")
        self.assertEqual(after[f"{STS}::b"]["gostack"], "skip")
        self.assertEqual(after[f"{SQS}::c"]["gostack"], "pass")
        self.assertEqual({n: after[n]["localstack"] for n in after}, {n: before[n]["localstack"] for n in before})

    def test_k_subset_updates_only_the_rows_it_matched(self) -> None:
        before = {
            f"{SQS}::purge_a": row("pass", "pass"),
            f"{SQS}::purge_b": row("pass", "fail"),
            f"{SQS}::other": row("pass", "pass"),
            f"{SQS}::skipped": row("skip", None, "why"),
        }
        after = self.merge(before, "gostack", {f"{SQS}::purge_a": "fail"})
        self.assertEqual(after[f"{SQS}::purge_a"]["gostack"], "fail")
        for nodeid in (f"{SQS}::purge_b", f"{SQS}::other", f"{SQS}::skipped"):
            self.assertEqual(after[nodeid], before[nodeid], nodeid)

    def test_single_service_run_leaves_other_services_untouched(self) -> None:
        before = {
            f"{STS}::a": row("pass", "pass"),
            f"{SQS}::c": row("pass", "fail", "kept"),
            f"{SQS}::d": row("fail", None, "kept"),
        }
        after = self.merge(before, "localstack", {f"{STS}::a": "pass"})
        self.assertEqual(after[f"{SQS}::c"], before[f"{SQS}::c"])
        self.assertEqual(after[f"{SQS}::d"], before[f"{SQS}::d"])

    def test_row_missing_from_a_run_of_its_own_service_keeps_both_columns(self) -> None:
        before = {f"{STS}::a": row("pass", "pass"), f"{STS}::b": row("pass", "pass")}
        after = self.merge(before, "gostack", {f"{STS}::a": "pass"})
        self.assertEqual(after[f"{STS}::b"], before[f"{STS}::b"])

    def test_excluded_nodeid_of_the_run_is_written_excluded(self) -> None:
        before = {f"{STS}::a": row("pass", "pass"), f"{STS}::iam": row("pass", "pass")}
        after = self.merge(before, "gostack", {f"{STS}::a": "pass"}, {f"{STS}::iam": "depends-on:iam"})
        self.assertEqual(after[f"{STS}::iam"], row("excluded", "excluded", "depends-on:iam"))


if __name__ == "__main__":
    unittest.main()
