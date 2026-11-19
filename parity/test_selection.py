"""Tests for which services a change selects and which LocalStack test directory
each runs. Standard library only:

    python3 -m unittest discover -s parity -p 'test_*.py'
"""

from __future__ import annotations

import argparse
import contextlib
import difflib
import io
import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import ledger
from test_ledger import git_in, without_git_env

KNOWN = {"ssm", "sqs", "sts"}
REGISTRATION = ledger.REGISTRATION_FILE

SERVICES_GO = """\
package main

import (
	"github.com/kylecannon/gostack/internal/service/s3"
	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/kylecannon/gostack/internal/service/sts"
)

func newProviders() *providers {
	p := &providers{}
	p.regs = registrations()
	return p
}

func registrations() []service.Registration {
	return []service.Registration{
		sqs.New().Registration(),
		sts.Registration(),
		s3.New().Registration(),
	}
}
"""


def whole_diff(old: str, new: str) -> str:
    """The diff of REGISTRATION_FILE in the form registration_diff asks git
    for: the whole file as context, in one hunk."""
    lines = difflib.unified_diff(old.splitlines(), new.splitlines(), f"a/{REGISTRATION}", f"b/{REGISTRATION}", n=ledger.WHOLE_FILE, lineterm="")
    return "\n".join(lines) + "\n"


def edited(*replacements: tuple[str, str]) -> str:
    """SERVICES_GO with each (old, new) text replaced, once each."""
    text = SERVICES_GO
    for old, new in replacements:
        assert old in text, old
        text = text.replace(old, new, 1)
    return text


