package gateway_test

import (
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	allowCredentials = "Access-Control-Allow-Credentials"
	allowMethods     = "Access-Control-Allow-Methods"
	allowHeaders     = "Access-Control-Allow-Headers"
	exposeHeaders    = "Access-Control-Expose-Headers"
)

func corsRequest(t *testing.T, method string, headers map[string]string) *http.Request {
	t.Helper()

	all := map[string]string{headerAuth: signed("test", usEast1, nameSQS)}
	maps.Copy(all, headers)

	return newRequest(t, method, "http://localhost:4566/", "", all)
}

// Test_Gateway_cors follows tests/integration/test_security.py of LocalStack.
func Test_Gateway_cors(t *testing.T) {
	tests := []struct {
		name          string
		giveSettings  func(*gateway.Settings)
		giveMethod    string
		giveHeaders   map[string]string
		wantStatus    int
		wantOrigin    string
		wantNoCORS    bool
		wantCredsTrue bool
	}{
		{
			name:          "allowed origin",
			giveMethod:    http.MethodGet,
			giveHeaders:   map[string]string{headerOrigin: appOrigin},
			wantStatus:    http.StatusOK,
			wantOrigin:    appOrigin,
			wantCredsTrue: true,
		},
		{
			name:          "localhost on the listen port",
			giveMethod:    http.MethodGet,
			giveHeaders:   map[string]string{headerOrigin: "http://localhost:4566"},
			wantStatus:    http.StatusOK,
			wantOrigin:    "http://localhost:4566",
			wantCredsTrue: true,
		},
		{
			name:        "localhost on another port",
			giveMethod:  http.MethodGet,
			giveHeaders: map[string]string{headerOrigin: "http://localhost:4567"},
			wantStatus:  http.StatusForbidden,
			wantNoCORS:  true,
		},
		{
			name:        "forbidden origin",
			giveMethod:  http.MethodGet,
			giveHeaders: map[string]string{headerOrigin: badOrigin},
			wantStatus:  http.StatusForbidden,
			wantNoCORS:  true,
		},
		{
			name:        "forbidden referer",
			giveMethod:  http.MethodGet,
			giveHeaders: map[string]string{headerReferer: "http://attacker.com/page?x=1"},
			wantStatus:  http.StatusForbidden,
			wantNoCORS:  true,
		},
		{
			name:       "allowed referer without an Origin",
			giveMethod: http.MethodGet,
			giveHeaders: map[string]string{
				headerReferer: appOrigin + "/dashboard",
			},
			wantStatus: http.StatusOK,
			wantNoCORS: true,
		},
		{
			name:        "the Origin decides when there is a Referer too",
			giveMethod:  http.MethodGet,
			giveHeaders: map[string]string{headerOrigin: badOrigin, headerReferer: appOrigin + "/"},
			wantStatus:  http.StatusForbidden,
			wantNoCORS:  true,
		},
		{
			name:       "no Origin and no Referer sends no CORS header",
			giveMethod: http.MethodGet,
			wantStatus: http.StatusOK,
			wantNoCORS: true,
		},
		{
			name:         "DISABLE_CORS_CHECKS lets any origin in with *, without credentials",
			giveSettings: func(s *gateway.Settings) { s.DisableCORSChecks = true },
			giveMethod:   http.MethodGet,
			giveHeaders:  map[string]string{headerOrigin: "https://invalid.localstack.cloud"},
			wantStatus:   http.StatusOK,
			wantOrigin:   "*",
		},
		{
			name:         "DISABLE_CORS_HEADERS sends no header and still checks",
			giveSettings: func(s *gateway.Settings) { s.DisableCORSHeaders = true },
			giveMethod:   http.MethodGet,
			giveHeaders:  map[string]string{headerOrigin: appOrigin},
			wantStatus:   http.StatusOK,
			wantNoCORS:   true,
		},
		{
			name:         "DISABLE_CORS_HEADERS does not open the checks",
			giveSettings: func(s *gateway.Settings) { s.DisableCORSHeaders = true },
			giveMethod:   http.MethodGet,
			giveHeaders:  map[string]string{headerOrigin: badOrigin},
			wantStatus:   http.StatusForbidden,
			wantNoCORS:   true,
		},
		{
			name:          "EXTRA_CORS_ALLOWED_ORIGINS adds an origin",
			giveSettings:  func(s *gateway.Settings) { s.ExtraCORSAllowedOrigins = " https://test.com , ,https://other.com" },
			giveMethod:    http.MethodGet,
			giveHeaders:   map[string]string{headerOrigin: "https://other.com"},
			wantStatus:    http.StatusOK,
			wantOrigin:    "https://other.com",
			wantCredsTrue: true,
		},
		{
			name:         "EXTRA_CORS_ALLOWED_ORIGINS is exact about the scheme",
			giveSettings: func(s *gateway.Settings) { s.ExtraCORSAllowedOrigins = "https://test.com" },
			giveMethod:   http.MethodGet,
			giveHeaders:  map[string]string{headerOrigin: "http://test.com"},
			wantStatus:   http.StatusForbidden,
			wantNoCORS:   true,
		},
		{
			name:          "an S3 website origin on an internal domain and the listen port",
			giveMethod:    http.MethodGet,
			giveHeaders:   map[string]string{headerOrigin: "http://bucket.s3-website.localhost.localstack.cloud:4566"},
			wantStatus:    http.StatusOK,
			wantOrigin:    "http://bucket.s3-website.localhost.localstack.cloud:4566",
			wantCredsTrue: true,
		},
		{
			name:          "an S3 website origin without a port",
			giveMethod:    http.MethodGet,
			giveHeaders:   map[string]string{headerOrigin: "http://bucket.s3-website.localhost"},
			wantStatus:    http.StatusOK,
			wantOrigin:    "http://bucket.s3-website.localhost",
			wantCredsTrue: true,
		},
		{
			name:        "an S3 website origin on another port",
			giveMethod:  http.MethodGet,
			giveHeaders: map[string]string{headerOrigin: "http://bucket.s3-website.localhost:9999"},
			wantStatus:  http.StatusForbidden,
			wantNoCORS:  true,
		},
		{
			name:        "an S3 website origin on an outside domain",
			giveMethod:  http.MethodGet,
			giveHeaders: map[string]string{headerOrigin: "http://bucket.s3-website.example.com"},
			wantStatus:  http.StatusForbidden,
			wantNoCORS:  true,
		},
		{
			name:          "a preflight is answered 204 with the full lists",
			giveMethod:    http.MethodOptions,
			giveHeaders:   map[string]string{headerOrigin: appOrigin, headerRequestMethod: http.MethodPost},
			wantStatus:    http.StatusNoContent,
			wantOrigin:    appOrigin,
			wantCredsTrue: true,
		},
		{
			name:         "a forbidden preflight is 403",
			giveMethod:   http.MethodOptions,
			giveHeaders:  map[string]string{headerOrigin: badOrigin, headerRequestMethod: http.MethodPost},
			wantStatus:   http.StatusForbidden,
			wantNoCORS:   true,
			giveSettings: nil,
		},
		{
			name:       "OPTIONS without an Origin is 204 without CORS headers",
			giveMethod: http.MethodOptions,
			wantStatus: http.StatusNoContent,
			wantNoCORS: true,
		},
		{
			name:         "DISABLE_PREFLIGHT_PROCESSING hands OPTIONS on",
			giveSettings: func(s *gateway.Settings) { s.DisablePreflightProcessing = true },
			giveMethod:   http.MethodOptions,
			giveHeaders:  map[string]string{headerOrigin: appOrigin},
			wantStatus:   http.StatusOK,
			wantOrigin:   appOrigin,
			// the preflight is not answered here, so the service answers it
			wantCredsTrue: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mutate []func(*gateway.Settings)
			if tt.giveSettings != nil {
				mutate = append(mutate, tt.giveSettings)
			}

			g := newGateway(t, newSettings(mutate...), []service.Registration{sqsRegistration(okServe)})

			resp, _ := call(t, g.Handler(), corsRequest(t, tt.giveMethod, tt.giveHeaders))

			require.Equal(t, tt.wantStatus, resp.StatusCode)

			if tt.wantNoCORS {
				for _, name := range []string{allowOrigin, allowCredentials, allowMethods, allowHeaders, exposeHeaders} {
					assert.Empty(t, resp.Header.Values(name), name)
				}

				return
			}

			assert.Equal(t, []string{tt.wantOrigin}, resp.Header.Values(allowOrigin))
			assert.Equal(t, []string{"Origin"}, resp.Header.Values("Vary"))

			if tt.wantCredsTrue {
				assert.Equal(t, []string{valueTrue}, resp.Header.Values(allowCredentials))
			} else {
				assert.Empty(t, resp.Header.Values(allowCredentials))
			}

			methods := strings.Split(resp.Header.Get(allowMethods), ",")
			assert.ElementsMatch(t, []string{"HEAD", "GET", "PUT", "POST", "DELETE", "OPTIONS", "PATCH"}, methods)
			assert.Contains(t, strings.Split(resp.Header.Get(allowHeaders), ","), "authorization")
			assert.Contains(t, strings.Split(resp.Header.Get(allowHeaders), ","), "x-amz-target")
			assert.Equal(t, "etag,x-amz-version-id,x-amz-log-result,x-amz-executed-version,x-amz-function-error",
				resp.Header.Get(exposeHeaders))
		})
	}
}

