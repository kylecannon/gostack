"""Tests for parity/ledger.py merge. Standard library only:

    python3 -m unittest discover -s parity -p 'test_*.py'
"""

from __future__ import annotations

import argparse
import contextlib
import io
import json
import os
import shutil
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

    def test_a_service_with_only_a_localstack_baseline_is_pruned_like_the_rest_without_a_demotion(self) -> None:
        # Its rows have no gostack result: collection is LocalStack's, and a
        # removed row that never passed in gostack needs no demotion.
        sns = "tests/aws/services/sns/test_sns.py::T"
        ledger.write_ledger(self.ledger_path, {**self.before, f"{sns}::kept": row("pass", None), f"{sns}::gone": row("pass", None)})
        self.listing.write_text(f"{STS}::kept\n{SQS}::kept\n{sns}::kept\n", encoding="utf-8")
        out, after = self.prune(dry_run=False)
        self.assertIn(f"removed {sns}::gone", out)
        self.assertIn(f"{sns}::kept", after)
        self.assertNotIn(f"{sns}::gone", after)
        self.assertNotIn("sns", (self.dir / "demotions.yaml").read_text(encoding="utf-8"))

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
        # check holds a service with gostack results to a package under
        # internal/service/: its suite is run only while one exists.
        for served in ("sqs", "sts"):
            package = self.repo / "internal" / "service" / served
            package.mkdir(parents=True)
            (package / f"{served}.go").write_text(f"package {served}\n", encoding="utf-8")
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

    def test_a_service_with_only_a_localstack_baseline_adds_and_stays_without_a_regression(self) -> None:
        # SQ-213 commits baselines for services gostack does not serve: rows
        # with a LocalStack column and a null gostack column. No gostack run
        # includes them, and the ratchet only protects a gostack pass.
        sns = "tests/aws/services/sns/test_sns.py::T::t"
        base = self.split_base()
        self.rows[sns] = row("pass", None)
        ledger.write_ledger(self.dir, self.rows)
        code, out = self.check(base)
        self.assertEqual(code, 0, out)
        base = self.commit("sns baseline")
        code, out = self.check(base)
        self.assertEqual(code, 0, out)
        self.flip(sns, "localstack")
        code, out = self.check(base)
        self.assertEqual(code, 1, out)
        self.assertIn(f"localstack: {sns}", out)

    def test_a_service_deleted_while_its_gostack_results_stay_is_refused(self) -> None:
        # Selection runs a suite only while internal/service/<svc>/ exists, so
        # the committed passes of a deleted service would stop being run.
        base = self.split_base()
        shutil.rmtree(self.repo / "internal" / "service" / "sqs")
        for target in (base, "HEAD"):
            code, out = self.check(target)
            self.assertEqual(code, 1, out)
            self.assertIn("sqs: 1 gostack result(s) in the ledger and no internal/service/sqs/ package", out)
            self.assertIn("remove the rows", out)
            self.assertNotIn("sts: ", out)

    def test_a_service_with_a_baseline_and_no_gostack_result_needs_no_package(self) -> None:
        sns = "tests/aws/services/sns/test_sns.py::T::t"
        excluded = "tests/aws/services/sns/test_sns.py::T::u"
        self.rows[sns] = row("pass", None)
        self.rows[excluded] = row("excluded", "excluded", "r")
        base = self.split_base()
        code, out = self.check(base)
        self.assertEqual(code, 0, out)

    def test_a_package_holding_only_a_test_file_is_not_a_service(self) -> None:
        base = self.split_base()
        package = self.repo / "internal" / "service" / "sqs"
        for name in list(package.iterdir()):
            name.unlink()
        (package / "queue_test.go").write_text("package sqs\n", encoding="utf-8")
        code, out = self.check(base)
        self.assertEqual(code, 1, out)
        self.assertIn("no internal/service/sqs/ package", out)

    def test_an_exclusion_that_depends_on_a_served_service_fails_and_is_named(self) -> None:
        # SQ-479: two dynamodb rows and one secretsmanager row stayed excluded
        # as depends-on:kms after kms was served.
        base = self.split_base()
        kms = self.repo / "internal" / "service" / "kms"
        kms.mkdir(parents=True)
        (kms / "kms.go").write_text("package kms\n", encoding="utf-8")
        (self.exclusions / "sqs.yaml").write_text(
            f'"{SQS}::d": depends-on:kms\n"{SQS}::e": depends-on:kms (describe_key)\n"{SQS}::f": depends-on:lambda\n',
            encoding="utf-8",
        )
        for target in (base, "HEAD"):
            code, out = self.check(target)
            self.assertEqual(code, 1, out)
            self.assertIn(f"parity/exclusions/sqs.yaml: {SQS}::d: depends-on:kms, but kms is served", out)
            self.assertIn(f"{SQS}::e: depends-on:kms (describe_key), but kms is served", out)
            self.assertNotIn(f"{SQS}::f", out)
            self.assertNotIn(f"{STS}::iam", out)

    def test_an_exclusion_that_depends_on_a_served_service_fails_without_a_committed_ledger(self) -> None:
        (self.repo / "README").write_text("x\n", encoding="utf-8")
        base = self.commit("no ledger")
        kms = self.repo / "internal" / "service" / "kms"
        kms.mkdir(parents=True)
        (kms / "kms.go").write_text("package kms\n", encoding="utf-8")
        (self.exclusions / "sqs.yaml").write_text(f'"{SQS}::d": depends-on:kms\n', encoding="utf-8")
        code, out = self.check(base)
        self.assertEqual(code, 1, out)
        self.assertIn("kms is served", out)

    def test_an_exclusion_that_depends_on_a_service_gostack_lacks_passes(self) -> None:
        base = self.split_base()
        (self.exclusions / "sqs.yaml").write_text(f'"{SQS}::d": depends-on:kms\n', encoding="utf-8")
        code, out = self.check(base)
        self.assertEqual(code, 0, out)

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

    def test_a_base_that_names_no_commit_fails(self) -> None:
        # SQ-181 N3: these printed "nothing to compare" and passed.
        self.split_base()
        self.set_gostack(f"{SQS}::b", "fail")
        for base in ("deadbeef" * 5, "f" * 40, "no-such-branch", "-q"):
            with self.subTest(base=base):
                code, out = self.check(base)
                self.assertEqual(code, 1, out)
                self.assertIn("is not a commit", out)

    def test_the_all_zero_base_fails_when_head_has_history(self) -> None:
        self.split_base()
        self.commit_change("second")
        self.set_gostack(f"{SQS}::b", "fail")
        code, out = self.check("0" * 40)
        self.assertEqual(code, 1, out)
        self.assertIn("all-zero sha", out)

    def test_the_all_zero_base_of_the_first_commit_has_nothing_to_compare(self) -> None:
        # The push that created the repository: HEAD is a root commit.
        self.split_base()
        code, out = self.check("0" * 40)
        self.assertEqual(code, 0, out)
        self.assertIn("no committed ledger before the first commit", out)

    def commit_change(self, message: str) -> str:
        (self.repo / "README").write_text(message, encoding="utf-8")
        return self.commit(message)

    def test_a_gone_upstream_entry_for_a_row_still_in_the_ledger_is_refused(self) -> None:
        # SQ-181 N1: prune deletes every row it writes such an entry for.
        base = self.split_base()
        self.set_gostack(f"{SQS}::b", "fail")
        self.demotions.write_text(f'"{SQS}::b": gone upstream at {"a" * 40}\n', encoding="utf-8")
        code, out = self.check(base)
        self.assertEqual(code, 1, out)
        self.assertIn(f"{SQS}::b: says gone upstream, but its row is still in the ledger", out)
        self.demotions.write_text(f'"{SQS}::b": fails since the receipt handle change\n', encoding="utf-8")
        self.assertEqual(self.check(base)[0], 0)


