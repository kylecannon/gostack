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
            demotions=self.dir / "demotions.yaml",
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

    def test_a_removed_row_that_passed_in_gostack_is_written_as_a_demotion(self) -> None:
        demotions = self.dir / "demotions.yaml"
        demotions.write_text(f'"{SQS}::older": flaky upstream\n', encoding="utf-8")
        self.prune(dry_run=False)
        sha = ledger.pinned_localstack_sha()
        # STS::gone passed in gostack; SQS::gone[a-b] never ran there, so it
        # needs no demotion. The entry already there is kept, and the file is
        # sorted.
        self.assertEqual(
            ledger.load_demotions(demotions),
            {f"{SQS}::older": "flaky upstream", f"{STS}::gone": f"gone upstream at {sha}"},
        )
        text = demotions.read_text(encoding="utf-8")
        self.assertTrue(text.startswith(ledger.DEMOTIONS_HEADER))
        self.assertEqual(text[len(ledger.DEMOTIONS_HEADER) :], f'"{SQS}::older": flaky upstream\n"{STS}::gone": gone upstream at {sha}\n')
        self.assertRegex(sha, r"^[0-9a-f]{40}$")

    def test_dry_run_prints_the_same_list_and_changes_nothing(self) -> None:
        dry_out, after = self.prune(dry_run=True)
        self.assertEqual(after, self.before)
        self.assertFalse((self.dir / "parity.md").exists())
        self.assertFalse((self.dir / "demotions.yaml").exists())
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

    def test_a_nodeid_listed_twice_in_one_file_is_an_error(self) -> None:
        # json.loads keeps the last one, so `pass` would hide the `fail`.
        (self.dir).mkdir()
        nodeid = json.dumps(f"{SQS}::b")
        (self.dir / "sqs.json").write_text(
            f'{{\n{nodeid}: {json.dumps(row("pass", "fail"))},\n{nodeid}: {json.dumps(row("pass", "pass"))}\n}}\n', encoding="utf-8"
        )
        with self.assertRaises(SystemExit) as raised:
            ledger.load_ledger(self.dir)
        self.assertIn(f"{SQS}::b listed twice", str(raised.exception))

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
            # The nine shapes PyYAML refuses that the parser used to accept
            # (SQ-177 item6.out).
            f'"{STS}::a": a\tb\n',
            f'\t# c\n"{STS}::a": r\n',
            f'"{STS}::a": .inf\n',
            f'"{STS}::a": .NaN\n',
            f'"{STS}::a": 0x1F\n',
            f'"{STS}::a": 0b101\n',
            f'"{STS}::a": =\n',
            f'"{STS}::a": <<\n',
            f'"{STS}::a": 2001-12-14 21:59:43.10 -5\n',
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
        demotions = yaml.safe_load(ledger.DEMOTIONS.read_text(encoding="utf-8")) or {}
        self.assertEqual(ledger.load_demotions(ledger.DEMOTIONS), {k: v.strip() for k, v in demotions.items()})


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
        self.demotions = self.repo / "parity" / "demotions.yaml"
        self.git("init", "-q", "-b", "main")
        self.rows = {f"{STS}::a": row("pass", "pass"), f"{SQS}::b": row("pass", "pass"), f"{SQS}::c": row("fail", None, "x")}

    def git(self, *argv: str) -> str:
        return git_in(self.repo, *argv)

    def commit(self, message: str) -> str:
        self.git("add", "-A")
        self.git("commit", "-qm", message)
        return self.git("rev-parse", "HEAD")

    def check(self, base: str) -> tuple[int, str]:
        args = argparse.Namespace(
            ledger=self.dir, exclusions=self.exclusions, demotions=self.demotions, base=base, collected=self.repo / "none.json"
        )
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

    def set_gostack(self, nodeid: str, status: str | None) -> None:
        rows = ledger.load_ledger(self.dir)
        rows[nodeid]["gostack"] = status
        ledger.write_ledger(self.dir, rows)

    def test_a_passing_gostack_row_that_stops_passing_in_any_way_fails_and_is_named(self) -> None:
        base = self.split_base()

        def remove_row() -> None:
            rows = ledger.load_ledger(self.dir)
            del rows[f"{SQS}::b"]
            ledger.write_ledger(self.dir, rows)

        def empty_file() -> None:
            (self.dir / "sqs.json").write_text("{}\n", encoding="utf-8")

        demotions = {status: (lambda s=status: self.set_gostack(f"{SQS}::b", s)) for status in ("fail", "skip", "xfail", "excluded", None)}
        demotions.update({"missing": remove_row, "emptied file": empty_file})
        for label, demote in demotions.items():
            with self.subTest(demotion=label):
                ledger.write_ledger(self.dir, self.rows)
                demote()
                code, out = self.check(base)
                self.assertEqual(code, 1, out)
                now = "missing" if label in ("missing", "emptied file") else label
                self.assertIn(f"gostack: {SQS}::b: pass -> {now}", out)

    def test_every_demoted_nodeid_is_named_not_only_the_first(self) -> None:
        base = self.split_base()
        self.set_gostack(f"{SQS}::b", "excluded")
        self.set_gostack(f"{STS}::a", None)
        code, out = self.check(base)
        self.assertEqual(code, 1, out)
        self.assertIn("2 regression(s)", out)
        self.assertIn(f"gostack: {SQS}::b: pass -> excluded", out)
        self.assertIn(f"gostack: {STS}::a: pass -> None", out)

    def test_a_demotion_listed_with_a_reason_passes_and_is_printed(self) -> None:
        base = self.split_base()
        self.set_gostack(f"{SQS}::b", "excluded")
        self.demotions.write_text(f'"{SQS}::b": needs the Docker socket since localstack/localstack#1\n', encoding="utf-8")
        code, out = self.check(base)
        self.assertEqual(code, 0, out)
        self.assertIn(f"gostack: {SQS}::b: pass -> excluded (needs the Docker socket since localstack/localstack#1)", out)

    def test_a_later_change_against_a_base_that_holds_the_demotion_passes(self) -> None:
        self.split_base()
        self.set_gostack(f"{SQS}::b", "fail")
        self.demotions.write_text(f'"{SQS}::b": reviewed\n', encoding="utf-8")
        demoted = self.commit("demote b")
        # The next change touches something else; the entry stays and is valid.
        rows = ledger.load_ledger(self.dir)
        rows[f"{STS}::new"] = row("pass", "pass")
        ledger.write_ledger(self.dir, rows)
        code, out = self.check(demoted)
        self.assertEqual(code, 0, out)

    def test_a_prune_that_removes_a_passing_row_passes_with_the_entry_prune_writes(self) -> None:
        base = self.split_base()
        listing = self.repo / "collected.txt"
        listing.write_text(f"{STS}::a\n{SQS}::c\n", encoding="utf-8")
        args = argparse.Namespace(
            ledger=self.dir, docs=self.repo / "parity.md", collected=listing, dry_run=False, localstack=None, cache=self.repo / "cache.json", demotions=self.demotions
        )
        with contextlib.redirect_stdout(io.StringIO()):
            ledger.prune(args)
        self.assertNotIn(f"{SQS}::b", ledger.load_ledger(self.dir))
        code, out = self.check(base)
        self.assertEqual(code, 0, out)
        self.assertIn(f"gostack: {SQS}::b: pass -> missing (gone upstream at ", out)
        # Without the entry the same removal fails.
        self.demotions.unlink()
        code, out = self.check(base)
        self.assertEqual(code, 1, out)
        self.assertIn(f"gostack: {SQS}::b: pass -> missing", out)

    def test_an_entry_for_an_excluded_failing_or_absent_row_may_stay(self) -> None:
        base = self.split_base()
        self.demotions.write_text(f'"{SQS}::c": still failing\n"{SQS}::gone": pruned\n', encoding="utf-8")
        code, out = self.check(base)
        self.assertEqual(code, 0, out)

    def test_a_demoted_test_that_passes_again_fails_until_its_entry_is_removed(self) -> None:
        self.split_base()
        self.set_gostack(f"{SQS}::b", "fail")
        self.demotions.write_text(f'"{SQS}::b": reviewed\n', encoding="utf-8")
        demoted = self.commit("demote b")
        self.set_gostack(f"{SQS}::b", "pass")
        code, out = self.check(demoted)
        self.assertEqual(code, 1, out)
        self.assertIn(f"demotions.yaml: {SQS}::b: passes in the gostack column again; remove the entry", out)
        self.demotions.write_text("# none\n", encoding="utf-8")
        self.assertEqual(self.check(demoted)[0], 0)

    def test_a_stale_entry_fails_even_against_a_base_with_no_ledger(self) -> None:
        (self.repo / "README").write_text("x\n", encoding="utf-8")
        base = self.commit("no ledger")
        ledger.write_ledger(self.dir, self.rows)
        self.demotions.write_text(f'"{SQS}::b": reviewed\n', encoding="utf-8")
        code, out = self.check(base)
        self.assertEqual(code, 1, out)
        self.assertIn("passes in the gostack column again", out)

    def test_a_duplicate_or_reasonless_demotion_entry_is_refused(self) -> None:
        base = self.split_base()
        for text in (f'"{SQS}::c": one\n"{SQS}::c": two\n', f'"{SQS}::c":\n', f'"{SQS}::c": \n', f'"{SQS}::c": ""\n'):
            with self.subTest(text=text):
                self.demotions.write_text(text, encoding="utf-8")
                self.assertEqual(self.check(base)[0], 1)

    def test_the_transition_base_with_the_single_ledger_is_gated_the_same_way(self) -> None:
        base = self.legacy_base()
        self.set_gostack(f"{SQS}::b", "excluded")
        code, out = self.check(base)
        self.assertEqual(code, 1, out)
        self.assertIn(f"gostack: {SQS}::b: pass -> excluded", out)
        self.demotions.write_text(f'"{SQS}::b": reviewed\n', encoding="utf-8")
        self.assertEqual(self.check(base)[0], 0)

    def test_the_localstack_column_fails_only_on_pass_to_fail(self) -> None:
        base = self.split_base()
        rows = ledger.load_ledger(self.dir)
        rows[f"{SQS}::b"]["localstack"] = "skip"
        ledger.write_ledger(self.dir, rows)
        code, out = self.check(base)
        self.assertEqual(code, 0, out)
        self.assertIn(f"localstack: {SQS}::b: pass -> skip", out)

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


