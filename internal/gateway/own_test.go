package gateway_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// objectText is the object body the gzip tests send compressed.
	objectText = "an object body stored gzipped"

	headerContentEncoding = "Content-Encoding"
	encodingGzip          = "gzip"
)

// ownedCall is what a fake service's Serve was given.
type ownedCall struct {
	service  string
	method   string
	body     []byte
	encoding string
	length   int64
}

// recordCalls is a Serve that answers "ok" and appends what it was given to
// calls.
func recordCalls(t *testing.T, calls *[]ownedCall) func(http.ResponseWriter, *request.Context) {
	t.Helper()

	return func(w http.ResponseWriter, rc *request.Context) {
		body, err := io.ReadAll(rc.Request.Body)
		require.NoError(t, err)

		*calls = append(*calls, ownedCall{
			service: rc.Service, method: rc.Request.Method, body: body,
			encoding: rc.Request.Header.Get(headerContentEncoding), length: rc.Request.ContentLength,
		})

		_, _ = io.WriteString(w, "ok")
	}
}

// ownsBucketPaths is a Preprocess that owns every request under /bucket, as
// S3's CORS preprocess marks a path-style request to a bucket it has
// (services/s3/cors.py:101-110).
func ownsBucketPaths(r *http.Request) *http.Request {
	if strings.HasPrefix(r.URL.Path, "/bucket") {
		return service.Own(r, nameS3)
	}

	return nil
}

// ownGateway is a gateway with sqs, and with a fake s3 whose Preprocess owns
// every request under /bucket. flags sets the fake's RawBody and OwnCORS.
func ownGateway(
	t *testing.T, settings *liveSettings, flags func(*service.Registration),
) (*gateway.Gateway, *[]ownedCall) {
	t.Helper()

	var calls []ownedCall

	record := recordCalls(t, &calls)

	s3, _ := s3Registration()
	s3.Serve = record
	s3.Preprocess = ownsBucketPaths
	flags(&s3)

	return newGateway(t, settings, []service.Registration{sqsRegistration(record), s3}), &calls
}

func selfManaged(reg *service.Registration) {
	reg.RawBody, reg.OwnCORS = true, true
}

func gzipped(t *testing.T, text string) []byte {
	t.Helper()

	var buf bytes.Buffer

	zw := gzip.NewWriter(&buf)
	_, err := zw.Write([]byte(text))
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	return buf.Bytes()
}

func preflight(t *testing.T, target, origin string) *http.Request {
	t.Helper()

	return newRequest(t, http.MethodOptions, target, "", map[string]string{
		headerOrigin: origin, headerRequestMethod: http.MethodPut,
	})
}

// Test_Gateway_ownedRawBody is LocalStack's ContentDecoder, which skips s3
// (aws/handlers/codec.py): an owned request whose service sets RawBody reaches
// it with the gzip bytes as sent.
func Test_Gateway_ownedRawBody(t *testing.T) {
	zipped := gzipped(t, objectText)

	putObject := func(t *testing.T) *http.Request {
		t.Helper()

		return newRequest(t, http.MethodPut, "http://localhost:4566/bucket/key.gz", string(zipped), map[string]string{
			headerAuth: signed("test", usEast1, nameS3), headerContentEncoding: encodingGzip,
		})
	}

	t.Run("an owned request keeps its gzip body", func(t *testing.T) {
		g, calls := ownGateway(t, newSettings(), selfManaged)

		resp, _ := call(t, g.Handler(), putObject(t))

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Len(t, *calls, 1)
		assert.Equal(t, nameS3, (*calls)[0].service)
		assert.Equal(t, zipped, (*calls)[0].body)
		assert.Equal(t, encodingGzip, (*calls)[0].encoding)
		assert.Equal(t, int64(len(zipped)), (*calls)[0].length)
	})

	t.Run("an owned request of a service without RawBody is decoded", func(t *testing.T) {
		g, calls := ownGateway(t, newSettings(), func(reg *service.Registration) { reg.OwnCORS = true })

		resp, _ := call(t, g.Handler(), putObject(t))

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Len(t, *calls, 1)
		assert.Equal(t, objectText, string((*calls)[0].body))
		assert.Equal(t, "identity", (*calls)[0].encoding)
	})

	t.Run("an unowned request is decoded", func(t *testing.T) {
		g, calls := ownGateway(t, newSettings(), selfManaged)

		r := sendMessage(t)
		r.Body = io.NopCloser(bytes.NewReader(gzipped(t, `{"QueueUrl":"q"}`)))
		r.Header.Set(headerContentEncoding, encodingGzip)

		resp, _ := call(t, g.Handler(), r)

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Len(t, *calls, 1)
		assert.Equal(t, nameSQS, (*calls)[0].service)
		assert.JSONEq(t, `{"QueueUrl":"q"}`, string((*calls)[0].body))
		assert.Equal(t, "identity", (*calls)[0].encoding)
	})
}

