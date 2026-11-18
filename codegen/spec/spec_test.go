package spec_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/kylecannon/gostack/codegen/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	opAdd     = "add"
	opRemove  = "remove"
	opReplace = "replace"
	opTest    = "test"

	pathMemberA = "/m/A"

	objectURI = "/{Bucket}/{Key+}"
)

func Test_ApplyPatches(t *testing.T) {
	t.Parallel()

	const doc = `{"a":{"b":[1,2,3],"big":9223372036854775807},"c":"x"}`

	tests := []struct {
		name    string
		giveOps []spec.Patch
		want    string
	}{
		{
			name:    "add a member",
			giveOps: []spec.Patch{{Op: opAdd, Path: "/a/d", Value: []byte(`{"e":true}`)}},
			want:    `{"a":{"b":[1,2,3],"big":9223372036854775807,"d":{"e":true}},"c":"x"}`,
		},
		{
			name: "add into an array at an index and at the end",
			giveOps: []spec.Patch{
				{Op: opAdd, Path: "/a/b/1", Value: []byte(`9`)},
				{Op: opAdd, Path: "/a/b/-", Value: []byte(`7`)},
			},
			want: `{"a":{"b":[1,9,2,3,7],"big":9223372036854775807},"c":"x"}`,
		},
		{
			name:    "remove a member and an array element",
			giveOps: []spec.Patch{{Op: opRemove, Path: "/c"}, {Op: opRemove, Path: "/a/b/0"}},
			want:    `{"a":{"b":[2,3],"big":9223372036854775807}}`,
		},
		{
			name:    "replace a value",
			giveOps: []spec.Patch{{Op: opReplace, Path: "/c", Value: []byte(`"y"`)}},
			want:    `{"a":{"b":[1,2,3],"big":9223372036854775807},"c":"y"}`,
		},
		{
			name:    "move and copy",
			giveOps: []spec.Patch{{Op: "move", From: "/c", Path: "/a/c"}, {Op: "copy", From: "/a/b", Path: "/b2"}},
			want:    `{"a":{"b":[1,2,3],"big":9223372036854775807,"c":"x"},"b2":[1,2,3]}`,
		},
		{
			name:    "test that passes changes nothing",
			giveOps: []spec.Patch{{Op: opTest, Path: "/c", Value: []byte(`"x"`)}},
			want:    doc,
		},
		{
			name:    "pointer escapes",
			giveOps: []spec.Patch{{Op: opAdd, Path: "/a~1b~0c", Value: []byte(`1`)}},
			want:    `{"a":{"b":[1,2,3],"big":9223372036854775807},"a/b~c":1,"c":"x"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := spec.ApplyPatches([]byte(doc), tt.giveOps)
			require.NoError(t, err)
			assert.JSONEq(t, tt.want, string(got))
			assert.Contains(t, string(got), "9223372036854775807", "large numbers survive a patch")
		})
	}
}

// Test_ApplyPatches_KeyOrder checks the order a patched object keeps, which is
// the order a structure's members go on the wire: Python dict order, as
// LocalStack's jsonpatch leaves it.
func Test_ApplyPatches_KeyOrder(t *testing.T) {
	t.Parallel()

	const doc = `{"m":{"Z":1,"A":2,"C":3,"E":[]}}`

	tests := []struct {
		name    string
		giveOps []spec.Patch
		want    string
	}{
		{name: "no patch keeps the order written", want: doc},
		{
			name:    "remove then add moves a member last",
			giveOps: []spec.Patch{{Op: opRemove, Path: pathMemberA}, {Op: opAdd, Path: pathMemberA, Value: []byte(`2`)}},
			want:    `{"m":{"Z":1,"C":3,"E":[],"A":2}}`,
		},
		{
			name: "add of a new member goes last, of an existing one keeps its place",
			giveOps: []spec.Patch{
				{Op: opAdd, Path: "/m/B", Value: []byte(`5`)},
				{Op: opAdd, Path: "/m/Z", Value: []byte(`9`)},
			},
			want: `{"m":{"Z":9,"A":2,"C":3,"E":[],"B":5}}`,
		},
		{
			name:    "replace keeps its place",
			giveOps: []spec.Patch{{Op: opReplace, Path: pathMemberA, Value: []byte(`{"y":1,"x":2}`)}},
			want:    `{"m":{"Z":1,"A":{"y":1,"x":2},"C":3,"E":[]}}`,
		},
		{
			name:    "test ignores key order",
			giveOps: []spec.Patch{{Op: opTest, Path: "/m", Value: []byte(`{"E":[],"C":3,"A":2,"Z":1}`)}},
			want:    doc,
		},
		{
			name:    "copy of an object keeps its members and their order",
			giveOps: []spec.Patch{{Op: "copy", From: "/m", Path: "/n"}},
			want:    `{"m":{"Z":1,"A":2,"C":3,"E":[]},"n":{"Z":1,"A":2,"C":3,"E":[]}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := spec.ApplyPatches([]byte(doc), tt.giveOps)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
		})
	}
}

