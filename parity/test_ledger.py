"""Tests for parity/ledger.py merge. Standard library only:

    python3 -m unittest discover -s parity -p 'test_*.py'
"""

from __future__ import annotations

import argparse
import contextlib
import io
import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock

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
        ledger.write_ledger(self.dir / "ledger", before)
        (self.dir / "junit.xml").write_text(junit_for(results), encoding="utf-8")
        (self.dir / "excluded.json").write_text(json.dumps({"excluded": excluded or {}}), encoding="utf-8")
        args = argparse.Namespace(
            column=column,
            junit=self.dir / "junit.xml",
            excluded=self.dir / "excluded.json",
            ledger=self.dir / "ledger",
            docs=self.dir / "parity.md",
        )
        with contextlib.redirect_stdout(io.StringIO()):
            ledger.merge(args)
        return ledger.load_ledger(self.dir / "ledger")

    def test_a_nodeid_in_no_service_directory_is_an_error_and_nothing_is_written(self) -> None:
        before = {f"{STS}::a": row("pass", "pass")}
        ledger.write_ledger(self.dir / "ledger", before)
        written = (self.dir / "ledger" / "sts.json").read_bytes()
        for results, excluded in (
            ({f"{STS}::a": "pass", "tests/aws/services/zzz/test_z.py::T::t": "pass"}, None),
            ({f"{STS}::a": "pass"}, {"tests/other/test_x.py::T::t": "runner-docker"}),
        ):
            with self.subTest(results=results, excluded=excluded), self.assertRaises(SystemExit) as raised:
                self.merge(before, "gostack", results, excluded)
            self.assertIn("not under a known service's test directory", str(raised.exception))
            self.assertEqual((self.dir / "ledger" / "sts.json").read_bytes(), written)
            self.assertEqual(sorted(p.name for p in (self.dir / "ledger").iterdir()), ["sts.json"])
            self.assertFalse((self.dir / "parity.md").exists())

    def test_merge_writes_each_row_into_its_own_service_file(self) -> None:
        self.merge({}, "gostack", {f"{STS}::a": "pass", f"{SQS}::c": "fail"})
        self.assertEqual(sorted(p.name for p in (self.dir / "ledger").iterdir()), ["sqs.json", "sts.json"])
        sqs = json.loads((self.dir / "ledger" / "sqs.json").read_text(encoding="utf-8"))
        self.assertEqual(list(sqs), [f"{SQS}::c"])

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


class PruneTest(unittest.TestCase):
    def setUp(self) -> None:
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)
        self.ledger_path = self.dir / "ledger"
        self.before = {
            f"{STS}::kept": row("pass", "pass"),
            f"{STS}::gone": row("pass", "pass"),
            f"{SQS}::kept": row("pass", "fail"),
            f"{SQS}::gone[a-b]": row("skip", None, "why"),
        }
        ledger.write_ledger(self.ledger_path, self.before)
        # What `pytest --collect-only -q` prints: nodeids, a blank line, a summary.
        self.listing = self.dir / "collected.txt"
        self.listing.write_text(f"{STS}::kept\n{SQS}::kept\n\n2 tests collected in 0.10s\n", encoding="utf-8")

    def prune(self, dry_run: bool, collected: Path | None = None) -> tuple[str, dict]:
        args = argparse.Namespace(
            ledger=self.ledger_path,
            docs=self.dir / "parity.md",
            collected=collected or self.listing,
            dry_run=dry_run,
            localstack=None,
            cache=self.dir / "cache.json",
        )
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            ledger.prune(args)
        return out.getvalue(), ledger.load_ledger(self.ledger_path)

    def test_removes_rows_the_collection_lacks_and_prints_them(self) -> None:
        out, after = self.prune(dry_run=False)
        self.assertIn(f"removed {STS}::gone", out)
        self.assertIn(f"removed {SQS}::gone[a-b]", out)
        self.assertEqual(set(after), {f"{STS}::kept", f"{SQS}::kept"})
        self.assertTrue((self.dir / "parity.md").exists())

    def test_dry_run_prints_the_same_list_and_changes_nothing(self) -> None:
        dry_out, after = self.prune(dry_run=True)
        self.assertEqual(after, self.before)
        self.assertFalse((self.dir / "parity.md").exists())
        real_out, _ = self.prune(dry_run=False)
        self.assertEqual(dry_out.replace("would remove", "removed"), real_out)

    def test_nothing_stale_prints_an_empty_list(self) -> None:
        ledger.write_ledger(self.ledger_path, {k: v for k, v in self.before.items() if "gone" not in k})
        out, _ = self.prune(dry_run=True)
        self.assertEqual(out.splitlines(), ["prune: would remove 0 of 2 ledger row(s); 2 nodeids collected"])

    def test_empty_collection_refuses_to_remove_every_row(self) -> None:
        self.listing.write_text("no tests ran\n", encoding="utf-8")
        with self.assertRaises(SystemExit), contextlib.redirect_stdout(io.StringIO()):
            self.prune(dry_run=False)
        self.assertEqual(ledger.load_ledger(self.ledger_path), self.before)

    def test_service_with_no_collected_nodeid_refuses_to_prune(self) -> None:
        self.listing.write_text(f"{STS}::kept\n", encoding="utf-8")
        with self.assertRaises(SystemExit), contextlib.redirect_stdout(io.StringIO()):
            self.prune(dry_run=False)
        self.assertEqual(ledger.load_ledger(self.ledger_path), self.before)

    def test_stale_rows_reads_the_cached_collection(self) -> None:
        cache = self.dir / "cache.json"
        self.assertEqual(ledger.stale_rows(self.before, cache), [])  # no cache, no warning
        cache.write_text(json.dumps({"services": ["sts"], "nodeids": [f"{STS}::kept"]}), encoding="utf-8")
        self.assertEqual(ledger.stale_rows(self.before, cache), [f"{STS}::gone"])


