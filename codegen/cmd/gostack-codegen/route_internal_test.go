package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol/rest"
	"github.com/stretchr/testify/require"
)

const (
	objectPath    = "/bucket/a/b.txt"
	opListBuckets = "ListBuckets"
	opGetObject   = "GetObject"
)

// s3Router builds the rest.Router of the patched S3 model from the rules the
// generator emits.
func s3Router(t *testing.T) ([]routeRule, *rest.Router) {
	t.Helper()

	load := newTestLoader(t)

	p, err := load.Botocore("s3")
	require.NoError(t, err)

	g, err := newModel("s3", serviceTable["s3"], p, nil)
	require.NoError(t, err)

	rules, err := g.routeRules()
	require.NoError(t, err)

	out := make([]rest.Rule, 0, len(rules))

	for _, r := range rules {
		query := make([]string, 0, len(r.query))
		for _, q := range r.query {
			query = append(query, strings.TrimSuffix(q.Key+"="+q.Value, "="))
		}

		out = append(out, rest.Rule{
			Operation: r.op,
			Method:    r.method,
			Path:      rest.MustParsePattern(r.path),
			Query:     query,
			Params:    r.params,
			Headers:   r.headers,
			Host:      r.host,
		})
	}

	return rules, rest.NewRouter(out...)
}

func Test_routeRules_S3Groups(t *testing.T) {
	t.Parallel()

	_, router := s3Router(t)

	tests := []struct {
		name       string
		giveMethod string
		giveURL    string
		giveHeader map[string]string
		wantOp     string
	}{
		{
			name:       "ListBuckets",
			giveMethod: http.MethodGet,
			giveURL:    "http://localhost.localstack.cloud/",
			wantOp:     opListBuckets,
		},
		{
			name:       "ListDirectoryBuckets by host",
			giveMethod: http.MethodGet,
			giveURL:    "http://s3express-control.us-east-1.amazonaws.com/",
			wantOp:     "ListDirectoryBuckets",
		},
		{
			name:       "GetObject by key",
			giveMethod: http.MethodGet,
			giveURL:    objectPath,
			wantOp:     opGetObject,
		},
		{
			name:       "ListParts by uploadId",
			giveMethod: http.MethodGet,
			giveURL:    objectPath + "?uploadId=u",
			wantOp:     "ListParts",
		},
		{
			name:       "DeleteObject",
			giveMethod: http.MethodDelete,
			giveURL:    objectPath,
			wantOp:     "DeleteObject",
		},
		{
			name:       "AbortMultipartUpload by uploadId",
			giveMethod: http.MethodDelete,
			giveURL:    objectPath + "?uploadId=u",
			wantOp:     "AbortMultipartUpload",
		},
		{
			name:       "PutObject",
			giveMethod: http.MethodPut,
			giveURL:    objectPath,
			wantOp:     "PutObject",
		},
		{
			name:       "CopyObject by x-amz-copy-source",
			giveMethod: http.MethodPut,
			giveURL:    objectPath,
			giveHeader: map[string]string{"x-amz-copy-source": "src/k"},
			wantOp:     "CopyObject",
		},
		{
			name:       "UploadPart by partNumber and uploadId",
			giveMethod: http.MethodPut,
			giveURL:    objectPath + "?partNumber=1&uploadId=u",
			wantOp:     "UploadPart",
		},
		{
			name:       "UploadPartCopy by query and header",
			giveMethod: http.MethodPut,
			giveURL:    objectPath + "?partNumber=1&uploadId=u",
			giveHeader: map[string]string{"x-amz-copy-source": "src/k"},
			wantOp:     "UploadPartCopy",
		},
		{
			name:       "ListObjects",
			giveMethod: http.MethodGet,
			giveURL:    "/bucket",
			wantOp:     "ListObjects",
		},
		{
			name:       "ListObjectsV2 by list-type",
			giveMethod: http.MethodGet,
			giveURL:    "/bucket?list-type=2",
			wantOp:     "ListObjectsV2",
		},
		{
			name:       "ListBucketAnalyticsConfigurations",
			giveMethod: http.MethodGet,
			giveURL:    "/bucket?analytics",
			wantOp:     "ListBucketAnalyticsConfigurations",
		},
		{
			name:       "GetBucketAnalyticsConfiguration by id",
			giveMethod: http.MethodGet,
			giveURL:    "/bucket?analytics&id=a",
			wantOp:     "GetBucketAnalyticsConfiguration",
		},
		{
			name:       "ListBucketIntelligentTieringConfigurations",
			giveMethod: http.MethodGet,
			giveURL:    "/bucket?intelligent-tiering",
			wantOp:     "ListBucketIntelligentTieringConfigurations",
		},
		{
			name:       "GetBucketIntelligentTieringConfiguration by id",
			giveMethod: http.MethodGet,
			giveURL:    "/bucket?intelligent-tiering&id=a",
			wantOp:     "GetBucketIntelligentTieringConfiguration",
		},
		{
			name:       "ListBucketInventoryConfigurations",
			giveMethod: http.MethodGet,
			giveURL:    "/bucket?inventory",
			wantOp:     "ListBucketInventoryConfigurations",
		},
		{
			name:       "GetBucketInventoryConfiguration by id",
			giveMethod: http.MethodGet,
			giveURL:    "/bucket?inventory&id=a",
			wantOp:     "GetBucketInventoryConfiguration",
		},
		{
			name:       "ListBucketMetricsConfigurations",
			giveMethod: http.MethodGet,
			giveURL:    "/bucket?metrics",
			wantOp:     "ListBucketMetricsConfigurations",
		},
		{
			name:       "GetBucketMetricsConfiguration by id",
			giveMethod: http.MethodGet,
			giveURL:    "/bucket?metrics&id=a",
			wantOp:     "GetBucketMetricsConfiguration",
		},
		{
			name:       "GetBucketLifecycle twin resolves to Configuration",
			giveMethod: http.MethodGet,
			giveURL:    "/bucket?lifecycle",
			wantOp:     "GetBucketLifecycleConfiguration",
		},
		{
			name:       "PutBucketLifecycle twin resolves to Configuration",
			giveMethod: http.MethodPut,
			giveURL:    "/bucket?lifecycle",
			wantOp:     "PutBucketLifecycleConfiguration",
		},
		{
			name:       "GetBucketNotification twin resolves to Configuration",
			giveMethod: http.MethodGet,
			giveURL:    "/bucket?notification",
			wantOp:     "GetBucketNotificationConfiguration",
		},
		{
			name:       "PutBucketNotification twin resolves to Configuration",
			giveMethod: http.MethodPut,
			giveURL:    "/bucket?notification",
			wantOp:     "PutBucketNotificationConfiguration",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := httptest.NewRequestWithContext(t.Context(), tt.giveMethod, tt.giveURL, nil)
			for k, v := range tt.giveHeader {
				r.Header.Set(k, v)
			}

			op, ok := router.Route(r)
			require.True(t, ok)
			require.Equal(t, tt.wantOp, op)
		})
	}
}