class UncommittedTest(unittest.TestCase):
    """`uncommitted` compares the gostack column a run wrote into the working
    tree with the ledger committed at --rev, for the services the run selected."""

    def setUp(self) -> None:
        without_git_env(self)
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.repo = Path(tmp.name).resolve()
        self.dir = self.repo / "parity" / "ledger"
        self.git("init", "-q", "-b", "main")
        ledger.write_ledger(self.dir, {f"{STS}::a": row("pass", "pass"), f"{SQS}::b": row("pass", "pass"), f"{SQS}::c": row("pass", "fail", "x")})
        self.git("add", "-A")
        self.git("commit", "-qm", "ledger")

    def git(self, *argv: str) -> str:
        return git_in(self.repo, *argv)

    def change(self, nodeid: str, column: str, status: str | None) -> None:
        rows = ledger.load_ledger(self.dir)
        if status == "missing":
            del rows[nodeid]
        else:
            rows.setdefault(nodeid, row("pass", None))[column] = status
        ledger.write_ledger(self.dir, rows)

    def run_uncommitted(self, services: str, collected: set[str] | None = None, excluded: dict[str, str] | None = None) -> tuple[int, str]:
        """Run uncommitted after a run that collected the given nodeids, by
        default every row the working tree holds for the services."""
        if collected is None:
            selected = set(services.split(","))
            collected = {n for n in ledger.load_ledger(self.dir) if ledger.service_of(n) in selected}
        junit, excluded_file = write_run(self.repo / "out", collected, excluded or {})
        args = argparse.Namespace(
            ledger=self.dir, services=services, rev="HEAD", junit=junit, excluded=excluded_file, demotions=self.repo / "parity" / "demotions.yaml"
        )
        out = io.StringIO()
        with mock.patch.object(ledger, "ROOT", self.repo), contextlib.redirect_stdout(out), contextlib.redirect_stderr(out):
            try:
                ledger.uncommitted(args)
            except SystemExit as exit_:
                return (exit_.code if isinstance(exit_.code, int) else 1), out.getvalue() + str(exit_.code)
        return 0, out.getvalue()

    def test_an_unchanged_ledger_passes(self) -> None:
        code, out = self.run_uncommitted("sqs,sts")
        self.assertEqual(code, 0, out)

    def test_a_committed_service_the_run_did_not_include_is_not_compared(self) -> None:
        # A service with only a LocalStack baseline: no gostack rows, and no
        # run of it. Neither its rows nor its absence from the junit report is
        # looked at while another service's run is.
        sns = "tests/aws/services/sns/test_sns.py::T::t"
        self.change(sns, "localstack", "pass")
        self.git("add", "-A")
        self.git("commit", "-qm", "sns baseline")
        code, out = self.run_uncommitted("sqs,sts")
        self.assertEqual(code, 0, out)
        self.assertIn("sqs, sts", out)
        code, out = self.run_uncommitted("")
        self.assertEqual(code, 0, out)
        self.assertIn("no service", out)

    def test_every_difference_of_a_selected_service_fails_and_is_named(self) -> None:
        cases = {
            "a fix left uncommitted": (f"{SQS}::c", "pass", "committed fail, observed pass"),
            "a regression left uncommitted": (f"{SQS}::b", "fail", "committed pass, observed fail"),
            "a row the run removed": (f"{SQS}::b", "missing", "committed pass, observed missing"),
            "a new row": (f"{SQS}::new", "pass", "committed missing, observed pass"),
            "null": (f"{SQS}::b", None, "committed pass, observed null"),
        }
        for label, (nodeid, status, message) in cases.items():
            with self.subTest(case=label):
                self.change(nodeid, "gostack", status)
                code, out = self.run_uncommitted("sqs")
                self.assertEqual(code, 1, out)
                self.assertIn(f"  {nodeid}: {message}", out)
                self.assertIn("Commit the ledger", out)
                self.git("reset", "-q", "--hard")
                self.git("clean", "-qfd")

    def test_a_service_file_not_committed_at_all_fails(self) -> None:
        self.change("tests/aws/services/sns/test_sns.py::T::t", "gostack", "pass")
        code, out = self.run_uncommitted("sns")
        self.assertEqual(code, 1, out)
        self.assertIn("tests/aws/services/sns/test_sns.py::T::t: committed missing, observed pass", out)

    def test_a_service_the_run_did_not_select_is_not_compared(self) -> None:
        self.change(f"{STS}::a", "gostack", "fail")
        self.assertEqual(self.run_uncommitted("sqs")[0], 0)
        self.assertEqual(self.run_uncommitted("sqs,sts")[0], 1)

    def test_the_localstack_column_is_not_compared(self) -> None:
        self.change(f"{SQS}::b", "localstack", "fail")
        self.assertEqual(self.run_uncommitted("sqs")[0], 0)

    def test_a_service_name_with_no_test_directory_is_refused(self) -> None:
        code, out = self.run_uncommitted("sqs,nope", collected=set())
        self.assertEqual(code, 1, out)
        self.assertIn("no LocalStack test directory known for service 'nope'", out)

    def commit_row(self, nodeid: str, entry: dict) -> None:
        ledger.write_ledger(self.dir, {**ledger.load_ledger(self.dir), nodeid: entry})
        self.git("add", "-A")
        self.git("commit", "-qm", "row")

    def test_a_committed_row_the_run_did_not_collect_fails_and_is_named(self) -> None:
        # SQ-199 item1, the reviewer's case: a made-up row committed as pass.
        # merge leaves it untouched, so the working tree still matches HEAD.
        made_up = f"{SQS}::made_up"
        self.commit_row(made_up, row("pass", "pass"))
        ran = {f"{SQS}::b", f"{SQS}::c", f"{STS}::a"}
        code, out = self.run_uncommitted("sqs", collected=ran)
        self.assertEqual(code, 1, out)
        self.assertIn(f"  {made_up}: committed pass, not collected by this run", out)
        self.assertNotIn(f"{SQS}::b", out)
        self.assertNotIn("Commit the ledger", out)
        for status in ("fail", None):
            with self.subTest(status=status):
                self.commit_row(made_up, row("pass", status))
                code, out = self.run_uncommitted("sqs", collected=ran)
                self.assertEqual(code, 1, out)
                self.assertIn(f"  {made_up}: committed {status or 'null'}, not collected by this run", out)
        # A service the run did not select is not its to observe.
        self.assertEqual(self.run_uncommitted("sts", collected=ran)[0], 0)

    def test_an_excluded_row_the_run_did_not_collect_passes(self) -> None:
        self.commit_row(f"{SQS}::gone[x]", row("excluded", "excluded", "requires_in_process"))
        self.assertEqual(self.run_uncommitted("sqs", collected={f"{SQS}::b", f"{SQS}::c"})[0], 0)

    def test_the_collected_set_is_the_junit_report_and_the_plugins_excluded_list(self) -> None:
        junit, excluded = write_run(self.repo / "out", {f"{SQS}::b"}, {f"{SQS}::d": "runner-docker"})
        self.assertEqual(ledger.run_collected(junit, excluded), {f"{SQS}::b", f"{SQS}::d"})
        # The plugin's list counts on its own, whatever the report holds.
        junit.write_text(junit_for({f"{SQS}::b": "pass"}), encoding="utf-8")
        self.assertEqual(ledger.run_collected(junit, excluded), {f"{SQS}::b", f"{SQS}::d"})

    def test_a_row_listed_in_demotions_the_run_did_not_collect_passes(self) -> None:
        nodeid = f"{SQS}::flaky"
        self.commit_row(nodeid, row("pass", "fail", "x"))
        (self.repo / "parity" / "demotions.yaml").write_text(f'"{nodeid}": fails since the receipt handle change\n', encoding="utf-8")
        self.assertEqual(self.run_uncommitted("sqs", collected={f"{SQS}::b", f"{SQS}::c"})[0], 0)

    def test_a_run_that_left_no_report_fails(self) -> None:
        junit, excluded = write_run(self.repo / "out", set(), {})
        for missing in (junit, excluded):
            with self.subTest(missing=missing.name):
                write_run(self.repo / "out", set(), {})
                missing.unlink()
                args = argparse.Namespace(ledger=self.dir, services="sqs", rev="HEAD", junit=junit, excluded=excluded, demotions=self.repo / "none.yaml")
                with mock.patch.object(ledger, "ROOT", self.repo), self.assertRaises(SystemExit) as raised:
                    ledger.uncommitted(args)
                self.assertIn(f"{missing} is missing", str(raised.exception))

    def test_the_command_line_requires_the_runs_report(self) -> None:
        for argv in (["uncommitted", "--services", "sqs"], ["uncommitted", "--services", "sqs", "--junit", "j.xml"]):
            with self.subTest(argv=argv), mock.patch("sys.argv", ["ledger.py", *argv]), contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as raised:
                ledger.main()
            self.assertEqual(raised.exception.code, 2)


