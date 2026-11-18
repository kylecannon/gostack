package main

import (
	"net/http"
	"testing"

	"github.com/kylecannon/gostack/codegen/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// rawS3 is S3's model as botocore ships it, without LocalStack's patches.
	rawS3 = "botocore/s3/2006-03-01/service-2.json"

	bucketLabel = "{Bucket}"
)

func newTestLoader(t *testing.T) *spec.Loader {
	t.Helper()

	loader, err := spec.NewLoader("../../../specs")
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, loader.Close()) })

	return loader
}

// buildRest builds the model of a rest service the generator has no table
// entry for yet.
func buildRest(t *testing.T, name string, load func() (*spec.Service, error)) *generator {
	t.Helper()

	svc, err := load()
	require.NoError(t, err)

	g, err := newModel(name, serviceOptions{}, svc, nil)
	require.NoError(t, err)

	return g
}

// census is what a model holds, counted over its structs, each once, and its
// operations.
type census struct {
	structs      int
	located      map[string]int
	payloads     int
	flattened    int
	eventStreams int
	streaming    int
	greedyOps    int
	queryOps     int
	codes        map[int]int
}

func takeCensus(g *generator) census {
	c := census{located: map[string]int{}, codes: map[int]int{}}

	for _, st := range g.sortedStructs() {
		if st.shape == nil {
			continue // the empty struct of an operation without that root
		}

		c.structs++

		if st.payload != nil {
			c.payloads++
		}

		if st.shape.EventStream {
			c.eventStreams++
		}

		for _, f := range st.fields {
			r := f.pair.p

			if r.binding != "" {
				c.located[r.binding]++
			}

			if r.flat && r.shape.Type == spec.TypeList {
				c.flattened++
			}

			if r.streaming {
				c.streaming++
			}
		}
	}

	for _, name := range g.opNames {
		b := g.ops[name].http

		for _, seg := range b.path {
			if seg.greedy {
				c.greedyOps++
			}
		}

		if len(b.query) > 0 {
			c.queryOps++
		}

		c.codes[b.code]++
	}

	return c
}

func Test_newModel_Rest(t *testing.T) {
	t.Parallel()

	loader := newTestLoader(t)

	tests := []struct {
		name             string
		giveService      string
		giveLoad         func() (*spec.Service, error)
		wantOps          int
		wantStructs      int
		wantLocated      map[string]int
		wantPayloads     int
		wantFlattened    int
		wantEventStreams int
		wantStreaming    int
		wantGreedyOps    int
		wantQueryOps     int
		wantCodes        map[int]int
	}{
		{
			name:        "s3 as botocore ships it",
			giveService: "s3",
			giveLoad:    func() (*spec.Service, error) { return loader.File(rawS3) },
			wantOps:     111,
			wantStructs: 343,
			wantLocated: map[string]int{
				spec.LocationHeader: 623, spec.LocationQuery: 84, spec.LocationURI: 133, spec.LocationHeaders: 6,
			},
			wantPayloads:     60,
			wantFlattened:    53,
			wantEventStreams: 1,
			wantStreaming:    5,
			wantGreedyOps:    26,
			wantQueryOps:     94,
			wantCodes:        map[int]int{0: 92, 204: 19},
		},
		{
			// PostObject, statusCode members and more headers come from
			// spec-patches.json. This is the model make gen will read.
			name:        "s3 with LocalStack's patches",
			giveService: "s3",
			giveLoad:    func() (*spec.Service, error) { return loader.Botocore("s3") },
			wantOps:     112,
			wantStructs: 345,
			wantLocated: map[string]int{
				spec.LocationHeader: 642, spec.LocationQuery: 84, spec.LocationURI: 134, spec.LocationHeaders: 6,
				spec.LocationStatusCode: 4,
			},
			wantPayloads:     62,
			wantFlattened:    53,
			wantEventStreams: 1,
			wantStreaming:    6,
			wantGreedyOps:    26,
			wantQueryOps:     94,
			wantCodes:        map[int]int{0: 88, 204: 24},
		},
		{
			name:        "lambda, a rest-json service",
			giveService: "lambda",
			giveLoad:    func() (*spec.Service, error) { return loader.Botocore("lambda") },
			wantOps:     85,
			wantStructs: 261,
			wantLocated: map[string]int{
				spec.LocationHeader: 15, spec.LocationQuery: 79, spec.LocationURI: 84, spec.LocationStatusCode: 4,
			},
			wantPayloads:     8,
			wantFlattened:    0,
			wantEventStreams: 1,
			wantStreaming:    1,
			wantGreedyOps:    0,
			wantQueryOps:     2,
			wantCodes:        map[int]int{200: 56, 201: 8, 202: 9, 204: 12},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			g := buildRest(t, tt.giveService, tt.giveLoad)
			got := takeCensus(g)

			assert.Len(t, g.opNames, tt.wantOps, "operations")
			assert.Equal(t, tt.wantStructs, got.structs, "structure shapes")
			assert.Equal(t, tt.wantLocated, got.located, "members by location")
			assert.Equal(t, tt.wantPayloads, got.payloads, "shapes with a payload member")
			assert.Equal(t, tt.wantFlattened, got.flattened, "flattened list members")
			assert.Equal(t, tt.wantEventStreams, got.eventStreams, "event streams")
			assert.Equal(t, tt.wantStreaming, got.streaming, "streaming members")
			assert.Equal(t, tt.wantGreedyOps, got.greedyOps, "operations with a greedy label")
			assert.Equal(t, tt.wantQueryOps, got.queryOps, "operations with a literal query")
			assert.Equal(t, tt.wantCodes, got.codes, "operations by response code")
		})
	}
}