func Test_ApplyPatches_Errors(t *testing.T) {
	t.Parallel()

	const doc = `{"a":[1],"c":"x"}`

	tests := []struct {
		name   string
		giveOp spec.Patch
	}{
		{name: "remove a missing member", giveOp: spec.Patch{Op: opRemove, Path: "/nope"}},
		{name: "replace a missing member", giveOp: spec.Patch{Op: opReplace, Path: "/nope", Value: []byte(`1`)}},
		{name: "add under a missing parent", giveOp: spec.Patch{Op: opAdd, Path: "/nope/x", Value: []byte(`1`)}},
		{name: "array index past the end", giveOp: spec.Patch{Op: opAdd, Path: "/a/5", Value: []byte(`1`)}},
		{name: "test that fails", giveOp: spec.Patch{Op: opTest, Path: "/c", Value: []byte(`"y"`)}},
		{name: "unknown operation", giveOp: spec.Patch{Op: "frob", Path: "/c"}},
		{name: "pointer without a slash", giveOp: spec.Patch{Op: opRemove, Path: "c"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := spec.ApplyPatches([]byte(doc), []spec.Patch{tt.giveOp})
			require.Error(t, err)
		})
	}
}

func Test_Members_KeepSpecOrder(t *testing.T) {
	t.Parallel()

	svc, err := spec.ParseBytes([]byte(`{"shapes":{"S":{"type":"structure","required":["Z"],"members":{
		"Zed":{"shape":"A"},"Alpha":{"shape":"A","locationName":"a"},"Mid":{"shape":"A","flattened":true}}}}}`))
	require.NoError(t, err)

	members := svc.Shapes["S"].Members
	require.Len(t, members, 3)
	assert.Equal(t, "Zed", members[0].Name)
	assert.Equal(t, "Alpha", members[1].Name)
	assert.Equal(t, "a", members[1].Ref.LocationName)
	assert.True(t, members[2].Ref.Flattened)

	got, ok := members.Lookup("Mid")
	require.True(t, ok)
	assert.Equal(t, "A", got.Ref.Shape)

	_, ok = members.Lookup("Nope")
	assert.False(t, ok)
}

func Test_HTTP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		giveURI    string
		wantPath   string
		wantQuery  []spec.QueryLiteral
		wantMethod string
		wantCode   int
	}{
		{
			name:       "a path with a greedy label and no query",
			giveURI:    objectURI,
			wantPath:   objectURI,
			wantMethod: http.MethodGet,
			wantCode:   0,
		},
		{
			name:       "a key with a value",
			giveURI:    "/{Bucket}?list-type=2",
			wantPath:   "/{Bucket}",
			wantQuery:  []spec.QueryLiteral{{Key: "list-type", Value: "2"}},
			wantMethod: http.MethodGet,
			wantCode:   0,
		},
		{
			name:       "a bare key then a key with a value, in order",
			giveURI:    objectURI + "?select&select-type=2",
			wantPath:   objectURI,
			wantQuery:  []spec.QueryLiteral{{Key: "select"}, {Key: "select-type", Value: "2"}},
			wantMethod: "POST",
			wantCode:   200,
		},
		{
			name:       "the root",
			giveURI:    "/",
			wantPath:   "/",
			wantMethod: "DELETE",
			wantCode:   204,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			svc, err := spec.ParseBytes(fmt.Appendf(nil,
				`{"operations":{"Op":{"name":"Op","http":{"method":%q,"requestUri":%q,"responseCode":%d}}}}`,
				tt.wantMethod, tt.giveURI, tt.wantCode))
			require.NoError(t, err)

			h := svc.Operations["Op"].HTTP
			require.NotNil(t, h)
			assert.Equal(t, tt.wantMethod, h.Method)
			assert.Equal(t, tt.giveURI, h.RequestURI)
			assert.Equal(t, tt.wantCode, h.ResponseCode)
			assert.Equal(t, tt.wantPath, h.Path())
			assert.Equal(t, tt.wantQuery, h.Query())
		})
	}
}

