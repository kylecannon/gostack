package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/codegen/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// nameServe is an emitted name that the collision model gives to an error.
	nameServe = "Serve"
	// routeSuffixed is what a shape named Route is called in Go.
	routeSuffixed = "RouteShape"
)

// shapesNamedLikeEmitted are the structure shapes of the collision model, named
// after emitted identifiers. Its two error shapes, Serve and ServeValidated,
// are named after the rest of emittedNames.
var shapesNamedLikeEmitted = []string{
	"Fixture", "Handler", "Metadata", "NewError", "NewOutput", "Operations", "Route", "Run", "Unimplemented",
}

// collisionModel is a service model of the given protocol whose shapes are
// named after the identifiers the generator emits. Its one operation takes and
// returns every one of the structure shapes, and fails with either error.
func collisionModel(protocol string) string {
	members := make([]string, 0, len(shapesNamedLikeEmitted))
	shapes := make([]string, 0, len(shapesNamedLikeEmitted))

	for _, name := range shapesNamedLikeEmitted {
		members = append(members, fmt.Sprintf("%q:{\"shape\":%q}", name, name))
		shapes = append(shapes, fmt.Sprintf(
			"%q:{\"type\":\"structure\",\"members\":{\"Value\":{\"shape\":\"String\"}}}", name))
	}

	// Serve has a member beside its message, so a rest service gives it a
	// struct of members as well as a constructor.
	shapes = append(shapes,
		`"Serve":{"type":"structure","exception":true,"error":{"httpStatusCode":409},`+
			`"members":{"Message":{"shape":"String"},"Detail":{"shape":"String"}}}`,
		`"ServeValidated":{"type":"structure","exception":true,"error":{"httpStatusCode":400,"senderFault":true},`+
			`"members":{"Message":{"shape":"String"}}}`,
	)

	return fmt.Sprintf(`{"metadata":{"protocol":%q,"targetPrefix":"Collide","jsonVersion":"1.0","apiVersion":"2020-01-01"},
		"operations":{"Do":{"name":"Do","http":{"method":"POST","requestUri":"/do"},
			"input":{"shape":"DoRequest"},"output":{"shape":"DoResponse"},
			"errors":[{"shape":"Serve"},{"shape":"ServeValidated"}]}},
		"shapes":{"String":{"type":"string"},
			"DoRequest":{"type":"structure","members":{%[2]s}},
			"DoResponse":{"type":"structure","members":{%[2]s}},
			%[3]s}}`, protocol, strings.Join(members, ","), strings.Join(shapes, ","))
}

func collisionGenerator(t *testing.T, protocol string) *generator {
	t.Helper()

	svc, err := spec.ParseBytes([]byte(collisionModel(protocol)))
	require.NoError(t, err)

	g, err := newModel("collide", serviceOptions{}, svc, nil)
	require.NoError(t, err)

	return g
}

// declaredNames parses the generated files as one package and returns the
// exported identifiers it declares at package level: functions without a
// receiver, types, variables and constants.
func declaredNames(t *testing.T, files map[string][]byte) map[string]bool {
	t.Helper()

	out := map[string]bool{}
	fset := token.NewFileSet()

	for name, src := range files {
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		require.NoError(t, err, name)

		for _, decl := range f.Decls {
			for _, id := range declaredIdents(decl) {
				if id.IsExported() {
					out[id.Name] = true
				}
			}
		}
	}

	return out
}

// declaredIdents are the names a top-level declaration introduces. A method
// introduces none, as it is not at package level.
func declaredIdents(decl ast.Decl) []*ast.Ident {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv == nil {
			return []*ast.Ident{d.Name}
		}
	case *ast.GenDecl:
		var ids []*ast.Ident

		for _, s := range d.Specs {
			switch s := s.(type) {
			case *ast.TypeSpec:
				ids = append(ids, s.Name)
			case *ast.ValueSpec:
				ids = append(ids, s.Names...)
			default:
			}
		}

		return ids
	default:
	}

	return nil
}

// collisionFiles generates the files of a protocol's collision model, and the
// fixture file of the fixture harness as well.
func collisionFiles(t *testing.T, protocol string) (*generator, map[string][]byte) {
	t.Helper()

	g := collisionGenerator(t, protocol)

	files, err := g.files()
	require.NoError(t, err)

	fixture, err := g.render(g.fixtureFile())
	require.NoError(t, err)

	files["fixture.go"] = fixture

	return g, files
}

func Test_shapeName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		giveShape string
		want      string
	}{
		{giveShape: "Queue", want: "Queue"},
		{giveShape: "queueAttribute", want: "QueueAttribute"},
		{giveShape: "Route", want: routeSuffixed},
		{giveShape: "Handler", want: "HandlerShape"},
		{giveShape: "serve", want: "ServeShape"},
		{giveShape: routeSuffixed, want: routeSuffixed},
		{giveShape: "Router", want: "Router"},
	}

	for _, tt := range tests {
		t.Run(tt.giveShape, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, shapeName(tt.giveShape))
		})
	}

	assert.Equal(t, "NewRouteShape", constructorName("Route"))
	assert.Equal(t, "NewQueueDoesNotExist", constructorName("QueueDoesNotExist"))
}