WORKFLOW = ledger.ROOT / ".github" / "workflows" / "parity.yml"


def step_script(workflow: Path, name: str) -> str:
    """The `run: |` block of the step called name, dedented."""
    lines = workflow.read_text(encoding="utf-8").splitlines()
    start = lines.index(f"      - name: {name}")
    run = next(i for i in range(start, len(lines)) if lines[i].strip() == "run: |")
    indent = len(lines[run]) - len(lines[run].lstrip()) + 2
    body = []
    for line in lines[run + 1 :]:
        if line.strip() and len(line) - len(line.lstrip()) < indent:
            break
        body.append(line[indent:])
    return "\n".join(body) + "\n"


class MergeBaseTest(unittest.TestCase):
    """parity.yml compares a pull request with the merge base of its base and
    head, and selects from the paths changed since then. These run the
    workflow's own select step in a temporary repository: main moves on after
    the branch forks."""

    P = f"{SQS}::p"
    R = f"{SQS}::r"

    def setUp(self) -> None:
        without_git_env(self)
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.repo = Path(tmp.name).resolve()
        self.dir = self.repo / "parity" / "ledger"
        self.dir.mkdir(parents=True)
        (self.repo / "parity" / "ledger.py").write_bytes((ledger.ROOT / "parity" / "ledger.py").read_bytes())
        (self.repo / "specs").mkdir()
        (self.repo / "specs" / "SOURCES.lock").write_text(f"localstack_sha {'a' * 40}\n", encoding="utf-8")
        (self.repo / "internal" / "service" / "sqs").mkdir(parents=True)
        (self.repo / "internal" / "service" / "sqs" / "queue.go").write_text("package sqs\n", encoding="utf-8")
        # P passes at the fork. R fails at the fork; main makes it pass later.
        ledger.write_ledger(self.dir, {self.P: row("pass", "pass"), self.R: row("pass", "fail"), f"{STS}::a": row("pass", "pass")})
        self.git("init", "-q", "-b", "main")
        self.fork = self.commit("fork")
        self.git("checkout", "-qb", "branch")
        (self.repo / "internal" / "service" / "sqs" / "queue.go").write_text("package sqs\n\n// changed\n", encoding="utf-8")
        self.head = self.commit("branch change")

    def git(self, *argv: str) -> str:
        return git_in(self.repo, *argv)

    def commit(self, message: str) -> str:
        self.git("add", "-A")
        self.git("commit", "-qm", message)
        return self.git("rev-parse", "HEAD")

    def on_main(self, change) -> str:
        self.git("checkout", "-q", "main")
        change()
        tip = self.commit("main moves on")
        self.git("checkout", "-q", "branch")
        return tip

    def on_branch(self, change) -> None:
        change()
        self.head = self.commit("branch")

    def set_gostack(self, nodeid: str, status: str) -> None:
        rows = ledger.load_ledger(self.dir)
        rows[nodeid]["gostack"] = status
        ledger.write_ledger(self.dir, rows)

    def select(self, base_sha: str) -> tuple[int, dict[str, str], str]:
        """Run the workflow's select step as a pull_request event would."""
        output, summary = self.repo / "gh-output", self.repo / "gh-summary"
        env = {**os.environ, "EVENT": "pull_request", "BASE_SHA": base_sha, "HEAD_SHA": self.head, "REQUESTED": "", "GITHUB_OUTPUT": str(output), "GITHUB_STEP_SUMMARY": str(summary)}
        run = subprocess.run(
            ["bash", "-e", "-o", "pipefail", "-c", step_script(WORKFLOW, "Select the services")],
            cwd=self.repo,
            env=env,
            capture_output=True,
            text=True,
            check=False,
        )
        outputs = dict(line.split("=", 1) for line in output.read_text(encoding="utf-8").splitlines()) if output.exists() else {}
        return run.returncode, outputs, run.stderr

    def check(self, base: str) -> tuple[int, str]:
        args = argparse.Namespace(
            ledger=self.dir,
            exclusions=self.repo / "parity" / "exclusions",
            demotions=self.repo / "parity" / "demotions.yaml",
            base=base,
            collected=self.repo / "none.json",
        )
        out = io.StringIO()
        with mock.patch.object(ledger, "ROOT", self.repo), contextlib.redirect_stdout(out), contextlib.redirect_stderr(out):
            try:
                ledger.check(args)
            except SystemExit as exit_:
                return (exit_.code if isinstance(exit_.code, int) else 1), out.getvalue() + str(exit_.code)
        return 0, out.getvalue()

    def assert_selects_from_the_fork(self, tip: str) -> str:
        code, outputs, stderr = self.select(tip)
        self.assertEqual(code, 0, stderr)
        self.assertEqual(outputs["base"], self.fork)
        self.assertEqual(outputs["services"], "s3,sqs")  # sqs changed; s3 calls it
        return outputs["base"]

    def test_a_service_file_main_added_after_the_fork_does_not_fail_the_branch(self) -> None:
        def add_sns() -> None:
            ledger.write_ledger(self.dir, {**ledger.load_ledger(self.dir), "tests/aws/services/sns/test_sns.py::T::t": row("pass", "pass")})

        tip = self.on_main(add_sns)
        base = self.assert_selects_from_the_fork(tip)
        code, out = self.check(base)
        self.assertEqual(code, 0, out)
        # Against main's tip the branch would be blamed for a file it never had.
        code, out = self.check(tip)
        self.assertEqual(code, 1, out)
        self.assertIn("ledger/sns.json: deleted", out)

    def test_a_row_main_fixed_after_the_fork_is_compared_with_the_fork(self) -> None:
        # R: fail at the fork, pass on main, fail on the branch. The right result
        # is a pass: this head was built without main's fix, so its `fail` is
        # what the fork recorded. ci.yml checks the merge ref, which holds
        # main's `pass` for R, against main's tip.
        tip = self.on_main(lambda: self.set_gostack(self.R, "pass"))
        base = self.assert_selects_from_the_fork(tip)
        code, out = self.check(base)
        self.assertEqual(code, 0, out)
        code, out = self.check(tip)
        self.assertEqual(code, 1, out)
        self.assertIn(f"gostack: {self.R}: pass -> fail", out)

    def test_a_row_that_passed_at_the_fork_and_fails_on_the_branch_fails(self) -> None:
        tip = self.on_main(lambda: (self.repo / "README.md").write_text("x\n", encoding="utf-8"))
        self.on_branch(lambda: self.set_gostack(self.P, "fail"))
        base = self.assert_selects_from_the_fork(tip)
        code, out = self.check(base)
        self.assertEqual(code, 1, out)
        self.assertIn(f"gostack: {self.P}: pass -> fail", out)

    def test_no_merge_base_fails_the_select_step(self) -> None:
        self.git("checkout", "-q", "--orphan", "unrelated")
        unrelated = self.commit("unrelated history")
        self.git("checkout", "-q", "branch")
        for base_sha in (unrelated, "f" * 40):
            with self.subTest(base=base_sha):
                code, outputs, stderr = self.select(base_sha)
                self.assertNotEqual(code, 0)
                self.assertIn("no merge base", stderr)
                self.assertNotIn("services", outputs)

    def test_the_check_step_compares_with_the_selected_merge_base(self) -> None:
        script = step_script(WORKFLOW, "Fail on a regression")
        self.assertIn("make parity-check", script)
        text = WORKFLOW.read_text(encoding="utf-8")
        self.assertIn("PARITY_BASE: ${{ steps.select.outputs.base }}", text)
        self.assertNotIn("PARITY_BASE: ${{ github.event.pull_request.base.sha", text)


if __name__ == "__main__":
    unittest.main()