// Test_Gateway_ownedCORS is should_enforce_self_managed_service in LocalStack's
// aws/handlers/cors.py: an owned request whose service sets OwnCORS skips the
// gateway's CORS, unless DISABLE_CUSTOM_CORS_<NAME> is set.
func Test_Gateway_ownedCORS(t *testing.T) {
	const ownedObject = "http://localhost:4566/bucket/key"

	t.Run("an owned preflight reaches the service, whatever its origin", func(t *testing.T) {
		g, calls := ownGateway(t, newSettings(), selfManaged)

		for _, origin := range []string{badOrigin, appOrigin} {
			*calls = nil

			resp, body := call(t, g.Handler(), preflight(t, ownedObject, origin))

			assert.Equal(t, http.StatusOK, resp.StatusCode, origin)
			assert.Equal(t, "ok", body, origin)
			require.Len(t, *calls, 1, origin)
			assert.Equal(t, http.MethodOptions, (*calls)[0].method, origin)
			assert.Empty(t, resp.Header.Get(allowOrigin), "the gateway adds no CORS header")
			assert.Empty(t, resp.Header.Get(allowMethods), "the gateway adds no CORS header")
		}
	})

	t.Run("an owned request from a refused origin reaches the service", func(t *testing.T) {
		g, calls := ownGateway(t, newSettings(), selfManaged)

		resp, _ := call(t, g.Handler(), newRequest(t, http.MethodGet, ownedObject, "", map[string]string{
			headerOrigin: badOrigin,
		}))

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Len(t, *calls, 1)
		assert.Empty(t, resp.Header.Get(allowOrigin))
	})

	t.Run("unowned requests are still checked", func(t *testing.T) {
		g, calls := ownGateway(t, newSettings(), selfManaged)

		resp, _ := call(t, g.Handler(), preflight(t, "http://localhost:4566/", badOrigin))
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)

		resp, _ = call(t, g.Handler(), preflight(t, "http://localhost:4566/", appOrigin))
		assert.Equal(t, http.StatusNoContent, resp.StatusCode)
		assert.Equal(t, appOrigin, resp.Header.Get(allowOrigin))

		assert.Empty(t, *calls, "the gateway answers both")
	})

	tests := []struct {
		name         string
		giveFlags    func(*service.Registration)
		giveSettings func(*gateway.Settings)
	}{
		{
			name:         "DISABLE_CUSTOM_CORS_S3 gives the gateway's CORS back",
			giveFlags:    selfManaged,
			giveSettings: func(s *gateway.Settings) { s.DisableCustomCORS = []string{nameS3} },
		},
		{
			name:      "an owned request of a service without OwnCORS is checked",
			giveFlags: func(reg *service.Registration) { reg.RawBody = true },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mutate []func(*gateway.Settings)
			if tt.giveSettings != nil {
				mutate = append(mutate, tt.giveSettings)
			}

			g, calls := ownGateway(t, newSettings(mutate...), tt.giveFlags)

			resp, _ := call(t, g.Handler(), preflight(t, ownedObject, badOrigin))
			assert.Equal(t, http.StatusForbidden, resp.StatusCode)

			resp, _ = call(t, g.Handler(), preflight(t, ownedObject, appOrigin))
			assert.Equal(t, http.StatusNoContent, resp.StatusCode)
			assert.Equal(t, appOrigin, resp.Header.Get(allowOrigin))

			assert.Empty(t, *calls, "the gateway answers both")
		})
	}
}

