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

KNOWN = {"kms", "sqs", "sts"}
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

SERVICES_GO = """\
package main

import (
	"github.com/kylecannon/gostack/internal/service/s3"
	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/kylecannon/gostack/internal/service/sts"
)

func registrations() []service.Registration {
	return []service.Registration{
		sqs.New().Registration(),
		sts.Registration(),
		s3.New().Registration(),
	}
}
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
        paths = ["internal/service/sts/sts.go", "internal/service/lambda/a.go"]
        self.assertEqual(ledger.affected_services(paths, KNOWN), {"sts", "lambda"})

    def test_a_change_to_sqs_also_selects_s3_which_calls_it_in_process(self) -> None:
        self.assertEqual(ledger.IN_PROCESS_CALLS["s3"], ("sqs",))
        self.assertEqual(ledger.affected_services(["internal/service/sqs/queue.go"], KNOWN), {"sqs", "s3"})
        self.assertEqual(ledger.affected_services(["internal/service/s3/object.go"], KNOWN), {"s3"})

    def test_a_services_own_ledger_file_selects_only_it_not_its_callers(self) -> None:
        self.assertEqual(ledger.affected_services(["parity/ledger/sqs.json"], KNOWN), {"sqs"})

    def test_paths_no_service_owns_select_every_known_service(self) -> None:
        for path in (
            "internal/gateway/gateway.go",
            "internal/service/service.go",
            "internal/service/common/util.go",  # a directory that names no service
            "internal/service/zzz/a.go",
            "api/openapi/gostack.yaml",  # go:embed'd into the binary
            "Makefile",
            ".dockerignore",
            "go.mod",
            "cmd/gostack/main.go",
            ".github/workflows/parity.yml",
            "internal/gateway/a b.go",
            "internal/gateway/é.go",
            'internal/gateway/a"b.go',
            "LICENSE",
            "internal/service/lambda",  # a file named like a service directory
        ):
            with self.subTest(path=path):
                self.assertEqual(ledger.affected_services([path], KNOWN), KNOWN)

    def test_a_shared_path_beside_a_service_selects_every_service_and_that_one(self) -> None:
        got = ledger.affected_services(["internal/gateway/gateway.go", "internal/service/lambda/a.go"], KNOWN)
        self.assertEqual(got, KNOWN | {"lambda"})

    def test_paths_that_cannot_change_the_image_select_nothing(self) -> None:
        for path in (
            "docs/architecture.md",
            "bench/x.go",
            "tools/go.mod",
            ".claude/.codebase-info/INDEX.md",
            "README.md",
            "AGENTS.md",
            "CLAUDE.md",
            ".github/workflows/ci.yml",
            ".github/workflows/bench.yml",
        ):
            with self.subTest(path=path):
                self.assertEqual(ledger.affected_services([path], KNOWN), set())

    def test_the_no_effect_list_matches_whole_names_only(self) -> None:
        for path in ("docsx/a.md", "README.md.go", "internal/docs/a.go", "cmd/README.md"):
            with self.subTest(path=path):
                self.assertEqual(ledger.affected_services([path], KNOWN), KNOWN)

    def test_registration_file_is_shared_unless_its_lines_are_attributed(self) -> None:
        paths = ["internal/service/sts/sts.go", REGISTRATION]
        self.assertEqual(ledger.affected_services(paths, KNOWN), KNOWN)
        self.assertEqual(ledger.affected_services(paths, KNOWN, registration={"sts"}), {"sts"})

    def test_registration_lines_naming_another_service_select_it_too(self) -> None:
        paths = ["internal/service/sts/sts.go", REGISTRATION]
        self.assertEqual(ledger.affected_services(paths, KNOWN, registration={"sqs"}), {"sts", "sqs", "s3"})

    def test_attributed_registration_does_not_excuse_another_shared_path(self) -> None:
        paths = ["internal/service/lambda/lambda.go", REGISTRATION, "internal/api/lambda/types.go"]
        self.assertEqual(ledger.affected_services(paths, KNOWN, registration={"lambda"}), KNOWN | {"lambda"})


class RegistrationServicesTest(unittest.TestCase):
    NAMES = {"foo", "sqs", "sts", "s3"}

    def test_added_lines_name_the_service_they_register(self) -> None:
        self.assertEqual(ledger.registration_services(ADD_FOO, self.NAMES), {"foo"})

    def test_removed_and_edited_lines_count_as_changes_too(self) -> None:
        diff = ADD_FOO + "-\t\tsts.Registration(),\n-\tsqs.New(a)\n+\tsqs.New(b)\n"
        self.assertEqual(ledger.registration_services(diff, self.NAMES), {"foo", "sts", "sqs"})

    def test_a_line_naming_no_service_cannot_be_attributed(self) -> None:
        self.assertIsNone(ledger.registration_services(ADD_FOO + "+\tcfg.Reload()\n", self.NAMES))
        self.assertIsNone(ledger.registration_services(ADD_FOO + "+\t}\n", self.NAMES))

    def test_a_service_named_only_in_a_comment_does_not_count(self) -> None:
        # SQ-177 item5 5.g2 and 5.g3: a global change carrying the service name
        # in a trailing comment.
        for line in ('+\tos.Setenv("GOSTACK_LOOSE", "1") // sts\n', "+\tgateway.SkipSignatureCheck = true // sqs\n"):
            with self.subTest(line=line):
                self.assertIsNone(ledger.registration_services(line, self.NAMES))

    def test_comment_and_blank_line_changes_name_nothing_and_change_nothing(self) -> None:
        self.assertEqual(ledger.registration_services("+\t// registrations, one per line\n+\n-\t// old\n", self.NAMES), set())

    def test_a_name_inside_a_longer_word_does_not_count(self) -> None:
        self.assertIsNone(ledger.registration_services("+\tfoobar.Registration(),\n", self.NAMES))
        self.assertIsNone(ledger.registration_services("+\tp.s3Provider = nil\n", self.NAMES))

    def test_a_name_that_is_not_a_service_directory_does_not_count(self) -> None:
        self.assertIsNone(ledger.registration_services(ADD_FOO, {"sqs"}))
        self.assertIsNone(ledger.registration_services(ADD_FOO, set()))

    def test_a_diff_with_no_changed_line_cannot_be_attributed(self) -> None:
        self.assertIsNone(ledger.registration_services("", self.NAMES))


class ServicesCommandTest(unittest.TestCase):
    def setUp(self) -> None:
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.ledger = Path(tmp.name) / "ledger"
        entry = {"localstack": "pass", "gostack": "pass", "reason": ""}
        ledger.write_ledger(self.ledger, {f"tests/aws/services/{d}/test_x.py::t": entry for d in ("kms", "sqs", "sts")})

    def run_services(self, paths: list[str], diff: str | None, changed: bool = True, revisions: str | None = "a...b") -> str:
        args = argparse.Namespace(ledger=self.ledger, changed=changed, range=revisions)
        out = io.StringIO()
        with (
            contextlib.redirect_stdout(out),
            mock.patch("sys.stdin", io.StringIO("".join(p + "\0" for p in paths))),
            mock.patch.object(ledger, "registration_diff", return_value=diff),
            mock.patch.object(ledger, "service_dirs", return_value={"foo", "sqs", "sts", "s3"}),
        ):
            ledger.services(args)
        return out.getvalue().strip()

    def test_without_changed_every_ledger_service_is_printed(self) -> None:
        self.assertEqual(self.run_services([], None, changed=False), "kms,sqs,sts")

    def test_no_changed_path_selects_nothing(self) -> None:
        self.assertEqual(self.run_services([], None), "")

    def test_new_service_with_its_registration_line_selects_only_that_service(self) -> None:
        paths = ["internal/service/foo/foo.go", REGISTRATION]
        with mock.patch.dict(ledger.TEST_DIRS, {"foo": "foo"}):
            self.assertEqual(self.run_services(paths, ADD_FOO), "foo")

    def test_registration_line_for_another_service_beside_one_services_change_selects_both(self) -> None:
        # S2: before, the sts change excused any added line.
        self.assertEqual(self.run_services(["internal/service/sts/sts.go", REGISTRATION], "+\tsqs.New().Registration(),\n"), "s3,sqs,sts")

    def test_registration_edit_naming_no_service_selects_everything(self) -> None:
        paths = ["internal/service/foo/foo.go", REGISTRATION]
        with mock.patch.dict(ledger.TEST_DIRS, {"foo": "foo"}):
            self.assertEqual(self.run_services(paths, ADD_FOO + "-\told()\n"), "foo,kms,sqs,sts")

    def test_registration_edit_without_a_range_selects_everything(self) -> None:
        self.assertEqual(self.run_services([REGISTRATION], ADD_FOO, revisions=None), "kms,sqs,sts")

    def test_registration_edit_when_git_cannot_diff_selects_everything(self) -> None:
        self.assertEqual(self.run_services([REGISTRATION], None), "kms,sqs,sts")

    def test_a_change_under_lambda_selects_lambda_without_a_ledger_row(self) -> None:
        self.assertEqual(self.run_services(["internal/service/lambda/a.go"], None), "lambda")

    def test_newline_separated_input_is_refused(self) -> None:
        args = argparse.Namespace(ledger=self.ledger, changed=True, range=None)
        with mock.patch("sys.stdin", io.StringIO("internal/service/sqs/a.go\n")), self.assertRaises(SystemExit) as raised:
            ledger.services(args)
        self.assertIn("NUL-separated", str(raised.exception))

    def test_a_name_with_a_newline_is_one_path(self) -> None:
        self.assertEqual(self.run_services(["internal/service/sts/a\nb.go"], None), "sts")


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
            "parity/demotions.yaml",
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


class RealDiffSelectionTest(unittest.TestCase):
    """The selection as parity.yml feeds it: `git diff --name-only -z
    --no-renames` from the merge base, in a temporary repository."""

    def setUp(self) -> None:
        without_git_env(self)
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.repo = Path(tmp.name).resolve()
        self.write(REGISTRATION, SERVICES_GO)
        for service in ("s3", "sqs", "sts"):
            self.write(f"internal/service/{service}/{service}.go", f"package {service}\n")
            self.write(f"parity/exclusions/{service}.yaml", "# none\n")
        self.write("internal/gateway/gateway.go", "package gateway\n")
        entry = {"localstack": "pass", "gostack": "pass", "reason": ""}
        ledger.write_ledger(self.repo / "parity" / "ledger", {f"tests/aws/services/{s}/test_x.py::t": entry for s in ("kms", "s3", "sqs", "sts")})
        self.git("init", "-q")
        self.base = self.commit("base")

    def git(self, *argv: str) -> str:
        return git_in(self.repo, *argv)

    def write(self, relative: str, text: str) -> None:
        path = self.repo / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")

    def commit(self, message: str) -> str:
        self.git("add", "-A")
        self.git("commit", "-qm", message)
        return self.git("rev-parse", "HEAD")

    def add_to_registration(self, text: str) -> None:
        self.write(REGISTRATION, (self.repo / REGISTRATION).read_text(encoding="utf-8") + text)

    def selected(self) -> str:
        head = self.commit("change")
        names = git_in(self.repo, "-c", "core.quotePath=true", "diff", "--name-only", "-z", "--no-renames", self.base, head)
        args = argparse.Namespace(ledger=self.repo / "parity" / "ledger", changed=True, range=f"{self.base}...{head}")
        out = io.StringIO()
        with mock.patch.object(ledger, "ROOT", self.repo), mock.patch("sys.stdin", io.StringIO(names + "\0")), contextlib.redirect_stdout(out):
            ledger.services(args)
        return out.getvalue().strip()

    def test_a_change_under_sqs_selects_sqs_and_s3(self) -> None:
        self.write("internal/service/sqs/queue.go", "package sqs\n")
        self.assertEqual(self.selected(), "s3,sqs")

    def test_a_rename_from_a_shared_path_into_a_service_selects_every_service(self) -> None:
        # SQ-177 item5 5.r: without --no-renames only the new path was listed.
        self.git("mv", "internal/gateway/gateway.go", "internal/service/sts/gateway.go")
        self.assertEqual(self.selected(), "kms,s3,sqs,sts")

    def test_a_quoted_shared_name_selects_every_service(self) -> None:
        # SQ-177 item5 5.u and 5.q: git quoted these names, and the quoted path
        # matched nothing.
        for name in ("é.go", 'a"b.go'):
            with self.subTest(name=name):
                self.write(f"internal/gateway/{name}", "package gateway\n")
                self.assertEqual(self.selected(), "kms,s3,sqs,sts")
                self.git("reset", "-q", "--hard", self.base)

    def test_a_global_line_with_a_service_comment_beside_a_service_file_selects_every_service(self) -> None:
        # SQ-177 item5 5.g3: only sts.yaml touched, plus a global line ending in // sts.
        self.write("parity/exclusions/sts.yaml", "# none\n# touch\n")
        self.add_to_registration('\nfunc init() { os.Setenv("GOSTACK_LOOSE", "1") } // sts\n')
        self.assertEqual(self.selected(), "kms,s3,sqs,sts")

    def test_an_import_line_naming_the_changed_service_selects_only_it_and_its_callers(self) -> None:
        # SQ-177 item5 5.g1.
        self.write("internal/service/sts/new.go", "package sts\n")
        text = (self.repo / REGISTRATION).read_text(encoding="utf-8")
        self.write(REGISTRATION, text.replace("import (\n", 'import (\n\t_ "github.com/kylecannon/gostack/internal/service/sts"\n'))
        self.assertEqual(self.selected(), "sts")

    def test_a_registration_line_for_another_service_selects_it_as_well(self) -> None:
        self.write("internal/service/sts/new.go", "package sts\n")
        text = (self.repo / REGISTRATION).read_text(encoding="utf-8")
        self.write(REGISTRATION, text.replace("\t\tsqs.New().Registration(),\n", "\t\tsqs.New(true).Registration(),\n"))
        self.assertEqual(self.selected(), "s3,sqs,sts")

    def test_a_new_service_branch_that_adds_only_its_own_files_selects_only_itself(self) -> None:
        # SQ-177 item5 5.n.
        self.write("internal/service/sns/sns.go", "package sns\n")
        self.write("parity/ledger/sns.json", "{}\n")
        self.write("parity/exclusions/sns.yaml", "# sns\n")
        text = (self.repo / REGISTRATION).read_text(encoding="utf-8")
        text = text.replace("\t\"github.com/kylecannon/gostack/internal/service/sqs\"\n", '\t"github.com/kylecannon/gostack/internal/service/sns"\n\t"github.com/kylecannon/gostack/internal/service/sqs"\n')
        text = text.replace("\t\ts3.New().Registration(),\n", "\t\ts3.New().Registration(),\n\t\tsns.New().Registration(),\n")
        self.write(REGISTRATION, text)
        self.assertEqual(self.selected(), "sns")

    def test_a_makefile_or_api_change_selects_every_service(self) -> None:
        for path in ("Makefile", "api/openapi/gostack.yaml", "internal/service/common/util.go"):
            with self.subTest(path=path):
                self.write(path, "x\n")
                self.assertEqual(self.selected(), "kms,s3,sqs,sts")
                self.git("reset", "-q", "--hard", self.base)

    def test_a_docs_only_change_selects_nothing(self) -> None:
        self.write("docs/x.md", "x\n")
        self.write(".github/workflows/ci.yml", "x\n")
        self.assertEqual(self.selected(), "")


if __name__ == "__main__":
    unittest.main()