// Test_emittedNames_SuffixIsFree checks that a suffixed name is never itself
// one of the emitted names, which would need a second fallback.
func Test_emittedNames_SuffixIsFree(t *testing.T) {
	t.Parallel()

	for name := range emittedNames {
		assert.False(t, emittedNames[name+shapeSuffix], "%s%s is taken", name, shapeSuffix)
		assert.False(t, strings.HasSuffix(name, shapeSuffix), "%s already ends in %s", name, shapeSuffix)
	}
}

// Test_emittedNames_AreTheDeclaredOnes keeps the set from drifting. Whatever a
// protocol's emitters declare at package level, once the shapes' types and the
// error constructors are taken away, is in the set; and every name in the set
// is declared by some protocol.
func Test_emittedNames_AreTheDeclaredOnes(t *testing.T) {
	t.Parallel()

	// The rest protocols declare Route and the others ServeValidated, so the
	// set is checked against what the four declare together.
	union := map[string]bool{}

	for _, protocol := range []string{protoJSON, protoQuery, protoRestJSON, protoRestXML} {
		g, files := collisionFiles(t, protocol)
		own := declaredNames(t, files)

		for name := range own {
			union[name] = true
		}

		for _, st := range g.sortedStructs() {
			delete(own, st.goName)
		}

		for _, ex := range g.exceptions {
			assert.True(t, own[constructorName(ex.shape)], "%s: %s is declared", protocol, constructorName(ex.shape))
			delete(own, constructorName(ex.shape))
		}

		for name := range own {
			assert.True(t, emittedNames[name], "%s: %s is declared by an emitter but is not in emittedNames", protocol, name)
		}
	}

	for name := range emittedNames {
		assert.True(t, union[name], "emittedNames lists %s, which no emitter declares", name)
	}
}

func Test_newModel_ShapeNamedLikeEmitted(t *testing.T) {
	t.Parallel()

	for _, protocol := range []string{protoJSON, protoQuery, protoRestJSON, protoRestXML} {
		t.Run(protocol, func(t *testing.T) {
			t.Parallel()

			g, files := collisionFiles(t, protocol)

			for _, name := range shapesNamedLikeEmitted {
				assert.Contains(t, g.structs, name+shapeSuffix, "the type of shape %s", name)
				assert.NotContains(t, g.structs, name, "shape %s keeps no type of its own name", name)
			}

			assert.Equal(t, []string{"NewServeShape", "NewServeValidatedShape"},
				[]string{constructorName(g.exceptions[0].shape), constructorName(g.exceptions[1].shape)})

			// Handler is still the interface, and the shape beside it is a struct.
			all := declaredNames(t, files)
			assert.True(t, all["Handler"])
			assert.True(t, all["HandlerShape"])
		})
	}
}