def without_git_env(test: unittest.TestCase) -> None:
    """Clear every GIT_* variable for the test, for its own git calls and for
    ledger.py's. An inherited GIT_DIR once pointed a test's `git init` at the
    live repository."""
    patcher = mock.patch.dict(os.environ)
    patcher.start()
    test.addCleanup(patcher.stop)
    for name in [n for n in os.environ if n.startswith("GIT_")]:
        del os.environ[name]


def git_in(repo: Path, *argv: str) -> str:
    return subprocess.run(
        ["git", "-C", str(repo), "-c", "user.name=t", "-c", "user.email=t@example.com", *argv],
        capture_output=True,
        text=True,
        check=True,
    ).stdout.strip()


class LayoutTest(unittest.TestCase):
    """One file per service: the files' union is the ledger, written sorted."""

    def setUp(self) -> None:
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name) / "ledger"
        self.rows = {
            f"{SQS}::b": row("pass", "fail", "why"),
            f"{STS}::z": row("pass", "pass"),
            f"{SQS}::a[x]": row("excluded", "excluded", "requires_in_process"),
            f"{STS}::a": row(None, "skip", "skipped"),
            "tests/aws/services/lambda_/test_l.py::T::t": row("fail", None, "boom"),
        }

    def test_split_and_load_give_back_the_same_rows(self) -> None:
        ledger.write_ledger(self.dir, self.rows)
        self.assertEqual(sorted(p.name for p in self.dir.iterdir()), ["lambda.json", "sqs.json", "sts.json"])
        self.assertEqual(ledger.load_ledger(self.dir), self.rows)

    def test_a_second_write_of_the_same_rows_is_byte_identical_and_sorted(self) -> None:
        ledger.write_ledger(self.dir, self.rows)
        first = {p.name: p.read_bytes() for p in self.dir.iterdir()}
        ledger.write_ledger(self.dir, dict(reversed(list(self.rows.items()))))
        self.assertEqual({p.name: p.read_bytes() for p in self.dir.iterdir()}, first)
        keys = list(json.loads(first["sqs.json"]))
        self.assertEqual(keys, sorted(keys))

    def test_a_row_in_another_services_file_is_an_error(self) -> None:
        ledger.write_ledger(self.dir, self.rows)
        sts = json.loads((self.dir / "sts.json").read_text(encoding="utf-8"))
        sts[f"{SQS}::moved"] = row("pass", "pass")
        (self.dir / "sts.json").write_text(json.dumps(sts), encoding="utf-8")
        with self.assertRaises(SystemExit) as raised:
            ledger.load_ledger(self.dir)
        self.assertIn("belongs to sqs, not sts", str(raised.exception))

    def test_a_file_named_for_no_service_is_an_error(self) -> None:
        ledger.write_ledger(self.dir, self.rows)
        (self.dir / "nope.json").write_text("{}\n", encoding="utf-8")
        with self.assertRaises(SystemExit):
            ledger.load_ledger(self.dir)

    def test_the_old_single_file_beside_the_directory_is_an_error(self) -> None:
        ledger.write_ledger(self.dir, self.rows)
        (self.dir.parent / "ledger.json").write_text("{}\n", encoding="utf-8")
        with self.assertRaises(SystemExit):
            ledger.load_ledger(self.dir)

    def test_a_service_whose_rows_are_all_gone_keeps_an_empty_file(self) -> None:
        ledger.write_ledger(self.dir, self.rows)
        ledger.write_ledger(self.dir, {n: r for n, r in self.rows.items() if "/sts/" not in n})
        self.assertEqual((self.dir / "sts.json").read_text(encoding="utf-8"), "{}\n")

    def test_the_committed_ledger_is_in_the_layout(self) -> None:
        loaded = ledger.load_ledger(ledger.LEDGER)
        self.assertTrue(loaded)
        for service, path in ledger.service_files(ledger.LEDGER, ".json").items():
            self.assertEqual(path.read_text(encoding="utf-8"), ledger.format_ledger(json.loads(path.read_text(encoding="utf-8"))), service)


