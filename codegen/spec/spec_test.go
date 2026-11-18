package spec_test

import (
	"testing"

	"github.com/kylecannon/gostack/codegen/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	opAdd    = "add"
	opRemove = "remove"
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
			giveOps: []spec.Patch{{Op: "replace", Path: "/c", Value: []byte(`"y"`)}},
			want:    `{"a":{"b":[1,2,3],"big":9223372036854775807},"c":"y"}`,
		},
		{
			name:    "move and copy",
			giveOps: []spec.Patch{{Op: "move", From: "/c", Path: "/a/c"}, {Op: "copy", From: "/a/b", Path: "/b2"}},
			want:    `{"a":{"b":[1,2,3],"big":9223372036854775807,"c":"x"},"b2":[1,2,3]}`,
		},
		{
			name:    "test that passes changes nothing",
			giveOps: []spec.Patch{{Op: "test", Path: "/c", Value: []byte(`"x"`)}},
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

func Test_ApplyPatches_Errors(t *testing.T) {
	t.Parallel()

	const doc = `{"a":[1],"c":"x"}`

	tests := []struct {
		name   string
		giveOp spec.Patch
	}{
		{name: "remove a missing member", giveOp: spec.Patch{Op: opRemove, Path: "/nope"}},
		{name: "replace a missing member", giveOp: spec.Patch{Op: "replace", Path: "/nope", Value: []byte(`1`)}},
		{name: "add under a missing parent", giveOp: spec.Patch{Op: opAdd, Path: "/nope/x", Value: []byte(`1`)}},
		{name: "array index past the end", giveOp: spec.Patch{Op: opAdd, Path: "/a/5", Value: []byte(`1`)}},
		{name: "test that fails", giveOp: spec.Patch{Op: "test", Path: "/c", Value: []byte(`"y"`)}},
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

	t.Run("an unknown service is an error", func(t *testing.T) {
		t.Parallel()

		_, err := loader.Botocore("nope")
		require.Error(t, err)
	})
}