IMPORT_FOO = ("import (\n", 'import (\n\t"github.com/kylecannon/gostack/internal/service/foo"\n')
ELEMENT_FOO = ("\t\ts3.New().Registration(),\n", "\t\ts3.New().Registration(),\n\t\tfoo.Registration(),\n")
ADD_FOO = whole_diff(SERVICES_GO, edited(IMPORT_FOO, ELEMENT_FOO))


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
        paths = ["internal/service/ssm/keys.go", "internal/service/lambda/a.go"]
        self.assertEqual(ledger.affected_services(paths, KNOWN), {"ssm", "lambda"})

    def test_a_change_to_a_dependency_also_selects_the_services_that_depend_on_it(self) -> None:
        # s3 calls sqs in this process. Both reach other services' requests
        # too, so a fake table shows the dependents rule on its own.
        self.assertEqual(ledger.DEPENDS_ON["s3"], ("kms", "sns", "sqs"))
        with (
            mock.patch.object(ledger, "DEPENDS_ON", {"lambda": ("ssm",), "events": ("lambda",)}),
            mock.patch.object(ledger, "service_dirs", return_value={"lambda", "events"}),
        ):
            self.assertEqual(ledger.affected_services(["internal/service/ssm/keys.go"], KNOWN), {"ssm", "lambda", "events"})
            self.assertEqual(ledger.affected_services(["internal/service/lambda/a.go"], KNOWN), {"lambda", "events"})

    def test_a_dependent_that_is_not_built_is_not_selected(self) -> None:
        # A seeded DEPENDS_ON entry names a service nobody has built: the image
        # does not serve it and the ledger has no rows for it.
        with mock.patch.object(ledger, "DEPENDS_ON", {"lambda": ("ssm",)}), mock.patch.object(ledger, "service_dirs", return_value=set()):
            self.assertEqual(ledger.affected_services(["internal/service/ssm/keys.go"], KNOWN), {"ssm"})
        with mock.patch.object(ledger, "DEPENDS_ON", {"lambda": ("ssm",)}), mock.patch.object(ledger, "service_dirs", return_value={"lambda"}):
            self.assertEqual(ledger.affected_services(["internal/service/ssm/keys.go"], KNOWN), {"ssm", "lambda"})
        with mock.patch.object(ledger, "DEPENDS_ON", {"lambda": ("ssm",)}), mock.patch.object(ledger, "service_dirs", return_value=set()):
            self.assertEqual(ledger.affected_services(["internal/service/ssm/keys.go"], KNOWN | {"lambda"}), {"ssm", "lambda"})

    def test_the_services_built_next_are_seeded_with_their_dependencies(self) -> None:
        # The branch that builds one edits no table. Each service in the seeded
        # entries is a test directory the ledger knows by name.
        self.assertEqual(ledger.DEPENDS_ON["sns"], ("sqs",))
        self.assertEqual(ledger.DEPENDS_ON["ssm"], ("secretsmanager",))
        self.assertEqual(ledger.DEPENDS_ON["dynamodbstreams"], ("dynamodb",))
        self.assertEqual(ledger.DEPENDS_ON["dynamodb"], ("dynamodbstreams",))
        for service, dependencies in ledger.DEPENDS_ON.items():
            for name in (service, *dependencies):
                if name != ledger.EVERY_SERVICE:
                    self.assertIn(name, ledger.TEST_DIRS)

    def test_once_built_a_seeded_service_selects_what_the_code_will_have_it_select(self) -> None:
        built = {"dynamodb", "dynamodbstreams", "kms", "s3", "secretsmanager", "sns", "sqs", "ssm", "sts"}
        known = built
        with mock.patch.object(ledger, "service_dirs", return_value=built):
            cases = {
                "internal/service/kms/keys.go": {"kms", "s3"},
                "internal/service/sns/topics.go": {"sns", "s3"},
                "internal/api/sns/types.go": {"sns", "s3"},
                "internal/service/secretsmanager/secrets.go": {"secretsmanager", "ssm"},
                "internal/service/ssm/parameters.go": {"ssm"},
                "internal/service/dynamodb/tables.go": {"dynamodb", "dynamodbstreams"},
                "internal/service/dynamodbstreams/streams.go": {"dynamodb", "dynamodbstreams"},
                "internal/service/sqs/queue.go": known,
            }
            for path, want in cases.items():
                with self.subTest(path=path):
                    self.assertEqual(ledger.affected_services([path], known), want)

    def test_a_service_without_routing_hooks_selects_itself_and_its_dependents(self) -> None:
        # ssm sets none of Mount, ClaimHost, Claim, Fallback or Preprocess.
        self.assertNotIn("ssm", ledger.ROUTING_REACH)
        self.assertEqual(ledger.affected_services(["internal/service/ssm/keys.go"], KNOWN), {"ssm"})

    def test_a_change_to_a_service_with_routing_hooks_selects_every_service(self) -> None:
        # SQ-199 item2: s3's Fallback, Preprocess and ClaimHost and sqs's
        # queue-URL mount see requests meant for any service.
        self.assertEqual(set(ledger.ROUTING_REACH), {"s3", "sqs"})
        for path in ("internal/service/s3/object.go", "internal/service/sqs/routes.go", "internal/api/s3/types.go", "internal/api/sqs/types.go"):
            with self.subTest(path=path):
                self.assertEqual(ledger.affected_services([path], KNOWN), KNOWN | {"s3"})
        self.assertEqual(ledger.affected_services([REGISTRATION], KNOWN, registration={"s3"}), KNOWN | {"s3"})

    def test_a_change_to_sts_selects_every_service_because_every_test_session_calls_it(self) -> None:
        # SQ-181 S4: LocalStack's account_id and secondary_account_id session
        # fixtures call sts.get_caller_identity, so an sts change that keeps the
        # sts rows green can still break sqs test_cross_account_access[domain].
        self.assertEqual(ledger.DEPENDS_ON[ledger.EVERY_SERVICE], ("sts",))
        known = KNOWN | {"s3"}
        for path in ("internal/service/sts/operations.go", "internal/api/sts/types.go"):
            with self.subTest(path=path):
                self.assertEqual(ledger.affected_services([path], known), known)
        # Every known service, plus a service selected beside it that has no row yet.
        self.assertEqual(ledger.affected_services(["internal/service/sts/a.go", "internal/service/lambda/a.go"], known), known | {"lambda"})

    def test_a_services_own_ledger_file_selects_only_it_not_its_dependents(self) -> None:
        self.assertEqual(ledger.affected_services(["parity/ledger/sqs.json"], KNOWN), {"sqs"})
        self.assertEqual(ledger.affected_services(["parity/ledger/sts.json", "parity/exclusions/sts.yaml"], KNOWN), {"sts"})

    def test_a_services_generated_api_package_is_its_own_path(self) -> None:
        with mock.patch.object(ledger, "service_dirs", return_value={"ssm", "lambda", "sqs"}):
            self.assertEqual(ledger.affected_services(["internal/api/ssm/operations.go"], KNOWN), {"ssm"})
            self.assertEqual(ledger.affected_services(["internal/api/lambda/types.go"], KNOWN), {"lambda"})

    def test_a_generated_api_package_with_no_service_directory_is_shared(self) -> None:
        # SQ-182 generates packages for services not registered yet; the image
        # serves none of them, so they select what a shared path selects.
        with mock.patch.object(ledger, "service_dirs", return_value={"sqs"}):
            self.assertEqual(ledger.affected_services(["internal/api/lambda/types.go"], KNOWN), KNOWN)

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
            "LICENSE.md",
            "internal/gateway/x_test.go.orig",
            "internal/gateway/x_test.gox",
            "codegen/cmd/gostack-codegen/config.go",
            "internal/service/lambda",  # a file named like a service directory
            "internal/api/doc.go",
            "internal/api/zzz/types.go",
            "internal/api/lambda",
            "internal/apix/lambda/a.go",
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
            # SQ-199 item7: go build leaves out every _test.go file.
            "LICENSE",
            "internal/gateway/gateway_test.go",
            "cmd/gostack/services_internal_test.go",
            "internal/service/sqs/queue_test.go",
            "internal/service/s3/object_internal_test.go",
            "internal/api/sqs/validate_test.go",
            "codegen/cmd/gostack-codegen/names_internal_test.go",
        ):
            with self.subTest(path=path):
                self.assertEqual(ledger.affected_services([path], KNOWN), set())

    def test_a_test_file_beside_code_selects_what_the_code_selects(self) -> None:
        self.assertEqual(ledger.affected_services(["internal/service/ssm/a_test.go", "internal/service/ssm/a.go"], KNOWN), {"ssm"})
        self.assertEqual(ledger.affected_services(["internal/gateway/a_test.go", "codegen/cmd/gostack-codegen/emit.go"], KNOWN), KNOWN)

    def test_the_no_effect_list_matches_whole_names_only(self) -> None:
        for path in ("docsx/a.md", "README.md.go", "internal/docs/a.go", "cmd/README.md"):
            with self.subTest(path=path):
                self.assertEqual(ledger.affected_services([path], KNOWN), KNOWN)

    def test_registration_file_is_shared_unless_its_lines_are_attributed(self) -> None:
        paths = ["internal/service/lambda/lambda.go", REGISTRATION]
        self.assertEqual(ledger.affected_services(paths, KNOWN), KNOWN | {"lambda"})
        self.assertEqual(ledger.affected_services(paths, KNOWN, registration={"lambda"}), {"lambda"})

    def test_registration_lines_naming_another_service_select_it_too(self) -> None:
        paths = ["internal/service/lambda/lambda.go", REGISTRATION]
        self.assertEqual(ledger.affected_services(paths, KNOWN, registration={"ssm"}), {"lambda", "ssm"})
        # sts selects every known service, and sqs among them selects s3.
        self.assertEqual(ledger.affected_services(paths, KNOWN, registration={"sts"}), KNOWN | {"lambda", "s3"})
        # sqs's mount reaches every service's requests.
        self.assertEqual(ledger.affected_services(paths, KNOWN, registration={"sqs"}), KNOWN | {"lambda", "s3"})

    def test_attributed_registration_does_not_excuse_another_shared_path(self) -> None:
        paths = ["internal/service/lambda/lambda.go", REGISTRATION, "internal/gateway/gateway.go"]
        self.assertEqual(ledger.affected_services(paths, KNOWN, registration={"lambda"}), KNOWN | {"lambda"})


