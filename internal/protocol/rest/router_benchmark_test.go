package rest_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol/rest"
)

// BenchmarkRouter routes requests through rules shaped like S3's: an object
// path shared by four operations told apart by query and header, behind
// bucket sub-resources.
func BenchmarkRouter(b *testing.B) {
	object := rest.MustParsePattern("/{Bucket}/{Key+}")
	bucket := rest.MustParsePattern("/{Bucket}")
	router := rest.NewRouter(
		rest.Rule{Operation: "GetBucketAcl", Method: http.MethodGet, Path: bucket, Query: []string{"acl"}},
		rest.Rule{Operation: "ListObjectsV2", Method: http.MethodGet, Path: bucket, Query: []string{"list-type=2"}},
		rest.Rule{Operation: "ListObjects", Method: http.MethodGet, Path: bucket},
		rest.Rule{
			Operation: "UploadPartCopy", Method: http.MethodPut, Path: object,
			Params: []string{"partNumber", "uploadId"}, Headers: []string{"x-amz-copy-source"},
		},
		rest.Rule{Operation: "UploadPart", Method: http.MethodPut, Path: object, Params: []string{"partNumber", "uploadId"}},
		rest.Rule{Operation: "CopyObject", Method: http.MethodPut, Path: object, Headers: []string{"x-amz-copy-source"}},
		rest.Rule{Operation: "PutObject", Method: http.MethodPut, Path: object},
	)

	tests := []struct {
		name string
		req  *http.Request
	}{
		{name: "op=PutObject", req: httptest.NewRequestWithContext(b.Context(), http.MethodPut, "/b/a/key.txt", nil)},
		{name: "op=ListObjectsV2", req: httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/b?list-type=2", nil)},
	}

	for _, tt := range tests {
		b.Run(tt.name, func(b *testing.B) {
			for b.Loop() {
				if _, ok := router.Route(tt.req); !ok {
					b.Fatal("no route")
				}
			}
		})
	}
}
