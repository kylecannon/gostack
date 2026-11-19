"""Tests for which services a change selects and which LocalStack test directory
each runs. Standard library only:

    python3 -m unittest discover -s parity -p 'test_*.py'
"""

from __future__ import annotations

import argparse
import contextlib
import io
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import ledger
from test_ledger import git_in, without_git_env

KNOWN = {"sqs", "sts"}
REGISTRATION = ledger.REGISTRATION_FILE

ADD_FOO = """\
diff --git a/cmd/gostack/services.go b/cmd/gostack/services.go
--- a/cmd/gostack/services.go
+++ b/cmd/gostack/services.go
@@ -8,0 +9 @@ import (
+	"github.com/kylecannon/gostack/internal/service/foo"
@@ -30,0 +32 @@ func registrations(cfg *config.Config, p *providers) []service.Registration {
+		foo.Registration(),
"""


class TestDirTest(unittest.TestCase):
    def test_lambda_runs_the_directory_with_the_underscore(self) -> None:
        self.assertEqual(ledger.test_dir("lambda"), "tests/aws/services/lambda_")

    def test_other_services_run_their_own_directory(self) -> None:
        self.assertEqual(ledger.test_dir("sqs"), "tests/aws/services/sqs")

    def test_unknown_service_is_refused(self) -> None:
        with self.assertRaises(SystemExit):
            ledger.test_dir("nope")

    def test_nodeid_of_lambda_maps_back_to_the_service_name(self) -> None:
        self.assertEqual(ledger.service_of("tests/aws/services/lambda_/test_lambda.py::t"), "lambda")
        self.assertEqual(ledger.service_of("tests/aws/services/sqs/test_sqs.py::t"), "sqs")

    def test_a_nodeid_in_no_service_directory_is_an_error(self) -> None:
        for nodeid in ("tests/other/test_x.py::t", "tests/aws/services/zzz/test_z.py::t"):
            with self.subTest(nodeid=nodeid), self.assertRaises(SystemExit):
                ledger.service_of(nodeid)

    def test_test_dirs_command_prints_one_path_per_service(self) -> None:
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            ledger.test_dirs(argparse.Namespace(services="lambda,sts"))
        self.assertEqual(out.getvalue().splitlines(), ["tests/aws/services/lambda_", "tests/aws/services/sts"])

    def test_every_service_has_a_distinct_directory(self) -> None:
        self.assertEqual(len(set(ledger.TEST_DIRS.values())), len(ledger.TEST_DIRS))


class AffectedServicesTest(unittest.TestCase):
    def test_change_under_a_service_directory_selects_that_service(self) -> None:
        got = ledger.affected_services(["internal/service/lambda/function.go"], KNOWN)
        self.assertEqual(got, {"lambda"})
        self.assertEqual(ledger.test_dir("lambda"), "tests/aws/services/lambda_")

    def test_two_service_directories_select_both(self) -> None:
        paths = ["internal/service/sqs/queue.go", "internal/service/sts/sts.go"]
        self.assertEqual(ledger.affected_services(paths, KNOWN), {"sqs", "sts"})

    def test_directory_with_no_localstack_tests_selects_nothing(self) -> None:
        self.assertEqual(ledger.affected_services(["internal/service/zzz/a.go"], KNOWN), set())

    def test_shared_path_selects_every_known_service(self) -> None:
        got = ledger.affected_services(["internal/gateway/gateway.go", "internal/service/lambda/a.go"], KNOWN)
        self.assertEqual(got, KNOWN | {"lambda"})

    def test_registration_file_is_shared_unless_the_additions_are_proven(self) -> None:
        paths = ["internal/service/foo/foo.go", REGISTRATION]
        self.assertEqual(ledger.affected_services(paths, KNOWN), KNOWN)
        self.assertEqual(ledger.affected_services(paths, KNOWN, registration_adds=True), set())

    def test_new_service_and_its_registration_select_only_that_service(self) -> None:
        paths = ["internal/service/lambda/lambda.go", REGISTRATION]
        self.assertEqual(ledger.affected_services(paths, KNOWN, registration_adds=True), {"lambda"})

    def test_registration_additions_do_not_excuse_another_shared_path(self) -> None:
        paths = ["internal/service/lambda/lambda.go", REGISTRATION, "internal/api/lambda/types.go"]
        self.assertEqual(ledger.affected_services(paths, KNOWN, registration_adds=True), KNOWN | {"lambda"})

    def test_docs_and_bench_select_nothing(self) -> None:
        self.assertEqual(ledger.affected_services(["docs/architecture.md", "bench/x.go"], KNOWN), set())