func Test_newModel_ShapeSuffixIsTaken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		giveModel string
		wantErr   string
	}{
		{
			name: "a reserved shape beside the shape named as its suffixed name",
			giveModel: `{"metadata":{"protocol":"rest-json"},
				"operations":{"Op":{"name":"Op","http":{"method":"POST","requestUri":"/"},"input":{"shape":"OpRequest"}}},
				"shapes":{
					"OpRequest":{"type":"structure","members":{"A":{"shape":"Route"},"B":{"shape":"RouteShape"}}},
					"Route":{"type":"structure","members":{}},
					"RouteShape":{"type":"structure","members":{}}}}`,
			wantErr: "would both be the Go type RouteShape",
		},
		{
			name: "an error named like a reserved name beside the shape named as its suffixed name",
			giveModel: `{"metadata":{"protocol":"rest-json"},
				"operations":{"Op":{"name":"Op","http":{"method":"POST","requestUri":"/"},"input":{"shape":"OpRequest"}}},
				"shapes":{
					"OpRequest":{"type":"structure","members":{"A":{"shape":"RouteShape"}}},
					"RouteShape":{"type":"structure","members":{}},
					"Route":{"type":"structure","exception":true,"members":{"Detail":{"shape":"String"}}},
					"String":{"type":"string"}}}`,
			wantErr: "the Go type RouteShape of its members is another shape's as well",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			svc, err := spec.ParseBytes([]byte(tt.giveModel))
			require.NoError(t, err)

			_, err = newModel("svc", serviceOptions{}, svc, nil)
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// roundTripTest runs in the generated package of a collision model. It sends
// every renamed shape through the codec and back, and has the handler fail with
// the error that is named like Serve.
const roundTripTest = `package collide

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
)

type echo struct{ Unimplemented }

func (echo) Do(_ context.Context, in *DoInput) (*DoOutput, error) {
	if in.Route != nil && in.Route.Value != nil && *in.Route.Value == "fail" {
		detail := "the detail"

		return nil, (&ServeShape{Detail: &detail}).Err("it failed")
	}

	return &DoOutput{Route: in.Route, Handler: in.Handler, Fixture: in.Fixture}, nil
}

func call(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest("POST", "/do", strings.NewReader(body))
	rec := httptest.NewRecorder()

	Serve(echo{}, rec, &request.Context{Service: "collide", Protocol: protocol.%[1]s, RequestID: "req-1", Request: req})

	return rec
}

func TestRoundTrip(t *testing.T) {
	rec := call(t, %[2]q)
	if rec.Code != 200 {
		t.Fatalf("status %%d: %%s", rec.Code, rec.Body)
	}

	for _, want := range %#[3]v {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the response has no %%s: %%s", want, rec.Body)
		}
	}
}

func TestWireErrorCode(t *testing.T) {
	rec := call(t, %[4]q)
	if rec.Code != 409 {
		t.Fatalf("status %%d: %%s", rec.Code, rec.Body)
	}

	for _, want := range %#[5]v {
		if !strings.Contains(rec.Header().Get("X-Amzn-Errortype")+rec.Body.String(), want) {
			t.Errorf("the error has no %%s: %%s %%s", want, rec.Header(), rec.Body)
		}
	}
}

func TestNames(t *testing.T) {
	_ = NewServeShape("x")
	_ = NewServeValidatedShape("x")
	_ = Operations
	_ = Metadata
	_ = Unimplemented{}
	_ = RouteShape{}
	_ = HandlerShape{}
	_ = MetadataShape{}
}
`

// scratchDir makes a scratch directory under this package's testdata, which is
// inside the module so that a package generated into it can import the module's
// internal packages, and removes it when the test ends. The directory is its
// own, uniquely named, so tests running side by side never share one, and
// testdata itself is shared and never removed. A run that is killed leaves its
// scratch directory behind, which .gitignore keeps out of the tree.
func scratchDir(t *testing.T, pattern string) string {
	t.Helper()

	require.NoError(t, os.MkdirAll("testdata", 0o750))

	//nolint:usetesting // t.TempDir is outside the module, where internal packages cannot be imported
	scratch, err := os.MkdirTemp("testdata", pattern)
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, os.RemoveAll(scratch)) })

	return scratch
}

// Test_files_ShapesNamedLikeEmittedCompile writes the generated package of a
// collision model into a scratch directory under testdata, builds it, and runs
// a round trip through its codec: the Go name of a shape changes, its wire name
// does not. Its Route shape is the case apigatewayv2's model raises, so that
// service has no entry of its own in the service table.
func Test_files_ShapesNamedLikeEmittedCompile(t *testing.T) {
	t.Parallel()

	scratch := scratchDir(t, "collide-*")

	tests := []struct {
		protocol    string
		kind        string
		body        string
		want        []string
		failBody    string
		wantFailure []string
	}{
		{
			protocol: protoRestJSON, kind: "KindRestJSON",
			body:        `{"Route":{"Value":"r"},"Handler":{"Value":"h"},"Fixture":{"Value":"f"}}`,
			want:        []string{`"Route":{"Value":"r"}`, `"Handler":{"Value":"h"}`, `"Fixture":{"Value":"f"}`},
			failBody:    `{"Route":{"Value":"fail"}}`,
			wantFailure: []string{nameServe, `"Detail":"the detail"`, "it failed"},
		},
		{
			protocol: protoRestXML, kind: "KindRestXML",
			body: "<DoRequest><Route><Value>r</Value></Route><Handler><Value>h</Value></Handler>" +
				"<Fixture><Value>f</Value></Fixture></DoRequest>",
			want: []string{
				"<Route><Value>r</Value></Route>", "<Handler><Value>h</Value></Handler>",
				"<Fixture><Value>f</Value></Fixture>",
			},
			failBody:    "<DoRequest><Route><Value>fail</Value></Route></DoRequest>",
			wantFailure: []string{"<Code>Serve</Code>", "<Detail>the detail</Detail>", "it failed"},
		},
	}

	pkgs := make([]string, 0, len(tests))

	for _, tt := range tests {
		_, files := collisionFiles(t, tt.protocol)
		dir := filepath.Join(scratch, strings.ReplaceAll(tt.protocol, "-", ""))

		require.NoError(t, os.MkdirAll(dir, 0o750))

		for name, src := range files {
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), src, 0o600))
		}

		test := fmt.Sprintf(roundTripTest, tt.kind, tt.body, tt.want, tt.failBody, tt.wantFailure)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "round_test.go"), []byte(test), 0o600))

		pkgs = append(pkgs, "./"+filepath.ToSlash(dir))
	}

	args := slices.Concat([]string{"test", "-count=1"}, pkgs)
	cmd := exec.CommandContext(t.Context(), "go", args...)
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "GIT_") })

	var out bytes.Buffer

	cmd.Stdout, cmd.Stderr = &out, &out

	require.NoError(t, cmd.Run(), out.String())
}
