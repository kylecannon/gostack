package awsjson

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type wideEntry struct {
	ID    *string `json:"Id"`
	Body  *string `json:"Body"`
	Delay *int32  `json:"Delay"`
}

// stampedWide decodes itself, as a generated structure with timestamp members
// does, and holds a list.
type stampedWide struct {
	Entries []wideEntry    `json:"Entries"`
	At      *protocol.Time `json:"At"`
}

func (s *stampedWide) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	type plain stampedWide

	return json.UnmarshalDecode(dec, (*plain)(s)) //nolint:wrapcheck // the decoder's own error
}

// wideList decodes itself and is not a structure.
type wideList []wideEntry

func (l *wideList) UnmarshalJSON(data []byte) error {
	return json.Unmarshal(data, (*[]wideEntry)(l)) //nolint:wrapcheck // the decoder's own error
}

// Test_Plan_UnmarshalerIsNotFree: a body of empty entries under a type that
// decodes itself is charged like any other: through its fields for a
// structure, as untyped values otherwise. Only the timestamps are free.
func Test_Plan_UnmarshalerIsNotFree(t *testing.T) {
	body := `{"Entries":[{}` + strings.Repeat(`,{}`, 1<<20) + `]}`
	budget := protocol.DecodeBudget(len(body))

	_, _, err := DecodeWithin[stampedWide]([]byte(body), budget)
	assert.Equal(t, ErrTooLarge, err, "a structure with an unmarshaler is planned through its fields")

	list := `[{}` + strings.Repeat(`,{}`, 1<<20) + `]`
	_, _, err = DecodeWithin[wideList]([]byte(list), protocol.DecodeBudget(len(list)))
	assert.Equal(t, ErrTooLarge, err, "any other unmarshaler costs what an untyped value does")

	for leaf := range freeLeaves {
		assert.Equal(t, kindNone, planOf(leaf).kind, leaf.String())
	}
}

// Test_Plan_EveryUnmarshalerInTheAPIIsKnown: every type in internal/api, and
// in the protocol packages the generated code uses, that has an unmarshaler of
// its own is either a free leaf or a structure, which the budget plans through
// its fields. A new one of any other kind fails here until it is planned.
func Test_Plan_EveryUnmarshalerInTheAPIIsKnown(t *testing.T) {
	free := map[string]bool{}
	for leaf := range freeLeaves {
		free[leaf.PkgPath()[strings.LastIndex(leaf.PkgPath(), "/")+1:]+"."+leaf.Name()] = true
	}

	found := 0
	check := func(path string, entry fs.DirEntry) {
		if !entry.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			found += checkUnmarshalers(t, path, free)
		}
	}

	// Every generated package, and the protocol and awsjson packages alone.
	require.NoError(t, filepath.WalkDir("../../api", func(path string, entry fs.DirEntry, err error) error {
		if err == nil {
			check(path, entry)
		}

		return err
	}))

	for _, dir := range []string{"..", "."} {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)

		for _, entry := range entries {
			check(filepath.Join(dir, entry.Name()), entry)
		}
	}

	assert.GreaterOrEqual(t, found, len(freeLeaves), "the walk found the timestamps")
}

// checkUnmarshalers checks the unmarshalers declared in one file and returns
// how many it found.
func checkUnmarshalers(t *testing.T, path string, free map[string]bool) int {
	t.Helper()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	require.NoError(t, err)

	structs := map[string]bool{}

	for _, decl := range file.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok {
			for _, spec := range gen.Specs {
				if ts, isType := spec.(*ast.TypeSpec); isType {
					_, structs[ts.Name.Name] = ts.Type.(*ast.StructType)
				}
			}
		}
	}

	found := 0

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || !isUnmarshalerName(fn.Name.Name) {
			continue
		}

		name := receiverName(fn.Recv.List[0].Type)
		qualified := file.Name.Name + "." + name
		found++

		assert.True(t, free[qualified] || structs[name],
			"%s: %s has %s and is neither a free leaf nor a structure", path, qualified, fn.Name.Name)
	}

	return found
}

func isUnmarshalerName(name string) bool {
	return name == "UnmarshalJSON" || name == "UnmarshalJSONFrom" || name == "UnmarshalText"
}

func receiverName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}

	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}

	return reflect.TypeOf(expr).String()
}