class RegistrationServicesTest(unittest.TestCase):
    """Only a whole element of the list registrations() returns, or an import
    of a service's package, is attributed to the services it names."""

    NAMES = {"foo", "ssm", "sqs", "sts", "s3"}

    def services(self, *replacements: tuple[str, str], names: set[str] | None = None) -> set[str] | None:
        return ledger.registration_services(whole_diff(SERVICES_GO, edited(*replacements)), self.NAMES if names is None else names)

    def test_an_added_import_and_element_name_the_service_they_register(self) -> None:
        self.assertEqual(ledger.registration_services(ADD_FOO, self.NAMES), {"foo"})
        self.assertEqual(self.services(IMPORT_FOO), {"foo"})
        self.assertEqual(self.services(ELEMENT_FOO), {"foo"})
        self.assertEqual(self.services(("\t\"github.com/kylecannon/gostack/internal/service/s3\"\n", '\ts3x "github.com/kylecannon/gostack/internal/service/s3"\n')), {"s3"})

    def test_removed_and_edited_elements_count_as_changes_too(self) -> None:
        got = self.services(IMPORT_FOO, ELEMENT_FOO, ("\t\tsts.Registration(),\n", ""), ("sqs.New()", "sqs.New(true)"))
        self.assertEqual(got, {"foo", "sts", "sqs"})
        self.assertEqual(ledger.registration_services(whole_diff(edited(ELEMENT_FOO), SERVICES_GO), self.NAMES), {"foo"})

    def test_an_element_naming_two_services_selects_both(self) -> None:
        self.assertEqual(self.services(("sqs.New()", "sqs.New(ssm.Client())")), {"sqs", "ssm"})

    def test_a_call_of_registrations_is_not_an_element(self) -> None:
        # SQ-199 item4, the reviewer's case: `p.regs = registrations(cfg, nil,
        # p.s3)` names s3 on both sides but hands every registration its
        # arguments.
        base = edited(("\tp.regs = registrations()\n", "\tp.regs = registrations(cfg, changed, p.s3)\n"))
        self.assertIsNone(ledger.registration_services(whole_diff(base, base.replace("changed, p.s3", "nil, p.s3")), self.NAMES))

    def test_a_change_to_the_signature_of_registrations_is_not_an_element(self) -> None:
        old = "func registrations() []service.Registration {\n"
        base = edited((old, "func registrations(s3Provider *s3.Provider) []service.Registration {\n"))
        changed = base.replace("(s3Provider *s3.Provider)", "(cfg *config.Config, s3Provider *s3.Provider)")
        self.assertIsNone(ledger.registration_services(whole_diff(base, changed), self.NAMES))
        self.assertIsNone(self.services(("\treturn []service.Registration{\n", "\treturn []service.Registration{ // sqs\n")))
        self.assertIsNone(self.services(("\treturn []service.Registration{\n", "\treturn append(base, []service.Registration{\n")))

    def test_an_element_split_over_lines_cannot_be_attributed(self) -> None:
        # SQ-181 S5 named the closing lines of a multi-line registration
        # nothing; none of its lines is a whole element, so it selects every service.
        split = ("\t\ts3.New().Registration(),\n", "\t\ts3.New().Registration(),\n\t\tfoo.New(\n\t\t\tfoo.Settings{},\n\t\t).Registration(),\n")
        self.assertIsNone(self.services(IMPORT_FOO, split))
        self.assertIsNone(self.services(("\t\tsts.Registration(),\n", "\t\tsts.Registration(func() bool {\n\t\t\treturn true\n\t\t}),\n")))

    def test_a_helper_beside_the_list_cannot_be_attributed(self) -> None:
        helper = "\n// fooSettings are the settings of the foo provider.\nfunc fooSettings() foo.Settings {\n\treturn foo.Settings{}\n}\n"
        self.assertIsNone(ledger.registration_services(whole_diff(SERVICES_GO, edited(IMPORT_FOO, ELEMENT_FOO) + helper), self.NAMES))
        self.assertIsNone(self.services(("\t}\n}\n", "\t}\n}\n\nfunc other() []service.Registration {\n\treturn []service.Registration{\n\t\tsqs.New().Registration(),\n\t}\n}\n")))

    def test_a_line_shaped_like_an_element_outside_the_list_cannot_be_attributed(self) -> None:
        base = SERVICES_GO + "\nfunc extras() []service.Registration {\n\treturn []service.Registration{\n\t\tsts.Registration(),\n\t}\n}\n"
        changed = base.replace("\t\tsts.Registration(),\n\t}\n}\n", "\t\tsts.Registration(),\n\t\tsqs.New().Registration(),\n\t}\n}\n")
        self.assertNotEqual(base, changed)
        self.assertIsNone(ledger.registration_services(whole_diff(base, changed), self.NAMES))

    def test_an_element_naming_no_service_cannot_be_attributed(self) -> None:
        for element in ("\t\ts3Provider.Registration(),\n", "\t\tnewThing().Registration(),\n", '\t\tnewThing("sqs").Registration(),\n', "\t\tnewThing(), // sqs\n", "\t\tfoobar.Registration(),\n"):
            with self.subTest(element=element):
                self.assertIsNone(self.services(("\t\ts3.New().Registration(),\n", element)))

    def test_a_directive_is_code(self) -> None:
        # SQ-199 item5: //go:debug and //go:build change the binary.
        for directive in ("//go:debug madvdontneed=1\n", "//go:build linux\n", "// +build linux\n", "//line services.go:1\n"):
            with self.subTest(directive=directive):
                self.assertIsNone(self.services(("package main\n", directive + "\npackage main\n")))

    def test_a_service_named_only_in_a_comment_does_not_count(self) -> None:
        # SQ-177 item5 5.g2 and 5.g3: a global change carrying the service name
        # in a trailing comment.
        for line in ('\tos.Setenv("GOSTACK_LOOSE", "1") // sts\n', "\tgateway.SkipSignatureCheck = true // sqs\n"):
            with self.subTest(line=line):
                self.assertIsNone(self.services(("\tp := &providers{}\n", "\tp := &providers{}\n" + line)))

    def test_comment_and_blank_line_changes_name_nothing_and_change_nothing(self) -> None:
        changes = (("\t\tsqs.New().Registration(),\n", "\t\t// registrations, one per line\n\n\t\tsqs.New().Registration(),\n"), ("package main\n", "// Package main.\npackage main\n"))
        self.assertEqual(self.services(*changes), set())
        # A block comment is not read as one: it may hide a whole element. See
        # test_a_changed_line_in_or_opening_a_block_comment_cannot_be_attributed.
        self.assertIsNone(self.services(("\t\tsts.Registration(),\n", "\t\tsts.Registration(),\n\t\t/* not a directive */\n")))

    def test_a_changed_line_in_or_opening_a_block_comment_cannot_be_attributed(self) -> None:
        # SQ-206 N2, the reviewer's case: a block comment between two edited
        # lines comments out the element between them. Both edited lines are
        # whole elements, but the sts line that went dead is unchanged and
        # would otherwise select nothing.
        opened = ("\t\tsqs.New().Registration(),\n", "\t\tsqs.New().Registration(), /*,\n")
        closed = ("\t\ts3.New().Registration(),\n", "\t\t*/ s3.New().Registration(),\n")
        self.assertIsNone(self.services(opened, closed))
        # The opening alone, and a line inside the block, too.
        self.assertIsNone(self.services(opened))
        self.assertIsNone(self.services(("\t\tsts.Registration(),\n", "\t\t/*\n\t\tsts.Registration(),\n\t\t*/\n")))
        self.assertIsNone(self.services(("\t\tsqs.New().Registration(),\n", "\t\t/* sqs.New().Registration(), */\n")))
        # And the other way round: the diff that brings the element back.
        commented = edited(opened, closed)
        self.assertIsNone(ledger.registration_services(whole_diff(commented, SERVICES_GO), self.NAMES))
        # An unchanged block comment, and a /* in a string or a // comment, change nothing.
        doc = ("func registrations() []service.Registration {\n", "/* registrations lists\n   every service. */\nfunc registrations() []service.Registration {\n")
        self.assertEqual(ledger.registration_services(whole_diff(edited(doc), edited(doc, ELEMENT_FOO, IMPORT_FOO)), self.NAMES), {"foo"})
        self.assertEqual(ledger.block_comment_lines(['a := "/*"', "b := `/*", "x`", "c := '\"' // /*", "d := 1"]), set())
        self.assertEqual(ledger.block_comment_lines(["a /* b", "c", "d */ e", "f"]), {0, 1, 2})
        self.assertEqual(self.services(IMPORT_FOO, ELEMENT_FOO, ("\t\tsts.Registration(),\n", '\t\tsts.Registration(), // "/*"\n')), {"foo", "sts"})

    def test_a_name_that_is_not_a_service_directory_does_not_count(self) -> None:
        self.assertIsNone(ledger.registration_services(ADD_FOO, {"sqs"}))
        self.assertIsNone(ledger.registration_services(ADD_FOO, set()))

    def test_a_diff_with_no_changed_line_or_not_of_the_whole_file_cannot_be_attributed(self) -> None:
        self.assertIsNone(ledger.registration_services("", self.NAMES))
        self.assertIsNone(ledger.registration_services(whole_diff(SERVICES_GO, SERVICES_GO), self.NAMES))
        zero_context = "--- a/x\n+++ b/x\n@@ -12,0 +13 @@ func registrations() []service.Registration {\n+\t\tfoo.Registration(),\n"
        self.assertIsNone(ledger.registration_services(zero_context, self.NAMES))
        two_hunks = "@@ -1,2 +1,2 @@\n a\n-b\n+c\n@@ -9,1 +9,1 @@\n-d\n+e\n"
        self.assertIsNone(ledger.registration_services(two_hunks, self.NAMES))


