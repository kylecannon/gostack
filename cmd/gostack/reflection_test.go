package main_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// mainSymbol is a symbol every Go binary with a symbol table has. A binary
// built with -s has none, and a check of its symbols would find nothing and
// pass, so the test refuses a binary that does not list it.
const mainSymbol = "main.main"

// prunedMethods are exported methods of types that are in the binary, that no
// code reached from main calls, and that no interface the binary uses names.
// The linker drops such a method unless some reachable code looks a method up
// by a name it cannot see at link time, which keeps every exported method of
// every type that is converted to an interface.
//
// They are rarely used methods, of two standard packages that are in the
// binary through other dependencies: a dependency that starts calling one is
// the one honest way to make it reachable, and then another is the sentinel.
// Two packages, so that one dependency changing does not blind the check: any
// one of them present fails it.
var prunedMethods = []string{
	"bytes.(*Buffer).UnreadRune",
	"math/big.(*Int).ModSqrt",
}

// Test_gostack_binaryPrunesMethods enforces the rule of docs/standards.md that
// reflection names a method with a constant. A call of reflect's MethodByName,
// Method or Value.Method with a name the linker cannot see keeps every exported
// method in the binary: 2.7 MB at the time of writing, and nothing in the
// build says so. This reads the symbols of the binary TestMain built, which is
// built the way make build builds it, minus the version.
func Test_gostack_binaryPrunesMethods(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "go", "tool", "nm", binary).CombinedOutput()
	require.NoError(t, err, "go tool nm %s:\n%s", binary, out)

	symbols := map[string]bool{}

	for line := range strings.Lines(string(out)) {
		// "address type name"
		if fields := strings.Fields(line); len(fields) >= 3 {
			symbols[fields[2]] = true
		}
	}

	require.True(t, symbols[mainSymbol],
		"the binary has no symbol %s, so it was built stripped and its symbols cannot be checked; "+
			"build it without -s", mainSymbol)

	var kept []string

	for _, name := range prunedMethods {
		if symbols[name] {
			kept = append(kept, name)
		}
	}

	require.Empty(t, kept,
		"the linker kept %v, which no code calls: some code in the binary looks up a method by a name that "+
			"is not a constant (reflect Type.MethodByName, Type.Method(i) or Value.Method, or a package that "+
			"does, such as html/template or text/template), so it keeps every exported method and the binary "+
			"grows by megabytes. Name methods with a constant, as the generated OperationMethod does "+
			"(docs/standards.md, \"Reflection names a method with a constant\"). go build -ldflags=-dumpdep "+
			"prints the link graph to stderr: the caller is the function, not a type: line, marked "+
			"<ReflectMethod>, that is not there on main",
		kept)
}