// Test_routeRules_EveryOperation sends each S3 operation the request its own
// rule describes and checks that it reaches that operation, or, for a
// deprecated twin, the operation that replaced it.
func Test_routeRules_EveryOperation(t *testing.T) {
	t.Parallel()

	rules, router := s3Router(t)

	replacement := map[string]string{}

	for _, r := range rules {
		if !r.deprecated {
			replacement[r.tieKey()] = r.op
		}
	}

	for _, r := range rules {
		t.Run(r.op, func(t *testing.T) {
			t.Parallel()

			path := strings.NewReplacer("{Bucket}", "bucket", "{Key+}", "a/b.txt").Replace(r.path)

			query := make([]string, 0, len(r.query)+len(r.params))
			for _, q := range r.query {
				query = append(query, strings.TrimSuffix(q.Key+"="+q.Value, "="))
			}

			for _, param := range r.params {
				query = append(query, param+"=1")
			}

			req := httptest.NewRequestWithContext(t.Context(), r.method, "http://"+r.host+"localhost.localstack.cloud"+path, nil)
			req.URL.RawQuery = strings.Join(query, "&")

			for _, h := range r.headers {
				req.Header.Set(h, "1")
			}

			want := r.op
			if r.deprecated {
				want = replacement[r.tieKey()]
			}

			op, ok := router.Route(req)
			require.True(t, ok)
			require.Equal(t, want, op)
		})
	}
}

func Test_routeRules_Indistinguishable(t *testing.T) {
	t.Parallel()

	load := newTestLoader(t)

	p, err := load.Botocore("s3")
	require.NoError(t, err)

	twin := *p.Operations["GetObjectAcl"]
	p.Operations["GetObjectAclAgain"] = &twin

	g, err := newModel("s3", serviceTable["s3"], p, nil)
	require.NoError(t, err)

	_, err = g.routeRules()
	require.ErrorContains(t, err, "bind the same requests")
}