class ServicesCommandTest(unittest.TestCase):
    def setUp(self) -> None:
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.ledger = Path(tmp.name) / "ledger"
        entry = {"localstack": "pass", "gostack": "pass", "reason": ""}
        ledger.write_ledger(self.ledger, {f"tests/aws/services/{d}/test_x.py::t": entry for d in ("ssm", "sqs", "sts")})

    def run_services(self, paths: list[str], diff: str | None, changed: bool = True, revisions: str | None = "a...b") -> str:
        args = argparse.Namespace(ledger=self.ledger, changed=changed, range=revisions)
        out = io.StringIO()
        with (
            contextlib.redirect_stdout(out),
            mock.patch("sys.stdin", io.StringIO("".join(p + "\0" for p in paths))),
            mock.patch.object(ledger, "registration_diff", return_value=diff),
            mock.patch.object(ledger, "service_dirs", return_value={"foo", "ssm", "sqs", "sts", "s3"}),
        ):
            ledger.services(args)
        return out.getvalue().strip()

    def test_without_changed_every_ledger_service_is_printed(self) -> None:
        self.assertEqual(self.run_services([], None, changed=False), "sqs,ssm,sts")

    def test_no_changed_path_selects_nothing(self) -> None:
        self.assertEqual(self.run_services([], None), "")

    def test_new_service_with_its_registration_line_selects_only_that_service(self) -> None:
        paths = ["internal/service/foo/foo.go", REGISTRATION]
        with mock.patch.dict(ledger.TEST_DIRS, {"foo": "foo"}):
            self.assertEqual(self.run_services(paths, ADD_FOO), "foo")

    def test_registration_line_for_another_service_beside_one_services_change_selects_both(self) -> None:
        # S2: before, the lambda change excused any added line.
        diff = whole_diff(SERVICES_GO, edited(("\t\tsts.Registration(),\n", "\t\tsts.Registration(),\n\t\tssm.New().Registration(),\n")))
        self.assertEqual(self.run_services(["internal/service/lambda/a.go", REGISTRATION], diff), "lambda,ssm")

    def test_registration_edit_naming_no_service_selects_everything(self) -> None:
        paths = ["internal/service/foo/foo.go", REGISTRATION]
        diff = whole_diff(SERVICES_GO, edited(IMPORT_FOO, ELEMENT_FOO, ("\tp := &providers{}\n", "\tp := &providers{old()}\n")))
        with mock.patch.dict(ledger.TEST_DIRS, {"foo": "foo"}):
            self.assertEqual(self.run_services(paths, diff), "foo,sqs,ssm,sts")

    def test_registration_edit_without_a_range_selects_everything(self) -> None:
        self.assertEqual(self.run_services([REGISTRATION], ADD_FOO, revisions=None), "sqs,ssm,sts")

    def test_registration_edit_when_git_cannot_diff_selects_everything(self) -> None:
        self.assertEqual(self.run_services([REGISTRATION], None), "sqs,ssm,sts")

    def test_a_change_under_lambda_selects_lambda_without_a_ledger_row(self) -> None:
        self.assertEqual(self.run_services(["internal/service/lambda/a.go"], None), "lambda")

    def test_newline_separated_input_is_refused(self) -> None:
        args = argparse.Namespace(ledger=self.ledger, changed=True, range=None)
        with mock.patch("sys.stdin", io.StringIO("internal/service/sqs/a.go\n")), self.assertRaises(SystemExit) as raised:
            ledger.services(args)
        self.assertIn("NUL-separated", str(raised.exception))

    def test_a_name_with_a_newline_is_one_path(self) -> None:
        self.assertEqual(self.run_services(["internal/service/lambda/a\nb.go"], None), "lambda")


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


