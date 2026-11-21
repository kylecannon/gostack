package awsjson

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"math"
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

	_, _, err := DecodeWithin[stampedWide]([]byte(body), budget, nil)
	assert.Equal(t, ErrTooLarge, err, "a structure with an unmarshaler is planned through its fields")

	list := `[{}` + strings.Repeat(`,{}`, 1<<20) + `]`
	_, _, err = DecodeWithin[wideList]([]byte(list), protocol.DecodeBudget(len(list)), nil)
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

// boundShapes holds every kind the walk charges: lists of structures and of
// pointers, maps, untyped values, nesting through a pointer, text and a
// timestamp.
type boundShapes struct {
	Entries []wideEntry           `json:"Entries"`
	Ptrs    []*wideEntry          `json:"Ptrs"`
	ByName  map[string]*wideEntry `json:"ByName"`
	Any     any                   `json:"Any"`
	Anys    []any                 `json:"Anys"`
	AnyMap  map[string]any        `json:"AnyMap"`
	Nested  *boundShapes          `json:"Nested"`
	Lists   [][]string            `json:"Lists"`
	Blob    []byte                `json:"Blob"`
	When    *protocol.Time        `json:"When"`
	Stamped *stampedWide          `json:"Stamped"`
	List    wideList              `json:"List"`
}

// FuzzStructureBound: the bound decodeCost takes without walking a body is
// never below what the walk charges for it, for any body and any of the types
// the walk plans differently.
func FuzzStructureBound(f *testing.F) {
	for _, seed := range []string{
		`{"Entries":[{},{},{}],"Ptrs":[{},{},null],"ByName":{"a":{},"b":{"Id":"x"}}}`,
		`{"Any":{"a":[1,[2,[3,{}]]],"b":{"c":"d"}},"Anys":[{},[],"x",{"k":[]}],"AnyMap":{"k":{"j":[]}}}`,
		`{"Nested":{"Nested":{"Entries":[{},{}],"Lists":[[],["a"],[]]}},"Blob":"aGk=","When":1}`,
		`{"Stamped":{"Entries":[{},{}],"At":1},"List":[{},{},{}]}`,
		`[{},{},{},{}]`, `{"a":1,"b":[[[[]]]]}`, `{"Entries":[{},{},`, `{"Any":[[[[[[`,
		`{"Entries":[{"Id":"a,b:c[d"},{}]}`, `{"ByName":{"a":{},"b\"":{}}}`,
	} {
		f.Add([]byte(seed))
	}

	types := []reflect.Type{
		reflect.TypeFor[boundShapes](), reflect.TypeFor[[]wideEntry](), reflect.TypeFor[map[string]any](),
		reflect.TypeFor[any](), reflect.TypeFor[stampedWide](), reflect.TypeFor[wideList](), reflect.TypeFor[[]*any](),
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		for _, typ := range types {
			p := planOf(typ)
			walked, _, _ := walkCost(body, p, math.MaxInt64)
			bound := decodePasses*int64(len(body)) + decodePasses*structureUnits(body)*p.perUnit

			require.GreaterOrEqual(t, bound, decodePasses*walked, "%s: %q", typ, body)
		}
	})
}

// Test_DecodeCost_TextIsNotWalked: a body whose text, not its structure,
// passes protocol.FreeDecode is charged its bound without a walk, so a large
// message costs one count of its separators, not a second reading of it as
// json; a dense body is still walked and charged what the walk finds.
func Test_DecodeCost_TextIsNotWalked(t *testing.T) {
	p := planOf(reflect.TypeFor[boundShapes]())

	large := []byte(`{"Entries":[{"Id":"a","Body":"` + strings.Repeat("m", 2<<20) + `"}]}`)
	cost, _, err := decodeCost(large, p, protocol.DecodeBudget(len(large)), protocol.FreeDecode)
	require.Nil(t, err)
	assert.Equal(t, decodePasses*(int64(len(large))+structureUnits(large)*p.perUnit), cost, "the bound, unwalked")

	dense := []byte(`{"Entries":[` + strings.Repeat(`{},`, 50_000) + `{}]}`)
	walked, largest, over := walkCost(dense, p, math.MaxInt64)
	require.False(t, over)

	cost, costLargest, err := decodeCost(dense, p, protocol.DecodeBudget(len(dense)), protocol.FreeDecode)
	require.Nil(t, err)
	assert.Equal(t, decodePasses*walked, cost, "the walk's own charge")
	assert.Equal(t, decodePasses*largest, costLargest, "and the most its list holds of it")
	assert.Positive(t, costLargest)

	_, _, err = decodeCost(dense, p, protocol.DecodeSlack/8, math.MaxInt64)
	assert.Equal(t, ErrTooLarge, err, "refused past the budget")
}

// Test_DecodeCost_TextFitsTheAdmittedRoom: what decoding an ordinary body
// charges stays within the room its body was admitted with
// (protocol.TextDecode times the body, and protocol.FreeBody), so a body that
// states its length never asks the bound for more after it is admitted.
func Test_DecodeCost_TextFitsTheAdmittedRoom(t *testing.T) {
	require.Equal(t, int64(decodePasses), int64(protocol.TextDecode), "the plan is both passes of the text")

	p := planOf(reflect.TypeFor[boundShapes]())

	for _, size := range []int{protocol.FreeBody + 1, 1 << 20, 1<<20 - 100, 4 << 20, 16<<20 - 100} {
		body := []byte(`{"Entries":[{"Id":"a","Body":"` + strings.Repeat("m", size-40) + `"}]}`)

		cost, _, err := decodeCost(body, p, protocol.DecodeBudget(len(body)), protocol.FreeDecode)
		require.Nil(t, err)
		assert.LessOrEqual(t, cost, protocol.TextDecode*int64(len(body))+protocol.FreeBody, "%d bytes", len(body))
	}
}