def write_run(directory: Path, collected: set[str], excluded: dict[str, str]) -> tuple[Path, Path]:
    """The junit report and excluded.json of a run that collected these
    nodeids, as parity/run.sh leaves them. An excluded test is in the report
    as skipped, as the plugin's skip marker puts it there."""
    directory.mkdir(parents=True, exist_ok=True)
    junit, excluded_file = directory / "junit.xml", directory / "excluded.json"
    results = {nodeid: "pass" for nodeid in collected}
    results.update(dict.fromkeys(excluded, "skip"))
    junit.write_text(junit_for(results), encoding="utf-8")
    excluded_file.write_text(json.dumps({"collected": len(results), "excluded": excluded, "unmatched": []}), encoding="utf-8")
    return junit, excluded_file


class DocsTest(unittest.TestCase):
    """docs/parity.md is rendered from the ledger. The uncommitted gate
    compares only parity/ledger/, so a stale copy is caught here, by `make
    parity-check` on every pull request, without a parity run."""

    def rendered(self, rows: dict[str, dict]) -> str:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "parity.md"
            ledger.write_docs(rows, path)
            return path.read_text(encoding="utf-8")

    def test_the_committed_docs_are_what_the_committed_ledger_renders(self) -> None:
        self.assertEqual(
            ledger.DOCS.read_text(encoding="utf-8"),
            self.rendered(ledger.load_ledger(ledger.LEDGER)),
            "docs/parity.md is stale: run `python3 parity/ledger.py docs` and commit it",
        )

    def test_a_service_with_only_a_localstack_baseline_shows_not_run_and_leaves_the_total_alone(self) -> None:
        # Rows with no gostack result: the page lists the service with "not
        # run" and no parity share, and its LocalStack passes are not counted
        # against the parity of the services gostack ran.
        rows = {f"{STS}::a": row("pass", "pass"), f"{SQS}::b": row("pass", "pass"), f"{SQS}::c": row("pass", "fail", "x")}
        sns = {f"tests/aws/services/sns/test_sns.py::T::{n}": row("pass", None) for n in "xy"}
        without, with_sns = self.rendered(rows), self.rendered({**rows, **sns})
        self.assertIn('| <a id="sns"></a>sns | 2 | 0 | 0 | 2 | 0 | not run | n/a |', with_sns)
        total = {text: next(line for line in text.splitlines() if line.startswith("| **total**")) for text in (without, with_sns)}
        self.assertTrue(total[without].endswith("| 3 | 0 | 2 | 66.7% |"), total[without])
        # The LocalStack pass column of the total counts sns's two passes; the
        # share does not, so the total names what it is of.
        self.assertTrue(total[with_sns].endswith("| 5 | 0 | 2 | 66.7% (2 of 3) |"), total[with_sns])

    def test_a_ledger_whose_docs_were_not_rewritten_is_caught(self) -> None:
        rows = ledger.load_ledger(ledger.LEDGER)
        nodeid = min(n for n, entry in rows.items() if entry["gostack"] == "pass")
        changed = {**rows, nodeid: {**rows[nodeid], "gostack": "fail"}}
        self.assertNotEqual(ledger.DOCS.read_text(encoding="utf-8"), self.rendered(changed))