// Test_Gateway_ownedCORSPolicy: a service that handles CORS itself gets the
// gateway's policy in request.Context.CORS, which is CorsEnforcer's origin
// check and CorsResponseEnricher's headers; a request the gateway's CORS has
// answered carries none.
func Test_Gateway_ownedCORSPolicy(t *testing.T) {
	const ownedObject = "http://localhost:4566/bucket/key"

	tests := []struct {
		name         string
		giveTarget   string
		giveSettings func(*gateway.Settings)
		giveFlags    func(*service.Registration)
		wantPolicy   bool
	}{
		{name: "an owned request", giveTarget: ownedObject, giveFlags: selfManaged, wantPolicy: true},
		{
			name: "DISABLE_CUSTOM_CORS_S3", giveTarget: ownedObject, giveFlags: selfManaged,
			giveSettings: func(s *gateway.Settings) { s.DisableCustomCORS = []string{nameS3} },
		},
		{name: "a service without OwnCORS", giveTarget: ownedObject, giveFlags: func(*service.Registration) {}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var policy request.DefaultCORS

			s3, _ := s3Registration()
			s3.Serve = func(w http.ResponseWriter, rc *request.Context) {
				policy = rc.CORS

				_, _ = io.WriteString(w, "ok")
			}
			s3.Preprocess = func(r *http.Request) *http.Request { return service.Own(r, nameS3) }
			tt.giveFlags(&s3)

			var mutate []func(*gateway.Settings)
			if tt.giveSettings != nil {
				mutate = append(mutate, tt.giveSettings)
			}

			g := newGateway(t, newSettings(mutate...), []service.Registration{s3})

			resp, _ := call(t, g.Handler(), newRequest(t, http.MethodGet, tt.giveTarget, "", map[string]string{
				headerAuth: signed("test", usEast1, nameS3),
			}))
			require.Equal(t, http.StatusOK, resp.StatusCode)

			if !tt.wantPolicy {
				assert.Nil(t, policy)

				return
			}

			require.NotNil(t, policy)
			assert.False(t, policy.ChecksDisabled())
			assert.True(t, policy.OriginAllowed(http.Header{headerOrigin: {appOrigin}}))
			assert.False(t, policy.OriginAllowed(http.Header{headerOrigin: {badOrigin}}))
			assert.True(t, policy.OriginAllowed(http.Header{}), "no Origin and no Referer is allowed")

			req := newRequest(t, http.MethodGet, tt.giveTarget, "", map[string]string{headerOrigin: appOrigin})
			header := http.Header{allowMethods: {"GET, PUT"}}
			policy.AddHeaders(header, req)

			assert.Equal(t, appOrigin, header.Get(allowOrigin))
			assert.Equal(t, "true", header.Get("Access-Control-Allow-Credentials"))
			assert.Equal(t, "GET, PUT", header.Get(allowMethods), "a header already set is kept")
			assert.NotEmpty(t, header.Get("Access-Control-Allow-Headers"))
			assert.NotEmpty(t, header.Get("Access-Control-Expose-Headers"))
			assert.Equal(t, "Origin", header.Get("Vary"))
		})
	}
}

// Test_Gateway_ownerWinsDetection is aws/handlers/service.py:38: the service a
// Preprocess marked takes the request ahead of every detection step.
func Test_Gateway_ownerWinsDetection(t *testing.T) {
	g, calls := ownGateway(t, newSettings(), selfManaged)

	r := sendMessage(t)
	r.URL.Path = "/bucket/key"

	resp, _ := call(t, g.Handler(), r)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Len(t, *calls, 1)
	assert.Equal(t, nameS3, (*calls)[0].service, "signed for sqs, owned by s3")
}

// Test_Gateway_hostClaimBeatsOwnerMark: a request that a service claimed by its
// host stays that service's. A Preprocess of another service that owns it, as
// S3's does for a path whose first segment is a bucket, is not run for it, so
// the claiming service still gets the root gzip decoding and CORS.
func Test_Gateway_hostClaimBeatsOwnerMark(t *testing.T) {
	const (
		bucketKey   = "/bucket/key"
		claimedHost = "http://s3.localhost.localstack.cloud:4566"
	)

	zipped := gzipped(t, objectText)

	newClaimGateway := func(t *testing.T) (*gateway.Gateway, *[]ownedCall) {
		t.Helper()

		var calls []ownedCall

		record := recordCalls(t, &calls)

		s3, _ := s3Registration()
		s3.Serve = record
		s3.Preprocess = ownsBucketPaths
		s3.ClaimHost = func(host string) bool { return strings.HasPrefix(host, "s3.") }
		selfManaged(&s3)

		edge := edgeRegistration()
		edge.Serve = record

		return newGateway(t, newSettings(), []service.Registration{s3, edge}), &calls
	}

	put := func(t *testing.T, target string) *http.Request {
		t.Helper()

		return newRequest(t, http.MethodPut, target, string(zipped), map[string]string{headerContentEncoding: encodingGzip})
	}

	t.Run("the claiming service gets the gzip body decoded", func(t *testing.T) {
		g, calls := newClaimGateway(t)

		resp, _ := call(t, g.Handler(), put(t, functionURL+bucketKey))

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Len(t, *calls, 1)
		assert.Equal(t, nameEdge, (*calls)[0].service)
		assert.Equal(t, objectText, string((*calls)[0].body))
		assert.Equal(t, "identity", (*calls)[0].encoding)
	})

	t.Run("the claiming service gets the gateway's CORS", func(t *testing.T) {
		g, calls := newClaimGateway(t)

		resp, _ := call(t, g.Handler(), preflight(t, functionURL+bucketKey, badOrigin))
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)

		resp, _ = call(t, g.Handler(), preflight(t, functionURL+bucketKey, appOrigin))
		assert.Equal(t, http.StatusNoContent, resp.StatusCode)
		assert.Equal(t, appOrigin, resp.Header.Get(allowOrigin))

		assert.Empty(t, *calls, "the gateway answers both")
	})

	t.Run("a service that claims a host and owns it keeps its own handling", func(t *testing.T) {
		g, calls := newClaimGateway(t)

		resp, _ := call(t, g.Handler(), put(t, claimedHost+bucketKey))

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Len(t, *calls, 1)
		assert.Equal(t, nameS3, (*calls)[0].service)
		assert.Equal(t, zipped, (*calls)[0].body, "the claimant's own Preprocess still marks it")
		assert.Equal(t, encodingGzip, (*calls)[0].encoding)

		resp, _ = call(t, g.Handler(), preflight(t, claimedHost+bucketKey, badOrigin))
		assert.Equal(t, http.StatusOK, resp.StatusCode, "the claimant's own CORS")
	})
}