func Test_Gateway_corsPreflightAnswersWithTheIdentificationHeaders(t *testing.T) {
	g := newGateway(t, newSettings(), []service.Registration{sqsRegistration(okServe)})

	resp, body := call(t, g.Handler(), corsRequest(t, http.MethodOptions, map[string]string{
		headerOrigin:                     appOrigin,
		headerRequestMethod:              http.MethodPost,
		"Access-Control-Request-Headers": "x-custom, content-type",
	}))

	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Empty(t, body)
	assert.Equal(t, []string{valueTrue}, rawHeader(resp.Header, "x-localstack"))
	assert.Equal(t, []string{valueTrue}, rawHeader(resp.Header, "x-gostack"))
	assert.NotContains(t, resp.Header.Get(allowHeaders), "x-custom",
		"LocalStack answers with its own list, whatever the request asks for")
}

func Test_Gateway_corsExtraHeaders(t *testing.T) {
	g := newGateway(t, newSettings(func(s *gateway.Settings) {
		s.ExtraCORSAllowedHeaders = "x-extra-allowed"
		s.ExtraCORSExposeHeaders = "x-extra-exposed"
	}), []service.Registration{sqsRegistration(okServe)})

	resp, _ := call(t, g.Handler(), corsRequest(t, http.MethodGet, map[string]string{
		headerOrigin:                             appOrigin,
		"Access-Control-Request-Private-Network": valueTrue,
	}))

	assert.Contains(t, strings.Split(resp.Header.Get(allowHeaders), ","), "x-extra-allowed")
	assert.True(t, strings.HasSuffix(resp.Header.Get(exposeHeaders), ",x-extra-exposed"))
	assert.Equal(t, valueTrue, resp.Header.Get("Access-Control-Allow-Private-Network"))
}

func Test_Gateway_corsFollowsTheSettings(t *testing.T) {
	settings := newSettings()
	g := newGateway(t, settings, []service.Registration{sqsRegistration(okServe)})
	origin := map[string]string{headerOrigin: badOrigin}

	resp, _ := call(t, g.Handler(), corsRequest(t, http.MethodGet, origin))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	settings.update(func(s *gateway.Settings) { s.DisableCORSChecks = true })

	resp, _ = call(t, g.Handler(), corsRequest(t, http.MethodGet, origin))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "*", resp.Header.Get(allowOrigin))
}