class OnlyAddsServiceLinesTest(unittest.TestCase):
    def test_added_lines_naming_the_service_pass(self) -> None:
        self.assertTrue(ledger.only_adds_service_lines(ADD_FOO, {"foo"}))

    def test_a_line_naming_another_service_fails(self) -> None:
        self.assertFalse(ledger.only_adds_service_lines(ADD_FOO, {"bar"}))

    def test_a_removed_line_fails(self) -> None:
        self.assertFalse(ledger.only_adds_service_lines(ADD_FOO + "-\t\tsts.Registration(),\n", {"foo", "sts"}))

    def test_an_added_line_naming_no_service_fails(self) -> None:
        self.assertFalse(ledger.only_adds_service_lines(ADD_FOO + "+\tcfg.Reload()\n", {"foo"}))

    def test_a_name_inside_a_longer_word_does_not_count(self) -> None:
        self.assertFalse(ledger.only_adds_service_lines("+\tfoobar.Registration(),\n", {"foo"}))

    def test_an_empty_diff_or_no_changed_service_fails(self) -> None:
        self.assertFalse(ledger.only_adds_service_lines("", {"foo"}))
        self.assertFalse(ledger.only_adds_service_lines(ADD_FOO, set()))


class ServicesCommandTest(unittest.TestCase):
    def setUp(self) -> None:
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.ledger = Path(tmp.name) / "ledger"
        entry = {"localstack": "pass", "gostack": "pass", "reason": ""}
        ledger.write_ledger(self.ledger, {f"tests/aws/services/{d}/test_x.py::t": entry for d in ("sqs", "sts")})

    def run_services(self, paths: list[str], diff: str | None, changed: bool = True, revisions: str | None = "a...b") -> str:
        args = argparse.Namespace(ledger=self.ledger, changed=changed, range=revisions)
        out = io.StringIO()
        with (
            contextlib.redirect_stdout(out),
            mock.patch("sys.stdin", io.StringIO("\n".join(paths))),
            mock.patch.object(ledger, "registration_diff", return_value=diff),
        ):
            ledger.services(args)
        return out.getvalue().strip()

    def test_without_changed_every_ledger_service_is_printed(self) -> None:
        self.assertEqual(self.run_services([], None, changed=False), "sqs,sts")

    def test_new_service_with_its_registration_line_selects_only_that_service(self) -> None:
        paths = ["internal/service/foo/foo.go", REGISTRATION]
        with mock.patch.dict(ledger.TEST_DIRS, {"foo": "foo"}):
            self.assertEqual(self.run_services(paths, ADD_FOO), "foo")

    def test_registration_edit_that_is_not_additions_selects_everything(self) -> None:
        paths = ["internal/service/foo/foo.go", REGISTRATION]
        with mock.patch.dict(ledger.TEST_DIRS, {"foo": "foo"}):
            self.assertEqual(self.run_services(paths, ADD_FOO + "-\told()\n"), "foo,sqs,sts")

    def test_registration_edit_without_a_range_selects_everything(self) -> None:
        self.assertEqual(self.run_services([REGISTRATION], ADD_FOO, revisions=None), "sqs,sts")

    def test_registration_edit_when_git_cannot_diff_selects_everything(self) -> None:
        self.assertEqual(self.run_services([REGISTRATION], None), "sqs,sts")

    def test_registration_edit_alone_has_no_changed_service_and_selects_everything(self) -> None:
        self.assertEqual(self.run_services([REGISTRATION], ADD_FOO), "sqs,sts")

    def test_a_change_under_lambda_selects_lambda_without_a_ledger_row(self) -> None:
        self.assertEqual(self.run_services(["internal/service/lambda/a.go"], None), "lambda")


