package s3

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_renamedRoot(t *testing.T) {
	tests := []struct {
		name     string
		give     string
		want     string
		wantDone bool
	}{
		{
			name: "a root in the table, with a declaration and a namespace",
			give: `<?xml version='1.0' encoding='utf-8'?>` + "\n" +
				`<DeleteObjectsOutput xmlns="urn:x"><Deleted/></DeleteObjectsOutput>`,
			want: `<?xml version='1.0' encoding='utf-8'?>` + "\n" +
				`<DeleteResult xmlns="urn:x"><Deleted/></DeleteResult>`,
			wantDone: true,
		},
		{
			name:     "a trailing newline stays",
			give:     "<ListBucketsOutput><a>1</a></ListBucketsOutput>\n",
			want:     `<ListAllMyBucketsResult xmlns="` + s3Namespace + `"><a>1</a></ListAllMyBucketsResult>` + "\n",
			wantDone: true,
		},
		{
			name:     "an empty root that closes itself",
			give:     `<GetBucketTaggingOutput xmlns="urn:x"/>`,
			want:     `<Tagging xmlns="urn:x"/>`,
			wantDone: true,
		},
		{name: "a root S3 does not rename", give: "<Error><Code>x</Code></Error>", wantDone: false},
		{name: "not XML", give: "plain bytes", wantDone: false},
		{name: "an unfinished declaration", give: "<?xml version", wantDone: false},
		{name: "empty", give: "", wantDone: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parts, done := renamedRoot([]byte(tt.give))
			require.Equal(t, tt.wantDone, done)

			if done {
				assert.Equal(t, tt.want, string(bytes.Join(parts, nil)))
			}
		})
	}
}