func Test_newModel_SharedRoots(t *testing.T) {
	t.Parallel()

	loader := newTestLoader(t)
	s3 := buildRest(t, "s3", func() (*spec.Service, error) { return loader.File(rawS3) })
	lambda := buildRest(t, "lambda", func() (*spec.Service, error) { return loader.Botocore("lambda") })

	t.Run("operations that share an input share its type", func(t *testing.T) {
		t.Parallel()

		old, current := s3.ops["GetBucketNotification"], s3.ops["GetBucketNotificationConfiguration"]
		require.Same(t, current.input, old.input)
		assert.Equal(t, "GetBucketNotificationConfigurationRequest", current.input.goName)
		assert.Equal(t,
			"GetBucketNotification(ctx context.Context, in *GetBucketNotificationConfigurationRequest) "+
				"(*GetBucketNotificationOutput, error)",
			handlerSignature(old))
	})

	t.Run("operations that share an output share its type", func(t *testing.T) {
		t.Parallel()

		create := lambda.ops["CreateFunction"]
		require.Same(t, create.output, lambda.ops["GetFunctionConfiguration"].output)
		assert.Equal(t, "FunctionConfiguration", create.output.goName)
		assert.Equal(t, "CreateFunctionInput", create.input.goName)
	})

	t.Run("a root of one operation is named for it", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, "GetObjectInput", s3.ops["GetObject"].input.goName)
		assert.Equal(t, "GetObjectOutput", s3.ops["GetObject"].output.goName)
		assert.Equal(t, "DeleteBucketOutput", s3.ops["DeleteBucket"].output.goName, "an absent output")
	})
}

func Test_newModel_HTTPBinding(t *testing.T) {
	t.Parallel()

	g := buildRest(t, "s3", func() (*spec.Service, error) { return newTestLoader(t).File(rawS3) })

	tests := []struct {
		name       string
		giveOp     string
		wantMethod string
		wantPath   []string
		wantQuery  []spec.QueryLiteral
		wantCode   int
	}{
		{
			name:       "the root",
			giveOp:     "ListBuckets",
			wantMethod: http.MethodGet,
			wantCode:   0,
		},
		{
			name:       "a greedy key label",
			giveOp:     "GetObject",
			wantMethod: http.MethodGet,
			wantPath:   []string{bucketLabel, "{Key+}"},
			wantCode:   0,
		},
		{
			name:       "a literal query with a value",
			giveOp:     "ListObjectsV2",
			wantMethod: http.MethodGet,
			wantPath:   []string{bucketLabel},
			wantQuery:  []spec.QueryLiteral{{Key: "list-type", Value: "2"}},
			wantCode:   0,
		},
		{
			name:       "a bare key and a key with a value",
			giveOp:     "SelectObjectContent",
			wantMethod: "POST",
			wantPath:   []string{bucketLabel, "{Key+}"},
			wantQuery:  []spec.QueryLiteral{{Key: "select"}, {Key: "select-type", Value: "2"}},
			wantCode:   0,
		},
		{
			name:       "a literal path and a response code",
			giveOp:     "DeleteBucketTagging",
			wantMethod: "DELETE",
			wantPath:   []string{bucketLabel},
			wantQuery:  []spec.QueryLiteral{{Key: "tagging"}},
			wantCode:   204,
		},
		{
			name:       "a literal segment",
			giveOp:     "WriteGetObjectResponse",
			wantMethod: "POST",
			wantPath:   []string{"WriteGetObjectResponse"},
			wantCode:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			op := g.ops[tt.giveOp]
			require.NotNil(t, op)

			b := op.http
			assert.Equal(t, tt.wantMethod, b.method)
			assert.Equal(t, tt.wantPath, renderPath(t, op.input, b.path))
			assert.Equal(t, tt.wantQuery, b.query)
			assert.Equal(t, tt.wantCode, b.code)
		})
	}
}

