package restxml_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The members the S3 wire test looks for.
const (
	memberContents     = "Contents"
	memberLastModified = "LastModified"
)

// s3Stub answers ListObjectsV2 and CopyObject with fixed outputs, so a test
// reads the bytes the generated S3 writer puts on the wire.
type s3Stub struct {
	s3api.Unimplemented

	modified *protocol.Time
}

func (s *s3Stub) ListObjectsV2(context.Context, *s3api.ListObjectsV2Input) (*s3api.ListObjectsV2Output, error) {
	return &s3api.ListObjectsV2Output{
		IsTruncated: new(false),
		Name:        new("bucket"),
		Prefix:      new(""),
		MaxKeys:     new(int32(1000)),
		KeyCount:    new(int32(1)),
		Contents: []s3api.Object{{
			Key: new("key"), LastModified: s.modified, ETag: new(`"etag"`), Size: new(int64(4)),
		}},
	}, nil
}

func (s *s3Stub) CopyObject(context.Context, *s3api.CopyObjectInput) (*s3api.CopyObjectOutput, error) {
	return &s3api.CopyObjectOutput{
		CopyObjectResult: &s3api.CopyObjectResult{ETag: new(`"etag"`), LastModified: s.modified},
	}, nil
}

// Test_S3Wire sends raw HTTP requests to the generated S3 Serve and checks the
// response body against what LocalStack's S3 serializer writes: the double
// quoted declaration, S3's root names and namespace, the members in spec order
// and timestamps with ".000Z".
func Test_S3Wire(t *testing.T) {
	t.Parallel()

	stub := &s3Stub{modified: &protocol.Time{Time: time.Date(2026, 11, 18, 12, 0, 0, 123456789, time.UTC)}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		//nolint:contextcheck // the generated Serve takes the context from rc.Request.
		s3api.Serve(stub, w, &request.Context{Service: "s3", RequestID: "req-1", Request: r})
	}))
	t.Cleanup(srv.Close)

	tests := []struct {
		name       string
		giveMethod string
		giveTarget string
		giveHeader http.Header
		wantRoot   string
		// wantOrder are members in the order the body has them, the last one
		// last of all.
		wantOrder []string
		// wantTimeAt is the path from the root to a timestamp.
		wantTimeAt []string
	}{
		{
			name:       "ListObjectsV2",
			giveMethod: http.MethodGet,
			giveTarget: "/bucket?list-type=2",
			wantRoot:   "ListBucketResult",
			wantOrder:  []string{"IsTruncated", "Name", memberContents},
			wantTimeAt: []string{memberContents, memberLastModified},
		},
		{
			name:       "CopyObject, whose payload member is the root",
			giveMethod: http.MethodPut,
			giveTarget: "/bucket/copy",
			giveHeader: http.Header{"X-Amz-Copy-Source": {"/bucket/key"}},
			wantRoot:   "CopyObjectResult",
			wantOrder:  []string{"ETag", memberLastModified},
			wantTimeAt: []string{memberLastModified},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequestWithContext(t.Context(), tt.giveMethod, srv.URL+tt.giveTarget, http.NoBody)
			require.NoError(t, err)

			maps.Copy(req.Header, tt.giveHeader)

			resp, err := srv.Client().Do(req)
			require.NoError(t, err)

			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

			declaration, _, _ := bytes.Cut(body, []byte("\n"))
			assert.Equal(t, `<?xml version="1.0" encoding="UTF-8"?>`, string(declaration))

			var root element

			//nolint:musttag // element takes any element; XMLName and the any fields need no names
			require.NoError(t, xml.Unmarshal(body, &root), string(body))
			assert.Equal(t, xml.Name{Space: "http://s3.amazonaws.com/doc/2006-03-01/", Local: tt.wantRoot}, root.XMLName)

			names := make([]string, 0, len(root.Kids))
			for _, k := range root.Kids {
				names = append(names, k.XMLName.Local)
			}

			last := -1

			for _, want := range tt.wantOrder {
				i := slices.Index(names, want)
				assert.Greater(t, i, last, "%s in %v", want, names)
				last = i
			}

			assert.Equal(t, len(names)-1, last, "%s is last in %v", tt.wantOrder[len(tt.wantOrder)-1], names)

			at := root
			for _, name := range tt.wantTimeAt {
				i := slices.IndexFunc(at.Kids, func(k element) bool { return k.XMLName.Local == name })
				require.GreaterOrEqual(t, i, 0, "%s has no %s", at.XMLName.Local, name)
				at = at.Kids[i]
			}

			assert.Equal(t, "2026-11-18T12:00:00.000Z", at.Text)
		})
	}
}