func Test_XMLNamespace(t *testing.T) {
	t.Parallel()

	svc, err := spec.ParseBytes([]byte(`{"shapes":{
		"Obj":{"type":"structure","xmlNamespace":{"prefix":"xsi","uri":"http://x"},"members":{
			"Bare":{"shape":"S","xmlNamespace":"http://bare"},
			"Plain":{"shape":"S"}}},
		"S":{"type":"string","xmlNamespace":"http://shape"}}}`))
	require.NoError(t, err)

	obj := svc.Shapes["Obj"]
	assert.Equal(t, &spec.XMLNamespace{Prefix: "xsi", URI: "http://x"}, obj.XMLNamespace, "object form")
	assert.Equal(t, &spec.XMLNamespace{URI: "http://bare"}, obj.Members[0].Ref.XMLNamespace, "bare URI on a member")
	assert.Nil(t, obj.Members[1].Ref.XMLNamespace, "absent")
	assert.Equal(t, &spec.XMLNamespace{URI: "http://shape"}, svc.Shapes["S"].XMLNamespace, "bare URI on a shape")

	_, err = spec.ParseBytes([]byte(`{"shapes":{"S":{"type":"string","xmlNamespace":7}}}`))
	require.Error(t, err)
}

func Test_Loader(t *testing.T) {
	t.Parallel()

	loader, err := spec.NewLoader("../../specs")
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, loader.Close()) })

	t.Run("botocore sqs is the json model", func(t *testing.T) {
		t.Parallel()

		svc, err := loader.Botocore("sqs")
		require.NoError(t, err)

		assert.Equal(t, "json", svc.Metadata.Protocol)
		assert.Equal(t, "AmazonSQS", svc.Metadata.TargetPrefix)
		assert.True(t, svc.Metadata.IsQueryCompatible())
		assert.Contains(t, svc.Operations, "SendMessage")
		assert.Equal(t, "QueueUrl", svc.Shapes["SendMessageRequest"].Members[0].Name, "members keep file order")
	})

	t.Run("the sqs-query model is the query one", func(t *testing.T) {
		t.Parallel()

		svc, err := loader.File("localstack/sqs-query/2012-11-05/service-2.json")
		require.NoError(t, err)

		assert.Equal(t, "query", svc.Metadata.Protocol)
		assert.False(t, svc.Metadata.IsQueryCompatible())
		assert.Equal(t, "SendMessageResult", svc.Operations["SendMessage"].Output.ResultWrapper)
	})

	t.Run("sts exceptions carry their error trait", func(t *testing.T) {
		t.Parallel()

		svc, err := loader.Botocore("sts")
		require.NoError(t, err)

		trait := svc.Shapes["IDPRejectedClaimException"].Error
		require.NotNil(t, trait)
		assert.Equal(t, "IDPRejectedClaim", trait.Code)
		assert.Equal(t, 403, trait.HTTPStatusCode)
		assert.True(t, trait.SenderFault)
	})

	t.Run("the s3 patches apply", func(t *testing.T) {
		t.Parallel()

		svc, err := loader.Botocore("s3")
		require.NoError(t, err)

		_, ok := svc.Shapes["NoSuchBucket"].Members.Lookup("BucketName")
		assert.True(t, ok, "patch /shapes/NoSuchBucket/members/BucketName adds the member")
	})

	t.Run("s3 carries the rest traits", func(t *testing.T) {
		t.Parallel()

		svc, err := loader.File("botocore/s3/2006-03-01/service-2.json")
		require.NoError(t, err)

		assert.Equal(t, "rest-xml", svc.Metadata.Protocol)

		var checksums, static, hostPrefixes, contextParams int

		for _, op := range svc.Operations {
			if op.HTTPChecksum != nil {
				checksums++
			}

			if len(op.StaticContextParams) > 0 {
				static++
			}

			if op.Endpoint != nil && op.Endpoint.HostPrefix != "" {
				hostPrefixes++
			}
		}

		for _, shape := range svc.Shapes {
			for _, m := range shape.Members {
				if m.Ref.ContextParam != nil {
					contextParams++
				}
			}
		}

		assert.Equal(t, 32, checksums, "operations with httpChecksum")
		assert.Equal(t, 75, static, "operations with staticContextParams")
		assert.Equal(t, 1, hostPrefixes, "operations with endpoint.hostPrefix")
		assert.Equal(t, 125, contextParams, "members with contextParam")

		getObject := svc.Operations["GetObject"]
		assert.Equal(t, &spec.HTTP{Method: http.MethodGet, RequestURI: objectURI}, getObject.HTTP)
		assert.Equal(t, &spec.HTTPChecksum{
			RequestValidationModeMember: "ChecksumMode",
			ResponseAlgorithms:          []string{"CRC64NVME", "CRC32", "CRC32C", "SHA256", "SHA1"},
		}, getObject.HTTPChecksum)
		assert.Equal(t, &spec.HTTPChecksum{RequestAlgorithmMember: "ChecksumAlgorithm"},
			svc.Operations["PutObject"].HTTPChecksum)
		assert.Equal(t, 204, svc.Operations["DeleteObject"].HTTP.ResponseCode)
		assert.Equal(t, &spec.Endpoint{HostPrefix: "{RequestRoute}."}, svc.Operations["WriteGetObjectResponse"].Endpoint)
		assert.Equal(t, map[string]spec.StaticContextParam{
			"DisableAccessPoints":         {Value: true},
			"UseS3ExpressControlEndpoint": {Value: true},
		}, svc.Operations["CreateBucket"].StaticContextParams)

		bucket, ok := svc.Shapes["GetObjectRequest"].Members.Lookup("Bucket")
		require.True(t, ok)
		assert.Equal(t, spec.LocationURI, bucket.Ref.Location)
		assert.Equal(t, "Bucket", bucket.Ref.LocationName)
		assert.Equal(t, &spec.ContextParam{Name: "Bucket"}, bucket.Ref.ContextParam)

		route, ok := svc.Shapes["WriteGetObjectResponseRequest"].Members.Lookup("RequestRoute")
		require.True(t, ok)
		assert.True(t, route.Ref.HostLabel)
		assert.Equal(t, spec.LocationHeader, route.Ref.Location)

		put := svc.Shapes["PutObjectRequest"]
		assert.Equal(t, "Body", put.Payload)

		body, ok := put.Members.Lookup("Body")
		require.True(t, ok)
		assert.True(t, body.Ref.Streaming)

		acl, ok := svc.Shapes["PutBucketAclRequest"].Members.Lookup("AccessControlPolicy")
		require.True(t, ok)
		assert.Equal(t, &spec.XMLNamespace{URI: "http://s3.amazonaws.com/doc/2006-03-01/"}, acl.Ref.XMLNamespace)

		grantee := svc.Shapes["Grantee"]
		assert.Equal(t, &spec.XMLNamespace{Prefix: "xsi", URI: "http://www.w3.org/2001/XMLSchema-instance"},
			grantee.XMLNamespace)

		typ, ok := grantee.Members.Lookup("Type")
		require.True(t, ok)
		assert.True(t, typ.Ref.XMLAttribute)
		assert.Equal(t, "xsi:type", typ.Ref.LocationName)

		assert.True(t, svc.Shapes["SelectObjectContentEventStream"].EventStream)
		assert.True(t, svc.Shapes["RecordsEvent"].Event)

		records, ok := svc.Shapes["RecordsEvent"].Members.Lookup("Payload")
		require.True(t, ok)
		assert.True(t, records.Ref.EventPayload)
	})

	t.Run("lambda marks streaming on the shape", func(t *testing.T) {
		t.Parallel()

		svc, err := loader.Botocore("lambda")
		require.NoError(t, err)

		assert.True(t, svc.Shapes["BlobStream"].Streaming)
		assert.True(t, svc.Shapes["InvokeWithResponseStreamResponseEvent"].EventStream)
	})

	t.Run("an unknown service is an error", func(t *testing.T) {
		t.Parallel()

		_, err := loader.Botocore("nope")
		require.Error(t, err)
	})
}