class ExclusionsTest(unittest.TestCase):
    def setUp(self) -> None:
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name) / "exclusions"
        self.dir.mkdir()

    def write(self, service: str, text: str) -> None:
        (self.dir / f"{service}.yaml").write_text(text, encoding="utf-8")

    def test_patterns_of_every_file_load_with_their_reasons(self) -> None:
        self.write("sts", f'# comment\n\n"{STS}::a": depends-on:iam\n')
        self.write("sqs", f'"tests/aws/services/sqs/resource_providers/*": depends-on:cloudformation\n"{SQS}::b": hard-skipped (localstack/localstack#5938)\n')
        self.assertEqual(
            ledger.load_exclusions(self.dir),
            {
                f"{STS}::a": "depends-on:iam",
                "tests/aws/services/sqs/resource_providers/*": "depends-on:cloudformation",
                f"{SQS}::b": "hard-skipped (localstack/localstack#5938)",
            },
        )

    def test_a_pattern_in_another_services_file_is_an_error(self) -> None:
        self.write("sts", f'"{SQS}::b": hard-skipped\n')
        with self.assertRaises(SystemExit) as raised:
            ledger.load_exclusions(self.dir)
        self.assertIn("matches sqs tests, not sts", str(raised.exception))

    def test_a_pattern_spanning_services_is_an_error(self) -> None:
        self.write("sts", '"tests/aws/services/*": runner-docker\n')
        with self.assertRaises(SystemExit):
            ledger.load_exclusions(self.dir)

    def test_a_line_yaml_would_read_differently_is_refused(self) -> None:
        for line in (
            f"{STS}::a: unquoted\n",
            f'"{STS}::a": reason # a comment\n',
            f'"{STS}::a": "quoted"\n',
            f'"{STS}::a": true\n',
            f'"{STS}::a": a: b\n',
            f'"{STS}::a":\n',
            f'"{STS}::a": x\n"{STS}::a": y\n',
        ):
            with self.subTest(line=line):
                self.write("sts", line)
                with self.assertRaises(SystemExit):
                    ledger.load_exclusions(self.dir)

    def test_the_old_single_file_beside_the_directory_is_an_error(self) -> None:
        (self.dir.parent / "exclusions.yaml").write_text("{}\n", encoding="utf-8")
        with self.assertRaises(SystemExit):
            ledger.load_exclusions(self.dir)

    def test_the_committed_files_read_as_yaml_reads_them(self) -> None:
        patterns = ledger.load_exclusions(ledger.EXCLUSIONS)
        self.assertTrue(patterns)
        try:
            import yaml
        except ImportError:
            self.skipTest("PyYAML is not installed; the line-shape test covers the parser")
        loaded: dict = {}
        for path in ledger.service_files(ledger.EXCLUSIONS, ".yaml").values():
            loaded.update(yaml.safe_load(path.read_text(encoding="utf-8")) or {})
        self.assertEqual(patterns, {k: v.strip() for k, v in loaded.items()})