// Test_Gateway_hostClaimOwns: the service that claims a host owns the request
// without a Preprocess to say so, so its RawBody and OwnCORS apply to the host
// it claimed. A claim that is ambiguous has no owner, and the gateway's own
// path on a claimed host is nobody's.
func Test_Gateway_hostClaimOwns(t *testing.T) {
	const claimedHost = "http://s3.localhost.localstack.cloud:4566"

	zipped := gzipped(t, objectText)

	newClaimGateway := func(t *testing.T, regs ...func(*service.Registration)) (*gateway.Gateway, *[]ownedCall) {
		t.Helper()

		var calls []ownedCall

		record := recordCalls(t, &calls)

		s3, _ := s3Registration()
		s3.Serve = record
		s3.ClaimHost = func(host string) bool { return strings.HasPrefix(host, "s3.") }
		selfManaged(&s3)

		all := make([]service.Registration, 0, 1+len(regs))
		all = append(all, s3)

		for _, edit := range regs {
			edge := edgeRegistration()
			edge.Serve = record
			edit(&edge)

			all = append(all, edge)
		}

		return newGateway(t, newSettings(), all), &calls
	}

	t.Run("a claimant without a Preprocess gets its raw body", func(t *testing.T) {
		g, calls := newClaimGateway(t)

		resp, _ := call(t, g.Handler(), newRequest(t, http.MethodPut, claimedHost+"/bucket/key", string(zipped),
			map[string]string{headerContentEncoding: encodingGzip}))

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Len(t, *calls, 1)
		assert.Equal(t, nameS3, (*calls)[0].service)
		assert.Equal(t, zipped, (*calls)[0].body)
		assert.Equal(t, encodingGzip, (*calls)[0].encoding)
	})

	t.Run("a claimant without a Preprocess handles its own preflight", func(t *testing.T) {
		g, calls := newClaimGateway(t)

		resp, _ := call(t, g.Handler(), preflight(t, claimedHost+"/bucket/key", badOrigin))

		assert.Equal(t, http.StatusOK, resp.StatusCode, "the root CORS would answer 403")
		require.Len(t, *calls, 1)
		assert.Equal(t, nameS3, (*calls)[0].service)
	})

	t.Run("two claimants leave the request unowned", func(t *testing.T) {
		g, calls := newClaimGateway(t, func(edge *service.Registration) {
			edge.ClaimHost = func(host string) bool { return strings.HasPrefix(host, "s3.") }
		})

		resp, _ := call(t, g.Handler(), preflight(t, claimedHost+"/bucket/key", badOrigin))

		assert.Equal(t, http.StatusForbidden, resp.StatusCode, "the gateway's CORS answers")
		assert.Empty(t, *calls)
	})

	t.Run("the gateway's own path is not marked", func(t *testing.T) {
		g, calls := newClaimGateway(t)

		resp, _ := call(t, g.Handler(), preflight(t, claimedHost+"/_localstack/health", badOrigin))

		assert.Equal(t, http.StatusForbidden, resp.StatusCode, "the gateway's CORS answers")
		assert.Empty(t, *calls)
	})
}