class ParityFileSelectionTest(unittest.TestCase):
    """A service's own ledger and exclusions files select that service alone;
    everything else under parity/ stays shared."""

    def test_a_services_own_ledger_or_exclusions_file_selects_only_it(self) -> None:
        for path in ("parity/ledger/sqs.json", "parity/exclusions/sqs.yaml"):
            with self.subTest(path=path):
                self.assertEqual(ledger.affected_services([path], KNOWN), {"sqs"})
        self.assertEqual(ledger.affected_services(["parity/ledger/lambda.json"], KNOWN), {"lambda"})

    def test_the_rest_of_parity_is_shared(self) -> None:
        for path in (
            "parity/ledger.py",
            "parity/run.sh",
            "parity/exclusions/README.md",
            "parity/ledger/zzz.json",
            "parity/ledger/sqs.yaml",
            "parity/ledger/sub/sqs.json",
            "parity/ledger.json",
            "parity/exclusions.yaml",
        ):
            with self.subTest(path=path):
                self.assertEqual(ledger.affected_services([path], KNOWN), KNOWN)

    def test_a_shared_path_beside_a_services_files_selects_every_service(self) -> None:
        paths = ["parity/ledger/sqs.json", "internal/gateway/gateway.go"]
        self.assertEqual(ledger.affected_services(paths, KNOWN | {"s3"}), KNOWN | {"s3"})

    def test_a_new_service_branch_that_adds_only_its_own_files_selects_only_itself(self) -> None:
        without_git_env(self)
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp)
            target = repo / REGISTRATION
            target.parent.mkdir(parents=True)
            target.write_text("regs := []{\n\tsqs.Registration(),\n}\n", encoding="utf-8")
            entry = {"localstack": "pass", "gostack": "pass", "reason": ""}
            ledger.write_ledger(repo / "parity" / "ledger", {"tests/aws/services/sqs/test_x.py::t": entry})
            git_in(repo, "init", "-q")
            git_in(repo, "add", ".")
            git_in(repo, "commit", "-qm", "base")
            with mock.patch.dict(ledger.TEST_DIRS, {"foo": "foo"}), mock.patch.dict(ledger.SERVICE_OF_DIR, {"foo": "foo"}):
                (repo / "internal" / "service" / "foo").mkdir(parents=True)
                (repo / "internal" / "service" / "foo" / "foo.go").write_text("package foo\n", encoding="utf-8")
                target.write_text("regs := []{\n\tsqs.Registration(),\n\tfoo.Registration(),\n}\n", encoding="utf-8")
                ledger.write_ledger(
                    repo / "parity" / "ledger",
                    {"tests/aws/services/sqs/test_x.py::t": entry, "tests/aws/services/foo/test_f.py::t": entry},
                )
                (repo / "parity" / "exclusions").mkdir()
                (repo / "parity" / "exclusions" / "foo.yaml").write_text(
                    '"tests/aws/services/foo/test_f.py::u": depends-on:iam\n', encoding="utf-8"
                )
                git_in(repo, "add", ".")
                git_in(repo, "commit", "-qm", "add foo")
                changed = git_in(repo, "diff", "--name-only", "HEAD~1...HEAD")
                self.assertEqual(
                    sorted(changed.splitlines()),
                    [REGISTRATION, "internal/service/foo/foo.go", "parity/exclusions/foo.yaml", "parity/ledger/foo.json"],
                )
                args = argparse.Namespace(ledger=repo / "parity" / "ledger", changed=True, range="HEAD~1...HEAD")
                out = io.StringIO()
                with mock.patch.object(ledger, "ROOT", repo), mock.patch("sys.stdin", io.StringIO(changed)), contextlib.redirect_stdout(out):
                    ledger.services(args)
                self.assertEqual(out.getvalue().strip(), "foo")


class RegistrationDiffTest(unittest.TestCase):
    def setUp(self) -> None:
        without_git_env(self)

    def test_a_real_git_diff_of_added_lines_is_recognised(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp)

            def git(*argv: str) -> str:
                return git_in(repo, *argv)

            git("init", "-q")
            target = repo / REGISTRATION
            target.parent.mkdir(parents=True)
            target.write_text("regs := []{\n\tsqs.Registration(),\n}\n", encoding="utf-8")
            git("add", ".")
            git("commit", "-qm", "base")
            target.write_text("regs := []{\n\tsqs.Registration(),\n\tfoo.Registration(),\n}\n", encoding="utf-8")
            git("commit", "-qam", "add foo")
            with mock.patch.object(ledger, "ROOT", repo):
                diff = ledger.registration_diff("HEAD~1...HEAD")
            self.assertIsNotNone(diff)
            self.assertTrue(ledger.only_adds_service_lines(diff or "", {"foo"}))
            self.assertFalse(ledger.only_adds_service_lines(diff or "", {"sqs"}))


if __name__ == "__main__":
    unittest.main()