WORKFLOW = ledger.ROOT / ".github" / "workflows" / "parity.yml"
CI_WORKFLOW = ledger.ROOT / ".github" / "workflows" / "ci.yml"

# Stands in for `make parity-check` in the workflow steps: the Makefile's
# PARITY_BASE handling (an empty or unset base is HEAD) and its
# `ledger.py check --base`, without the unit tests, which would run this file.
MAKE_SHIM = """\
#!/bin/sh
[ "$1" = parity-check ] || { echo "make shim: unexpected target $1" >&2; exit 2; }
shift
for arg in "$@"; do
\tcase "$arg" in PARITY_BASE=*) PARITY_BASE=${arg#PARITY_BASE=} ;; esac
done
printf '%s\\n' "${PARITY_BASE:-HEAD}" >"$MAKE_LOG"
exec python3 parity/ledger.py check --base "${PARITY_BASE:-HEAD}"
"""


def step_script(workflow: Path, name: str) -> str:
    """The `run: |` block of the step called name, dedented."""
    lines = workflow.read_text(encoding="utf-8").splitlines()
    start = lines.index(f"      - name: {name}")
    run = next(i for i in range(start, len(lines)) if lines[i].strip().startswith("run:"))
    if lines[run].strip() != "run: |":
        return lines[run].strip()[len("run:") :].strip() + "\n"
    indent = len(lines[run]) - len(lines[run].lstrip()) + 2
    body = []
    for line in lines[run + 1 :]:
        if line.strip() and len(line) - len(line.lstrip()) < indent:
            break
        body.append(line[indent:])
    return "\n".join(body) + "\n"