class SettingsFileSelectionTest(unittest.TestCase):
    """cmd/gostack/settings-<svc>.go maps the configuration onto <svc>'s
    settings: it is <svc>'s own path. Every other file of cmd/gostack is
    shared."""

    def test_a_services_settings_file_selects_that_service_and_its_dependents(self) -> None:
        known = {"s3", "sqs", "sts", "ssm"}
        with mock.patch.object(ledger, "service_dirs", return_value=known):
            self.assertEqual(ledger.affected_services(["cmd/gostack/settings-ssm.go"], known), {"ssm"})
            # s3 calls sqs, so an sqs change runs s3's suite too.
            self.assertEqual(ledger.affected_services(["cmd/gostack/settings-sqs.go"], known), {"sqs", "s3"})

    def test_a_settings_file_does_not_select_every_service_for_a_service_with_routing_hooks(self) -> None:
        # The file maps configuration and sets no hook; the hooks are in
        # internal/service/<svc>/, which still selects every service.
        known = {"s3", "sqs", "sts", "ssm"}
        self.assertIn("sqs", ledger.ROUTING_REACH)
        with mock.patch.object(ledger, "service_dirs", return_value=known):
            self.assertEqual(ledger.affected_services(["cmd/gostack/settings-sqs.go"], known), {"sqs", "s3"})
            self.assertEqual(ledger.affected_services(["cmd/gostack/settings-s3.go"], known), {"s3"})
            self.assertEqual(ledger.affected_services(["internal/service/sqs/queue.go"], known), known)

    def test_the_sts_settings_file_selects_every_service_like_the_rest_of_sts(self) -> None:
        known = {"s3", "sqs", "sts", "ssm"}
        with mock.patch.object(ledger, "service_dirs", return_value=known):
            self.assertEqual(ledger.affected_services(["cmd/gostack/settings-sts.go"], known), known)

    def test_a_settings_file_beside_another_services_code_selects_both(self) -> None:
        known = {"s3", "sqs", "sts", "ssm"}
        with mock.patch.object(ledger, "service_dirs", return_value=known):
            self.assertEqual(ledger.affected_services(["cmd/gostack/settings-ssm.go", "internal/service/sts/a.go"], known), known)

    def test_the_rest_of_cmd_gostack_is_shared(self) -> None:
        known = {"s3", "sqs", "sts", "ssm"}
        for path in (
            "cmd/gostack/settings.go",
            "cmd/gostack/main.go",
            "cmd/gostack/settings-.go",
            "cmd/gostack/settings-nope.go",
            "cmd/gostack/settings-ssm.txt",
            "cmd/gostack/sub/settings-ssm.go",
            "cmd/gostack/xsettings-ssm.go",
            "internal/settings-ssm.go",
        ):
            with self.subTest(path=path), mock.patch.object(ledger, "service_dirs", return_value=known):
                self.assertEqual(ledger.affected_services([path], known), known)

    def test_a_settings_test_file_selects_nothing(self) -> None:
        self.assertEqual(ledger.affected_services(["cmd/gostack/settings-ssm_test.go", "cmd/gostack/settings-ssm_internal_test.go"], KNOWN), set())

    def test_a_settings_file_beside_the_registration_lines_selects_what_each_selects(self) -> None:
        known = {"s3", "sqs", "sts", "ssm"}
        with mock.patch.object(ledger, "service_dirs", return_value=known):
            self.assertEqual(ledger.affected_services(["cmd/gostack/settings-ssm.go", REGISTRATION], known, registration={"ssm"}), {"ssm"})
            self.assertEqual(ledger.affected_services(["cmd/gostack/settings-ssm.go", REGISTRATION], known), known)

    def test_the_tables_command_lists_the_test_directories_for_the_go_test(self) -> None:
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            ledger.tables(argparse.Namespace())
        tables = json.loads(out.getvalue())
        self.assertEqual(tables["test_dirs"], sorted(ledger.TEST_DIRS))
        self.assertEqual({svc: tuple(deps) for svc, deps in tables["depends_on"].items()}, ledger.DEPENDS_ON)


