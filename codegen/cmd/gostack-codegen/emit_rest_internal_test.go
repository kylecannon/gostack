package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// s3Files generates the patched S3 model, which no make gen writes yet.
func s3Files(t *testing.T) (*generator, map[string][]byte) {
	t.Helper()

	p, err := newTestLoader(t).Botocore("s3")
	require.NoError(t, err)

	g, err := newModel("s3", serviceTable["s3"], p, nil)
	require.NoError(t, err)

	files, err := g.files()
	require.NoError(t, err)

	return g, files
}

// funcs parses the generated files as one package and returns the source of
// its functions and methods by name, a method as Type.Name.
func funcs(t *testing.T, files map[string][]byte) map[string]string {
	t.Helper()

	out := map[string]string{}
	fset := token.NewFileSet()

	for name, src := range files {
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		require.NoError(t, err, name)
		require.Equal(t, "s3", f.Name.Name, name)

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}

			key := fn.Name.Name
			if fn.Recv != nil {
				recv := fn.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}

				key = recv.(*ast.Ident).Name + "." + key //nolint:forcetypeassert // generated receivers are named types
			}

			out[key] = string(src[fset.Position(fn.Pos()).Offset:fset.Position(fn.End()).Offset])
		}
	}

	return out
}

func Test_files_S3Shards(t *testing.T) {
	t.Parallel()

	_, files := s3Files(t)
	_, again := s3Files(t)

	require.Equal(t, files, again, "two runs write the same files")

	names := slices.Sorted(func(yield func(string) bool) {
		for name := range files {
			if !yield(name) {
				return
			}
		}
	})

	assert.Equal(t, []string{
		"codec_restxml_1.go", "codec_restxml_2.go", "codec_restxml_3.go", "codec_restxml_4.go",
		"dispatch_1.go", "dispatch_2.go", "errors.go", "operations.go", "types_1.go", "types_2.go",
	}, names)

	for name, src := range files {
		lines := strings.Count(string(src), "\n")
		assert.Less(t, lines, maxShardLines+200, "%s has %d lines", name, lines)
	}

	funcs(t, files) // every shard parses, in package s3
}

func Test_files_S3Codec(t *testing.T) {
	t.Parallel()

	g, files := s3Files(t)
	all := funcs(t, files)

	tests := []struct {
		name     string
		giveFunc string
		want     string
	}{
		{name: "a streamed request body is not read", giveFunc: "readPutObject", want: "in.Body = rc.Request.Body"},
		{name: "a streamed response body is copied", giveFunc: "writeGetObject", want: "restxml.WriteStream(w, env,"},
		{name: "a 204 operation", giveFunc: "writeDeleteBucket", want: "restxml.WriteResponse(w, env, 204, nil)"},
		{name: "an error in the S3 envelope", giveFunc: "writeError", want: "restxml.WriteS3Error("},
		{name: "an error with header members", giveFunc: "NoSuchKey.writeError", want: "v.encodeBindings(w.Header())"},
		{name: "routing before decoding", giveFunc: "Serve", want: "rc.Operation = op"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Contains(t, all, tt.giveFunc)
			assert.Contains(t, all[tt.giveFunc], tt.want)
		})
	}

	assert.Equal(t, "io.Reader", g.ops["PutObject"].input.payload.typ)
	assert.Equal(t, "io.ReadCloser", g.ops[opGetObject].output.payload.typ)
	assert.Equal(t, "io.Reader", g.ops["UploadPart"].input.payload.typ)
}
