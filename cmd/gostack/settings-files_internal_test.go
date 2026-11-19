package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	servicesFile   = "services.go"
	settingsPrefix = "settings-"
	sqsFile        = "settings-sqs.go"
)

// Test_settingsFiles holds cmd/gostack/settings-<svc>.go to what parity
// selection assumes of it: that it is <svc>'s own file. parity/ledger.py
// selects <svc> for a change to it and not every service, so code that other
// services run on must not live there, and nothing but <svc>'s own
// registration may call its function.
func Test_settingsFiles(t *testing.T) {
	tables := readParityTables(t, "../..")

	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	sources := map[string]string{}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		text, err := os.ReadFile(name)
		require.NoError(t, err)

		sources[name] = string(text)
	}

	require.Empty(t, settingsFileProblems(sources, tables.TestDirs),
		"cmd/gostack/settings-<svc>.go: one function, <svc>Settings, used on <svc>'s own registration line in services.go")
}

func Test_settingsFileProblems(t *testing.T) {
	const (
		services = "package main\n\nfunc registrations() {\n\tsqs.New(liveSettings(cfg, sqsSettings))\n}\n"
		settings = "package main\n\nimport \"x\"\n\nfunc sqsSettings(c *x.C) int { return 0 }\n"
		known    = "sqs"
	)

	problems := func(sources map[string]string) []string {
		return settingsFileProblems(sources, []string{known, "s3"})
	}

	t.Run("accepts a file that holds its function, used on its own registration line", func(t *testing.T) {
		require.Empty(t, problems(map[string]string{servicesFile: services, sqsFile: settings}))
	})

	t.Run("refuses a settings file that holds another declaration", func(t *testing.T) {
		// Moving liveSettings into settings-sqs.go would make a change to a
		// helper every service uses select sqs alone.
		moved := settings + "\nfunc liveSettings() {}\n"
		got := problems(map[string]string{servicesFile: services, sqsFile: moved})

		require.Equal(t, []string{"settings-sqs.go: declares liveSettings, not only sqsSettings"}, got)
	})

	t.Run("refuses a settings file that holds a type, a variable or a constant", func(t *testing.T) {
		for _, extra := range []string{"type T int", "var v = 1", "const c = 1"} {
			got := problems(map[string]string{servicesFile: services, sqsFile: settings + "\n" + extra + "\n"})

			require.Len(t, got, 1, extra)
			require.Contains(t, got[0], "declares")
		}
	})

	t.Run("refuses a settings file for a service that is not known", func(t *testing.T) {
		ghost := "package main\n\nfunc ghostSettings() {}\n"
		got := problems(map[string]string{servicesFile: services, "settings-ghost.go": ghost})

		require.Equal(t, []string{"settings-ghost.go: ghost is not a known service"}, got)
	})

	t.Run("refuses a settings file without its function", func(t *testing.T) {
		got := problems(map[string]string{servicesFile: services, "settings-s3.go": "package main\n"})

		require.Equal(t, []string{"settings-s3.go: does not declare s3Settings"}, got)
	})

	t.Run("refuses a use of the function outside services.go", func(t *testing.T) {
		other := "package main\n\nfunc start() { _ = sqsSettings }\n"
		got := problems(map[string]string{servicesFile: services, sqsFile: settings, "start.go": other})

		require.Equal(t, []string{"start.go: uses sqsSettings; only sqs's registration line in services.go may"}, got)
	})

	t.Run("refuses a use in services.go on a line of another service", func(t *testing.T) {
		wrong := "package main\n\nfunc registrations() {\n\ts3.New(liveSettings(cfg, sqsSettings))\n}\n"
		got := problems(map[string]string{servicesFile: wrong, sqsFile: settings})

		require.Equal(t, []string{"services.go:4: uses sqsSettings off sqs's own registration line"}, got)
	})
}

// settingsFileProblems lists how the settings files among sources, every
// non-test Go file of cmd/gostack by name, break the rules of Test_settingsFiles.
func settingsFileProblems(sources map[string]string, known []string) []string {
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}

	var problems []string

	for _, name := range slices.Sorted(maps.Keys(sources)) {
		file, err := parser.ParseFile(fset, name, sources[name], 0)
		if err != nil {
			problems = append(problems, name+": "+err.Error())

			continue
		}

		parsed[name] = file
	}

	for _, name := range slices.Sorted(maps.Keys(parsed)) {
		svc, ok := settingsService(name)
		if !ok {
			continue
		}

		if !slices.Contains(known, svc) {
			problems = append(problems, name+": "+svc+" is not a known service")

			continue
		}

		problems = append(problems, settingsDeclarationProblems(name, svc, parsed[name])...)
		problems = append(problems, settingsUseProblems(fset, svc, parsed)...)
	}

	return problems
}

// settingsService returns <svc> for a file named settings-<svc>.go.
func settingsService(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, settingsPrefix)
	if !ok {
		return "", false
	}

	return strings.CutSuffix(rest, ".go")
}

// settingsDeclarationProblems lists what the file declares besides imports and
// <svc>Settings, or that it lacks <svc>Settings.
func settingsDeclarationProblems(name, svc string, file *ast.File) []string {
	want := svc + "Settings"
	found := false

	var problems []string

	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.GenDecl:
			if decl.Tok != token.IMPORT {
				problems = append(problems, name+": declares "+decl.Tok.String()+" declaration, not only "+want)
			}
		case *ast.FuncDecl:
			if decl.Recv == nil && decl.Name.Name == want {
				found = true

				continue
			}

			problems = append(problems, name+": declares "+decl.Name.Name+", not only "+want)
		default:
		}
	}

	if !found && len(problems) == 0 {
		problems = append(problems, name+": does not declare "+want)
	}

	return problems
}

// settingsUseProblems lists every use of <svc>Settings other than on a line of
// services.go that also names the <svc> package: <svc>'s own registration.
func settingsUseProblems(fset *token.FileSet, svc string, parsed map[string]*ast.File) []string {
	want := svc + "Settings"

	var problems []string

	for _, name := range slices.Sorted(maps.Keys(parsed)) {
		file := parsed[name]
		ownLines := packageLines(fset, file, svc)
		declared := declaredNames(file, want)

		ast.Inspect(file, func(node ast.Node) bool {
			ident, ok := node.(*ast.Ident)
			if !ok || ident.Name != want || declared[ident] {
				return true
			}

			line := fset.Position(ident.Pos()).Line

			switch {
			case name != servicesFile:
				problems = append(problems, name+": uses "+want+"; only "+svc+"'s registration line in services.go may")
			case !ownLines[line]:
				problems = append(problems, name+":"+strconv.Itoa(line)+": uses "+want+" off "+svc+"'s own registration line")
			default:
			}

			return true
		})
	}

	return problems
}

// packageLines lists the lines of file that name the package svc, as in
// svc.New.
func packageLines(fset *token.FileSet, file *ast.File, svc string) map[int]bool {
	lines := map[int]bool{}

	ast.Inspect(file, func(node ast.Node) bool {
		if sel, ok := node.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == svc {
				lines[fset.Position(pkg.Pos()).Line] = true
			}
		}

		return true
	})

	return lines
}

// declaredNames returns the identifiers that declare the function name, so a
// declaration is not counted as a use.
func declaredNames(file *ast.File, name string) map[*ast.Ident]bool {
	declared := map[*ast.Ident]bool{}

	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
			declared[fn.Name] = true
		}
	}

	return declared
}