// renderPath writes path segments back in request URI form, checking that each
// label is a uri member of the input.
func renderPath(t *testing.T, input *structType, path []pathSegment) []string {
	t.Helper()

	var out []string

	for _, seg := range path {
		if seg.label == nil {
			out = append(out, seg.literal)

			continue
		}

		require.Contains(t, input.fields, seg.label)
		require.Equal(t, spec.LocationURI, seg.label.pair.p.binding)

		greedy := ""
		if seg.greedy {
			greedy = "+"
		}

		out = append(out, "{"+wireName(seg.label)+greedy+"}")
	}

	return out
}

func Test_bindHTTP_Errors(t *testing.T) {
	t.Parallel()

	input := &structType{goName: "In", fields: []*field{
		{member: "Bucket", pair: pair{p: ref{binding: spec.LocationURI, loc: "Bucket"}}},
		{member: "Prefix", pair: pair{p: ref{binding: spec.LocationQuery, loc: "prefix"}}},
	}}

	tests := []struct {
		name    string
		giveURI string
		wantErr string
	}{
		{name: "an unterminated label", giveURI: "/{Bucket", wantErr: "malformed label {Bucket"},
		{name: "a label inside a segment", giveURI: "/b-{Bucket}", wantErr: "a label must be a whole path segment"},
		{name: "an empty label", giveURI: "/{}", wantErr: "malformed label {}"},
		{name: "a label with no member", giveURI: "/{Key+}", wantErr: "no uri member of In is named Key"},
		{name: "a label naming a non-uri member", giveURI: "/{prefix}", wantErr: "no uri member of In is named prefix"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := bindHTTP(&spec.HTTP{Method: http.MethodGet, RequestURI: tt.giveURI}, input)
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func Test_newModel_RestTraits(t *testing.T) {
	t.Parallel()

	g := buildRest(t, "s3", func() (*spec.Service, error) { return newTestLoader(t).File(rawS3) })

	fieldOf := func(t *testing.T, goName, member string) *field {
		t.Helper()

		st := g.structs[goName]
		require.NotNil(t, st, goName)

		for _, f := range st.fields {
			if f.member == member {
				return f
			}
		}

		require.Failf(t, "no such member", "%s.%s", goName, member)

		return nil
	}

	t.Run("a streaming payload", func(t *testing.T) {
		t.Parallel()

		body := fieldOf(t, "PutObjectInput", "Body")
		assert.Same(t, body, g.structs["PutObjectInput"].payload)
		assert.True(t, body.pair.p.streaming)
		assert.Empty(t, body.pair.p.binding, "a payload is in the body")
	})

	t.Run("a structure payload with a namespace", func(t *testing.T) {
		t.Parallel()

		acl := fieldOf(t, "PutBucketAclInput", "AccessControlPolicy")
		assert.Same(t, acl, g.structs["PutBucketAclInput"].payload)
		assert.Equal(t, &spec.XMLNamespace{URI: "http://s3.amazonaws.com/doc/2006-03-01/"}, acl.pair.p.xmlns)
		assert.Equal(t, "AccessControlPolicy", acl.pair.p.loc)
	})

	t.Run("a header, a header map and a query member", func(t *testing.T) {
		t.Parallel()

		since := fieldOf(t, "GetObjectInput", "IfModifiedSince")
		assert.Equal(t, spec.LocationHeader, since.pair.p.binding)
		assert.Equal(t, "If-Modified-Since", since.pair.p.loc)

		meta := fieldOf(t, "PutObjectInput", "Metadata")
		assert.Equal(t, spec.LocationHeaders, meta.pair.p.binding)
		assert.Equal(t, "x-amz-meta-", meta.pair.p.loc)

		expires := fieldOf(t, "GetObjectInput", "ResponseExpires")
		assert.Equal(t, spec.LocationQuery, expires.pair.p.binding)
		assert.Equal(t, "rfc822", expires.pair.p.tsfmt, "the format of the shape")
	})

	t.Run("an attribute in a namespace the shape declares", func(t *testing.T) {
		t.Parallel()

		typ := fieldOf(t, "Grantee", "Type")
		assert.True(t, typ.pair.p.xmlAttr)
		assert.Equal(t, "xsi:type", typ.pair.p.loc)

		grantee := fieldOf(t, "Grant", "Grantee")
		assert.Equal(t, &spec.XMLNamespace{Prefix: "xsi", URI: "http://www.w3.org/2001/XMLSchema-instance"},
			grantee.pair.p.xmlns, "the namespace of the shape")
	})

	t.Run("a flattened list", func(t *testing.T) {
		t.Parallel()

		contents := fieldOf(t, "ListObjectsV2Output", "Contents")
		assert.True(t, contents.pair.p.flat)
	})

	t.Run("an event stream and its payload", func(t *testing.T) {
		t.Parallel()

		payload := fieldOf(t, "SelectObjectContentOutput", "Payload")
		assert.Same(t, payload, g.structs["SelectObjectContentOutput"].payload)
		assert.True(t, payload.pair.p.shape.EventStream)
		assert.True(t, fieldOf(t, "RecordsEvent", "Payload").pair.p.eventPayload)
	})

	t.Run("a host label", func(t *testing.T) {
		t.Parallel()

		route := fieldOf(t, "WriteGetObjectResponseInput", "RequestRoute")
		assert.True(t, route.pair.p.hostLabel)
		assert.Equal(t, spec.LocationHeader, route.pair.p.binding)
	})
}

func Test_newModel_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		giveModel string
		wantErr   string
	}{
		{
			name:      "an unsupported protocol",
			giveModel: `{"metadata":{"protocol":"ec2"},"operations":{},"shapes":{}}`,
			wantErr:   `protocol "ec2" is not supported yet`,
		},
		{
			name: "a payload member that does not exist",
			giveModel: `{"metadata":{"protocol":"rest-xml"},
				"operations":{"Op":{"name":"Op","http":{"method":"GET","requestUri":"/"},"input":{"shape":"In"}}},
				"shapes":{"In":{"type":"structure","payload":"Body","members":{}}}}`,
			wantErr: "shape In has no payload member Body",
		},
		{
			name: "two shapes that would be one Go type",
			giveModel: `{"metadata":{"protocol":"rest-json"},
				"operations":{"Op":{"name":"Op","http":{"method":"GET","requestUri":"/"},"input":{"shape":"OpRequest"}}},
				"shapes":{
					"OpRequest":{"type":"structure","members":{"Body":{"shape":"OpInput"}}},
					"OpInput":{"type":"structure","members":{}}}}`,
			wantErr: "shape OpInput and another shape would both be the Go type OpInput",
		},
		{
			name: "a shape named like the absent root of another operation",
			giveModel: `{"metadata":{"protocol":"rest-json"},
				"operations":{
					"A":{"name":"A","http":{"method":"GET","requestUri":"/"},"input":{"shape":"AReq"}},
					"B":{"name":"B","http":{"method":"GET","requestUri":"/"}}},
				"shapes":{
					"AReq":{"type":"structure","members":{"Body":{"shape":"BInput"}}},
					"BInput":{"type":"structure","members":{}}}}`,
			wantErr: "the Go type BInput of an absent root is a shape's as well",
		},
		{
			name: "a label with no uri member",
			giveModel: `{"metadata":{"protocol":"rest-json"},
				"operations":{"Op":{"name":"Op","http":{"method":"GET","requestUri":"/{Id}"},"input":{"shape":"In"}}},
				"shapes":{"In":{"type":"structure","members":{}}}}`,
			wantErr: "no uri member of OpInput is named Id",
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