class RealDiffSelectionTest(unittest.TestCase):
    """The selection as parity.yml feeds it: `git diff --name-only -z
    --no-renames` from the base, in a temporary repository."""

    def setUp(self) -> None:
        without_git_env(self)
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.repo = Path(tmp.name).resolve()
        self.write(REGISTRATION, SERVICES_GO)
        for service in ("ssm", "s3", "sqs", "sts"):
            self.write(f"internal/service/{service}/{service}.go", f"package {service}\n")
            self.write(f"parity/exclusions/{service}.yaml", "# none\n")
        self.write("internal/gateway/gateway.go", "package gateway\n")
        entry = {"localstack": "pass", "gostack": "pass", "reason": ""}
        ledger.write_ledger(self.repo / "parity" / "ledger", {f"tests/aws/services/{s}/test_x.py::t": entry for s in ("ssm", "s3", "sqs", "sts")})
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

    def test_a_change_under_a_service_without_routing_hooks_selects_only_it(self) -> None:
        self.write("internal/service/ssm/keys.go", "package ssm\n")
        self.assertEqual(self.selected(), "ssm")

    def test_a_diff_touching_only_one_services_settings_file_selects_that_service_and_its_dependents(self) -> None:
        self.write("cmd/gostack/settings-sqs.go", "package main\n\n// sqs settings\n")
        self.assertEqual(self.selected(), "s3,sqs")

    def test_a_new_service_branch_with_its_settings_file_and_two_shared_lines_selects_it_and_its_dependents(self) -> None:
        # The kms branch: its package, its settings file, one import and one
        # line in registrations(). s3 will call kms (DEPENDS_ON).
        self.write("internal/service/kms/x.go", "package kms\n")
        self.write("cmd/gostack/settings-kms.go", "package main\n")
        text = (self.repo / REGISTRATION).read_text(encoding="utf-8")
        text = text.replace("\t\"github.com/kylecannon/gostack/internal/service/sqs\"\n", '\t"github.com/kylecannon/gostack/internal/service/kms"\n\t"github.com/kylecannon/gostack/internal/service/sqs"\n')
        self.write(REGISTRATION, text.replace("\t\ts3.New().Registration(),\n", "\t\ts3.New().Registration(),\n\t\tkms.New(liveSettings(cfg, kmsSettings)).Registration(),\n"))
        self.assertEqual(self.selected(), "kms,s3")

    def test_a_change_under_a_service_with_routing_hooks_selects_every_service(self) -> None:
        # SQ-199 item2: S3's Fallback and Preprocess, and SQS's queue-URL mount.
        for path in ("internal/service/s3/website.go", "internal/service/sqs/routes.go"):
            with self.subTest(path=path):
                self.write(path, "package x\n")
                self.assertEqual(self.selected(), "s3,sqs,ssm,sts")
                self.git("reset", "-q", "--hard", self.base)

    def test_a_rename_from_a_shared_path_into_a_service_selects_every_service(self) -> None:
        # SQ-177 item5 5.r: without --no-renames only the new path was listed.
        self.git("mv", "internal/gateway/gateway.go", "internal/service/sts/gateway.go")
        self.assertEqual(self.selected(), "s3,sqs,ssm,sts")

    def test_a_quoted_shared_name_selects_every_service(self) -> None:
        # SQ-177 item5 5.u and 5.q: git quoted these names, and the quoted path
        # matched nothing.
        for name in ("é.go", 'a"b.go'):
            with self.subTest(name=name):
                self.write(f"internal/gateway/{name}", "package gateway\n")
                self.assertEqual(self.selected(), "s3,sqs,ssm,sts")
                self.git("reset", "-q", "--hard", self.base)

    def test_a_global_line_with_a_service_comment_beside_a_service_file_selects_every_service(self) -> None:
        # SQ-177 item5 5.g3: only sts.yaml touched, plus a global line ending in // sts.
        self.write("parity/exclusions/sts.yaml", "# none\n# touch\n")
        self.add_to_registration('\nfunc init() { os.Setenv("GOSTACK_LOOSE", "1") } // sts\n')
        self.assertEqual(self.selected(), "s3,sqs,ssm,sts")

    def test_an_import_line_naming_the_changed_service_selects_only_it_and_its_dependents(self) -> None:
        # SQ-177 item5 5.g1.
        self.write("internal/service/ssm/new.go", "package ssm\n")
        text = (self.repo / REGISTRATION).read_text(encoding="utf-8")
        self.write(REGISTRATION, text.replace("import (\n", 'import (\n\t_ "github.com/kylecannon/gostack/internal/service/ssm"\n'))
        self.assertEqual(self.selected(), "ssm")

    def test_a_registration_line_for_another_service_selects_it_as_well(self) -> None:
        self.write("internal/service/ssm/new.go", "package ssm\n")
        self.write("internal/service/iam/iam.go", "package iam\n")
        text = (self.repo / REGISTRATION).read_text(encoding="utf-8")
        self.write(REGISTRATION, text.replace("\t\ts3.New().Registration(),\n", "\t\ts3.New().Registration(),\n\t\tiam.New().Registration(),\n"))
        self.assertEqual(self.selected(), "iam,ssm")

    def test_a_registration_edit_naming_a_service_with_routing_hooks_selects_every_service(self) -> None:
        self.write("internal/service/ssm/new.go", "package ssm\n")
        text = (self.repo / REGISTRATION).read_text(encoding="utf-8")
        self.write(REGISTRATION, text.replace("\t\tsqs.New().Registration(),\n", "\t\tsqs.New(true).Registration(),\n"))
        self.assertEqual(self.selected(), "s3,sqs,ssm,sts")

    def test_a_call_of_registrations_that_names_a_service_selects_every_service(self) -> None:
        # SQ-199 item4, the reviewer's case: the call names s3 and changes
        # what every registration is built from.
        text = (self.repo / REGISTRATION).read_text(encoding="utf-8")
        text = text.replace("\tp.regs = registrations()\n", "\tp.regs = registrations(cfg, changed, p.ssm)\n")
        self.write(REGISTRATION, text.replace("func registrations() ", "func registrations(cfg *config.Config, changed func(string), ssm *ssm.Provider) "))
        self.base = self.commit("registrations takes ssm")
        self.write("internal/service/ssm/new.go", "package ssm\n")
        self.write(REGISTRATION, (self.repo / REGISTRATION).read_text(encoding="utf-8").replace("changed, p.ssm", "nil, p.ssm"))
        self.assertEqual(self.selected(), "s3,sqs,ssm,sts")

    def test_a_directive_selects_every_service(self) -> None:
        # SQ-199 item5.
        text = (self.repo / REGISTRATION).read_text(encoding="utf-8")
        self.write(REGISTRATION, "//go:debug madvdontneed=1\n\n" + text)
        self.assertEqual(self.selected(), "s3,sqs,ssm,sts")

    def test_test_files_and_the_licence_alone_select_nothing(self) -> None:
        # SQ-199 item7.
        self.write("internal/gateway/gateway_test.go", "package gateway\n")
        self.write("internal/service/sqs/queue_test.go", "package sqs\n")
        self.write("cmd/gostack/services_internal_test.go", "package main\n")
        self.write("LICENSE", "MIT\n")
        self.assertEqual(self.selected(), "")

    def test_a_change_under_sts_selects_every_service(self) -> None:
        # SQ-181 S4, the reviewer's case: operations.go's caller identity feeds
        # every service's account_id fixture.
        self.write("internal/service/sts/operations.go", "package sts\n\n// fixed account\n")
        self.assertEqual(self.selected(), "s3,sqs,ssm,sts")

    def add_iam_branch(self) -> str:
        """A new-service branch's own files, its import and its one-line
        registration; returns REGISTRATION_FILE's new text."""
        self.write("internal/service/iam/iam.go", "package iam\n")
        self.write("internal/service/iam/iam_test.go", "package iam\n")
        self.write("internal/api/iam/types.go", "// Code generated by gostack-codegen. DO NOT EDIT.\n\npackage iam\n")
        self.write("internal/api/iam/operations.go", "package iam\n")
        self.write("parity/ledger/iam.json", "{}\n")
        self.write("parity/exclusions/iam.yaml", "# iam\n")
        text = (self.repo / REGISTRATION).read_text(encoding="utf-8")
        text = text.replace("\t\"github.com/kylecannon/gostack/internal/service/sqs\"\n", '\t"github.com/kylecannon/gostack/internal/service/iam"\n\t"github.com/kylecannon/gostack/internal/service/sqs"\n')
        return text.replace("\t\ts3.New().Registration(),\n", "\t\ts3.New().Registration(),\n\t\tiam.New(liveSettings(cfg, iamSettings)).Registration(),\n")

    def test_a_new_service_branch_that_adds_only_its_own_files_selects_only_itself(self) -> None:
        # SQ-177 item5 5.n: the generated package, the import and the one line
        # in registrations(), as a real new-service branch has them.
        self.write(REGISTRATION, self.add_iam_branch())
        self.assertEqual(self.selected(), "iam")

    def test_a_new_service_branch_that_adds_a_helper_to_the_registration_file_selects_every_service(self) -> None:
        # SQ-199 item4 reverses SQ-181 S5: a helper beside the list is code no
        # line-by-line rule can place, so it selects every service.
        text = self.add_iam_branch() + (
            "\n// iamSettings maps the configuration onto the settings of the IAM provider.\n"
            "func iamSettings(current *config.Settings) iam.Settings {\n"
            "\treturn iam.Settings{}\n"
            "}\n"
        )
        self.write(REGISTRATION, text)
        self.assertEqual(self.selected(), "iam,s3,sqs,ssm,sts")

    def test_a_new_service_registration_with_a_line_naming_no_service_selects_every_service(self) -> None:
        self.write("internal/service/iam/iam.go", "package iam\n")
        text = (self.repo / REGISTRATION).read_text(encoding="utf-8")
        text = text.replace("\t\ts3.New().Registration(),\n", "\t\ts3.New().Registration(),\n\t\tiam.New(\n\t\t\tcfg.Settings(),\n\t\t).Registration(),\n")
        self.write(REGISTRATION, text)
        self.assertEqual(self.selected(), "iam,s3,sqs,ssm,sts")

    def test_a_makefile_or_api_change_selects_every_service(self) -> None:
        # The Makefile's GEN_SERVICES line and the generator's serviceTable are
        # shared on purpose: they change what every generated package is.
        for path in ("Makefile", "api/openapi/gostack.yaml", "internal/service/common/util.go", "codegen/cmd/gostack-codegen/config.go"):
            with self.subTest(path=path):
                self.write(path, "x\n")
                self.assertEqual(self.selected(), "s3,sqs,ssm,sts")
                self.git("reset", "-q", "--hard", self.base)

    def test_a_docs_only_change_selects_nothing(self) -> None:
        self.write("docs/x.md", "x\n")
        self.write(".github/workflows/ci.yml", "x\n")
        self.assertEqual(self.selected(), "")


if __name__ == "__main__":
    unittest.main()