class MergeRefTest(unittest.TestCase):
    """parity.yml tests GitHub's merge ref of a pull request, the head merged
    into its base, and compares it with HEAD^1. These run the workflow's own
    steps under `bash -e -o pipefail` in a temporary repository: main moves on
    after the branch forks, the merge commit is made the way GitHub makes it,
    and the run's results are written into the working tree."""

    P = f"{SQS}::p"
    R = f"{SQS}::r"

    def setUp(self) -> None:
        without_git_env(self)
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.repo = Path(tmp.name).resolve() / "repo"
        self.dir = self.repo / "parity" / "ledger"
        self.dir.mkdir(parents=True)
        (self.repo / "parity" / "ledger.py").write_bytes((ledger.ROOT / "parity" / "ledger.py").read_bytes())
        (self.repo / "specs").mkdir()
        (self.repo / "specs" / "SOURCES.lock").write_text(f"localstack_sha {'a' * 40}\n", encoding="utf-8")
        (self.repo / "internal" / "service" / "sqs").mkdir(parents=True)
        (self.repo / "internal" / "service" / "sqs" / "queue.go").write_text("package sqs\n", encoding="utf-8")
        # Only a service with a directory here is selected: s3 calls sqs and
        # sts is called by every test session. sns gets none on purpose.
        for built in ("s3", "sts"):
            (self.repo / "internal" / "service" / built).mkdir(parents=True)
            (self.repo / "internal" / "service" / built / f"{built}.go").write_text(f"package {built}\n", encoding="utf-8")
        bin_dir = Path(tmp.name).resolve() / "bin"
        bin_dir.mkdir()
        (bin_dir / "make").write_text(MAKE_SHIM, encoding="utf-8")
        (bin_dir / "make").chmod(0o755)
        self.path = f"{bin_dir}{os.pathsep}{os.environ.get('PATH', '')}"
        self.make_log = Path(tmp.name).resolve() / "make.log"
        # P passes at the fork. R fails at the fork; main makes it pass later.
        ledger.write_ledger(self.dir, {self.P: row("pass", "pass"), self.R: row("pass", "fail"), f"{STS}::a": row("pass", "pass")})
        self.git("init", "-q", "-b", "main")
        self.fork = self.commit("fork")
        self.git("checkout", "-qb", "branch")
        (self.repo / "internal" / "service" / "sqs" / "queue.go").write_text("package sqs\n\n// changed\n", encoding="utf-8")
        self.head = self.commit("branch change")
        self.tip = self.fork

    def git(self, *argv: str) -> str:
        return git_in(self.repo, *argv)

    def commit(self, message: str) -> str:
        self.git("add", "-A")
        self.git("commit", "-qm", message)
        return self.git("rev-parse", "HEAD")

    def on_main(self, change) -> None:
        self.git("checkout", "-q", "main")
        change()
        self.tip = self.commit("main moves on")
        self.git("checkout", "-q", "branch")

    def on_branch(self, change) -> None:
        change()
        self.head = self.commit("branch")

    def merge_ref(self) -> str:
        """Check out what GitHub's refs/pull/N/merge holds: a merge commit whose
        first parent is the base tip and second the pull request head."""
        self.git("checkout", "-q", "--detach", self.tip)
        self.git("merge", "-q", "--no-ff", "-m", "Merge branch into main", self.head)
        return self.git("rev-parse", "HEAD")

    def set_gostack(self, nodeid: str, status: str) -> None:
        rows = ledger.load_ledger(self.dir)
        rows[nodeid]["gostack"] = status
        ledger.write_ledger(self.dir, rows)

    def step(self, workflow: Path, name: str, **env: str) -> tuple[int, str]:
        run = subprocess.run(
            ["bash", "-e", "-o", "pipefail", "-c", step_script(workflow, name)],
            cwd=self.repo,
            env={**os.environ, "PATH": self.path, "MAKE_LOG": str(self.make_log), **env},
            capture_output=True,
            text=True,
            check=False,
        )
        return run.returncode, run.stdout + run.stderr

    def select(self, event: str = "pull_request", pr_head: str | None = None, mergeable: str = "", requested: str = "") -> tuple[int, dict[str, str], str]:
        """Run the workflow's select step as the event would."""
        output, summary = self.repo.parent / "gh-output", self.repo.parent / "gh-summary"
        output.unlink(missing_ok=True)
        code, text = self.step(
            WORKFLOW,
            "Select the services",
            EVENT=event,
            PR_HEAD=self.head if pr_head is None else pr_head,
            MERGEABLE=mergeable,
            REQUESTED=requested,
            GITHUB_OUTPUT=str(output),
            GITHUB_STEP_SUMMARY=str(summary),
        )
        outputs = dict(line.split("=", 1) for line in output.read_text(encoding="utf-8").splitlines()) if output.exists() else {}
        return code, outputs, text

    def regression(self, base: str, event: str = "pull_request") -> tuple[int, str]:
        return self.step(WORKFLOW, "Fail on a regression", EVENT=event, PARITY_BASE=base)

    def committed_check(self, services: str, collected: set[str] | None = None) -> tuple[int, str]:
        """Run the workflow's uncommitted step after a run that collected the
        given nodeids, by default every row the working tree holds for
        services, with the report where parity/run.sh leaves it."""
        if collected is None:
            selected = set(services.split(","))
            collected = {n for n in ledger.load_ledger(self.dir) if ledger.service_of(n) in selected}
        write_run(self.repo / "parity" / "out" / "gostack", collected, {})
        return self.step(WORKFLOW, "Fail on an uncommitted ledger", SERVICES=services)

    def assert_selects_from_the_base_tip(self) -> str:
        code, outputs, text = self.select()
        self.assertEqual(code, 0, text)
        self.assertEqual(outputs["base"], self.tip)
        self.assertEqual(self.git("rev-parse", "HEAD^1"), self.tip)
        # sqs changed: its queue-URL mount reaches every service's requests,
        # so every served service in the merged ledger runs, and s3, which calls
        # sqs. A service main gave ledger rows and no internal/service/ directory
        # (sns, in the tests that add it) is not run.
        self.assertEqual(outputs["services"], "s3,sqs,sts")
        return outputs["base"]

    def test_a_service_file_main_added_after_the_fork_passes(self) -> None:
        def add_sns() -> None:
            ledger.write_ledger(self.dir, {**ledger.load_ledger(self.dir), "tests/aws/services/sns/test_sns.py::T::t": row("pass", None)})

        self.on_main(add_sns)
        self.merge_ref()
        base = self.assert_selects_from_the_base_tip()
        self.assertTrue((self.dir / "sns.json").exists())
        code, out = self.regression(base)
        self.assertEqual(code, 0, out)
        self.assertEqual(self.make_log.read_text(encoding="utf-8").strip(), self.tip)
        code, out = self.committed_check("s3,sqs")
        self.assertEqual(code, 0, out)

    def test_a_row_fixed_on_main_and_broken_by_the_branch_fails(self) -> None:
        # SQ-181 S1, item3c: main fixes R after the fork; the branch, built
        # alone, still fails it. The merged tree carries main's fix in its
        # ledger and the branch's code, and its run sees R fail.
        self.on_main(lambda: self.set_gostack(self.R, "pass"))
        self.merge_ref()
        base = self.assert_selects_from_the_base_tip()
        self.set_gostack(self.R, "fail")  # what the run of the merged tree observed
        code, out = self.regression(base)
        self.assertEqual(code, 1, out)
        self.assertIn(f"gostack: {self.R}: pass -> fail", out)
        code, out = self.committed_check("s3,sqs")
        self.assertEqual(code, 1, out)
        self.assertIn(f"{self.R}: committed pass, observed fail", out)

    def test_a_row_that_passes_at_the_base_and_fails_in_the_merged_tree_fails(self) -> None:
        self.on_main(lambda: (self.repo / "README.md").write_text("x\n", encoding="utf-8"))
        self.on_branch(lambda: self.set_gostack(self.P, "fail"))  # committed honestly
        self.merge_ref()
        base = self.assert_selects_from_the_base_tip()
        self.set_gostack(self.P, "fail")
        code, out = self.regression(base)
        self.assertEqual(code, 1, out)
        self.assertIn(f"gostack: {self.P}: pass -> fail", out)
        # The committed ledger is what the run saw; only the ratchet fails.
        self.assertEqual(self.committed_check("s3,sqs")[0], 0)

    def test_a_fix_the_branch_did_not_commit_fails_until_it_does(self) -> None:
        self.merge_ref()
        self.assert_selects_from_the_base_tip()
        self.set_gostack(self.R, "pass")
        code, out = self.committed_check("s3,sqs")
        self.assertEqual(code, 1, out)
        self.assertIn(f"{self.R}: committed fail, observed pass", out)
        self.assertIn("Commit the ledger", out)
        # sts did not run, so its rows are not compared.
        self.set_gostack(self.R, "fail")
        self.set_gostack(f"{STS}::a", "fail")
        self.assertEqual(self.committed_check("s3,sqs")[0], 0)

    def test_a_passing_row_the_run_never_collected_fails_the_uncommitted_step(self) -> None:
        # SQ-199 item1, the reviewer's case: the branch commits a made-up
        # sqs row as pass. The run never collects it, merge leaves it as it
        # is, and before this the step said the ledger matched.
        made_up = "tests/aws/services/sqs/test_sqs.py::TestSqs::made_up"
        self.on_branch(lambda: ledger.write_ledger(self.dir, {**ledger.load_ledger(self.dir), made_up: row("pass", "pass")}))
        self.merge_ref()
        self.assert_selects_from_the_base_tip()
        ran = {self.P, self.R, f"{STS}::a"}
        code, out = self.committed_check("s3,sqs,sts", collected=ran)
        self.assertEqual(code, 1, out)
        self.assertIn(f"{made_up}: committed pass, not collected by this run", out)
        self.assertEqual(self.committed_check("s3,sqs,sts", collected=ran | {made_up})[0], 0)

    def test_a_head_that_is_not_githubs_merge_fails_the_select_step(self) -> None:
        self.on_main(lambda: (self.repo / "README.md").write_text("x\n", encoding="utf-8"))
        merged = self.merge_ref()
        cases = {
            "the head itself": (self.head, self.head, "is the pull request head"),
            "the base tip": (self.tip, self.head, "has 1 parent(s), not 2"),
            "a merge of another head": (merged, self.fork, f"merges {self.head}, not the pull request head"),
            "no head given": (merged, "", "is not a commit"),
            "a head this checkout lacks": (merged, "f" * 40, "is not a commit"),
        }
        for label, (checked_out, pr_head, message) in cases.items():
            with self.subTest(case=label):
                self.git("checkout", "-q", "--detach", checked_out)
                code, outputs, text = self.select(pr_head=pr_head)
                self.assertNotEqual(code, 0, text)
                self.assertIn(message, text)
                self.assertNotIn("services", outputs)

    def test_a_head_that_is_itself_a_merge_is_not_taken_for_the_merge_ref(self) -> None:
        # The branch merged main: its head has two parents too.
        self.on_main(lambda: (self.repo / "README.md").write_text("x\n", encoding="utf-8"))
        self.git("merge", "-q", "--no-ff", "-m", "Merge main into branch", "main")
        self.head = self.git("rev-parse", "HEAD")
        code, _, text = self.select()
        self.assertNotEqual(code, 0, text)
        self.assertIn("is the pull request head", text)
        self.merge_ref()
        self.assertEqual(self.select()[0], 0)

    def test_a_pull_request_github_reports_as_not_mergeable_fails(self) -> None:
        self.merge_ref()
        code, outputs, text = self.select(mergeable="false")
        self.assertNotEqual(code, 0)
        self.assertIn("not mergeable", text)
        self.assertNotIn("services", outputs)
        for mergeable in ("", "true"):  # null reads as empty: not computed yet
            with self.subTest(mergeable=mergeable):
                self.assertEqual(self.select(mergeable=mergeable)[0], 0)

    def test_a_manual_run_compares_with_the_dispatched_ref(self) -> None:
        # HEAD is the branch tip, not a merge, and that is fine here.
        code, outputs, text = self.select(event="workflow_dispatch", pr_head="", requested="sqs")
        self.assertEqual(code, 0, text)
        self.assertEqual(outputs["base"], "")
        self.assertEqual(outputs["services"], "sqs")
        self.set_gostack(self.P, "fail")
        code, out = self.regression("", event="workflow_dispatch")
        self.assertEqual(code, 1, out)
        self.assertEqual(self.make_log.read_text(encoding="utf-8").strip(), "HEAD")
        self.assertIn(f"gostack: {self.P}: pass -> fail", out)

    def test_a_pull_request_without_a_base_fails_the_regression_step(self) -> None:
        code, out = self.regression("")
        self.assertEqual(code, 1, out)
        self.assertIn("no base to compare the ledger with", out)

    def test_the_workflow_wires_the_base_and_the_committed_check(self) -> None:
        text = WORKFLOW.read_text(encoding="utf-8")
        self.assertIn("PARITY_BASE: ${{ steps.select.outputs.base }}", text)
        self.assertNotIn("pull_request.base.sha", text)
        self.assertNotIn("ref: ${{ github.event.pull_request.head.sha", text)
        self.assertIn("if: always() && github.event_name == 'pull_request' && steps.run.outcome == 'success'", text)
        self.assertIn("SERVICES: ${{ steps.select.outputs.services }}", text)
        script = step_script(WORKFLOW, "Fail on an uncommitted ledger")
        self.assertIn("--junit parity/out/gostack/junit.xml", script)
        self.assertIn("--excluded parity/out/gostack/excluded.json", script)

    def test_ci_compares_the_merge_ref_with_the_same_base(self) -> None:
        # ci.yml's parity-ledger job and parity.yml read the same HEAD^1.
        self.on_main(lambda: self.set_gostack(self.R, "pass"))
        self.merge_ref()
        base = self.assert_selects_from_the_base_tip()
        code, out = self.step(CI_WORKFLOW, "Compare the ledger with the base", EVENT="pull_request", PR_HEAD=self.head, BEFORE="")
        self.assertEqual(code, 0, out)
        self.assertEqual(self.make_log.read_text(encoding="utf-8").strip(), base)
        self.git("checkout", "-q", "--detach", self.head)
        code, out = self.step(CI_WORKFLOW, "Compare the ledger with the base", EVENT="pull_request", PR_HEAD=self.head, BEFORE="")
        self.assertNotEqual(code, 0, out)
        self.assertIn("is the pull request head", out)
        # A push compares with the commit main was at before it.
        self.git("checkout", "-q", "main")
        code, out = self.step(CI_WORKFLOW, "Compare the ledger with the base", EVENT="push", PR_HEAD="", BEFORE=self.fork)
        self.assertEqual(code, 0, out)
        self.assertEqual(self.make_log.read_text(encoding="utf-8").strip(), self.fork)


if __name__ == "__main__":
    unittest.main()