class CheckAgainstBaseTest(unittest.TestCase):
    """`check --base` compares with the ledger committed at that revision, in
    either layout, so CI can pass the pull request's base instead of HEAD."""

    def setUp(self) -> None:
        without_git_env(self)
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.repo = Path(tmp.name).resolve()
        self.dir = self.repo / "parity" / "ledger"
        self.legacy = self.repo / "parity" / "ledger.json"
        self.exclusions = self.repo / "parity" / "exclusions"
        self.exclusions.mkdir(parents=True)
        (self.exclusions / "sts.yaml").write_text(f'"{STS}::iam": depends-on:iam\n', encoding="utf-8")
        self.git("init", "-q")
        self.rows = {f"{STS}::a": row("pass", "pass"), f"{SQS}::b": row("pass", "pass"), f"{SQS}::c": row("fail", None, "x")}

    def git(self, *argv: str) -> str:
        return git_in(self.repo, *argv)

    def commit(self, message: str) -> str:
        self.git("add", "-A")
        self.git("commit", "-qm", message)
        return self.git("rev-parse", "HEAD")

    def check(self, base: str) -> tuple[int, str]:
        args = argparse.Namespace(ledger=self.dir, exclusions=self.exclusions, base=base, collected=self.repo / "none.json")
        out = io.StringIO()
        with mock.patch.object(ledger, "ROOT", self.repo), contextlib.redirect_stdout(out), contextlib.redirect_stderr(out):
            try:
                ledger.check(args)
            except SystemExit as exit_:
                return int(exit_.code or 0) if not isinstance(exit_.code, str) else 1, out.getvalue() + str(exit_.code)
        return 0, out.getvalue()

    def legacy_base(self) -> str:
        self.legacy.write_text(json.dumps(self.rows), encoding="utf-8")
        base = self.commit("single ledger.json")
        self.legacy.unlink()
        ledger.write_ledger(self.dir, self.rows)
        return base

    def split_base(self) -> str:
        ledger.write_ledger(self.dir, self.rows)
        return self.commit("per-service files")

    def flip(self, nodeid: str, column: str = "gostack") -> None:
        rows = ledger.load_ledger(self.dir)
        rows[nodeid][column] = "fail"
        ledger.write_ledger(self.dir, rows)

    def test_a_row_that_passed_at_the_base_and_fails_now_exits_non_zero(self) -> None:
        base = self.split_base()
        self.flip(f"{STS}::a")
        self.commit("B")
        self.assertNotEqual(self.check(base)[0], 0)
        # Against HEAD, which already holds B, the same ledger is not a regression:
        # the comparison base is what makes the difference.
        self.assertEqual(self.check("HEAD")[0], 0)

    def test_a_base_with_the_single_ledger_is_read_row_by_row(self) -> None:
        base = self.legacy_base()
        code, out = self.check(base)
        self.assertEqual(code, 0, out)
        self.assertIn("ok against", out)
        self.flip(f"{SQS}::b")
        code, out = self.check(base)
        self.assertNotEqual(code, 0)
        self.assertIn(f"gostack: {SQS}::b: pass -> fail", out)

    def test_a_base_with_the_per_service_files_catches_the_same_flip(self) -> None:
        base = self.split_base()
        self.flip(f"{SQS}::b", "localstack")
        code, out = self.check(base)
        self.assertNotEqual(code, 0)
        self.assertIn(f"localstack: {SQS}::b: pass -> fail", out)

    def assert_deleting_sqs_fails(self, base: str) -> None:
        (self.dir / "sqs.json").unlink()
        code, out = self.check(base)
        self.assertNotEqual(code, 0)
        self.assertIn("ledger/sqs.json: deleted", out)

    def test_a_service_file_deleted_while_the_base_has_it_fails(self) -> None:
        self.assert_deleting_sqs_fails(self.split_base())

    def test_a_service_file_deleted_while_the_single_file_base_has_its_rows_fails(self) -> None:
        self.assert_deleting_sqs_fails(self.legacy_base())

    def test_a_row_removed_from_a_file_warns_and_passes_as_before(self) -> None:
        base = self.split_base()
        rows = ledger.load_ledger(self.dir)
        del rows[f"{SQS}::b"]
        ledger.write_ledger(self.dir, rows)
        code, out = self.check(base)
        self.assertEqual(code, 0, out)
        self.assertIn(f"gostack: {SQS}::b: pass -> missing", out)

    def test_a_base_with_neither_layout_has_nothing_to_compare(self) -> None:
        (self.repo / "README").write_text("x\n", encoding="utf-8")
        base = self.commit("no ledger")
        ledger.write_ledger(self.dir, self.rows)
        code, out = self.check(base)
        self.assertEqual(code, 0, out)
        self.assertIn(f"no committed ledger at {base}", out)

    def test_a_base_with_both_layouts_is_refused(self) -> None:
        ledger.write_ledger(self.dir, self.rows)
        self.legacy.write_text(json.dumps(self.rows), encoding="utf-8")
        base = self.commit("both")
        self.legacy.unlink()
        code, out = self.check(base)
        self.assertNotEqual(code, 0)
        self.assertIn("holds both", out)

    def test_a_misplaced_exclusion_fails_the_check(self) -> None:
        base = self.split_base()
        (self.exclusions / "sqs.yaml").write_text(f'"{STS}::iam2": depends-on:iam\n', encoding="utf-8")
        code, out = self.check(base)
        self.assertNotEqual(code, 0)
        self.assertIn("matches sts tests, not sqs", out)


if __name__ == "__main__":
    unittest.main()
