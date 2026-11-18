package gateway_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
)

const (
	nameTwinA   = "twin-a"
	nameTwinB   = "twin-b"
	nameEdge    = "edge"
	signingTwin = "twin"
	functionURL = "http://abc.lambda-url.us-east-1.localhost.localstack.cloud:4566"
	localURL    = "http://localhost:4566"
	queuePath   = "/000000000000/q"
	mounted     = "mounted"
)

// twinRegistration is a query service signed as "twin" that claims the paths
// under prefix, the way s3control claims /v20180820 among the services signed
// s3.
func twinRegistration(name, prefix string) service.Registration {
	return service.Registration{
		Metadata: service.Metadata{
			Name: name, SigningName: signingTwin, APIVersion: "2020-01-01", Protocols: []protocol.Kind{protocol.KindQuery},
		},
		Serve: echoService,
		Claim: func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, prefix) },
	}
}

// edgeRegistration is a service that claims requests by host, the way Lambda
// claims its function URLs.
func edgeRegistration() service.Registration {
	return service.Registration{
		Metadata: service.Metadata{
			Name: nameEdge, SigningName: nameEdge, Protocols: []protocol.Kind{protocol.KindRestJSON},
		},
		Serve: echoService,
		ClaimHost: func(host string) bool {
			return strings.Contains(strings.ToLower(host), ".lambda-url.")
		},
	}
}

// Test_Gateway_claims routes requests that services claim: by path among the
// services that share a signing name (LocalStack's custom_signing_name_rules),
// and by host before anything else (its ServiceLoaderForDataPlane).
func Test_Gateway_claims(t *testing.T) {
	sqsReg := sqsRegistration(echoService)
	// An Action sqs does not have leaves the tie to the twins.
	sqsReg.Metadata.Operations = []string{opSendMessage}
	sqsReg.Mount = func(r chi.Router) {
		r.HandleFunc("/{account:[0-9]{12}}/{queue}", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, mounted)
		})
	}

	regs := []service.Registration{
		twinRegistration(nameTwinA, "/v1"), twinRegistration(nameTwinB, "/v2"), edgeRegistration(), sqsReg,
	}

	internal := func(r chi.Router) {
		r.Get("/_localstack/health", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "up")
		})
	}

	twinCall := func(t *testing.T, path string) *http.Request {
		t.Helper()

		return newRequest(t, http.MethodPost, localURL+path, "Action=Do",
			map[string]string{headerAuth: signed("test", usEast1, signingTwin), headerType: formType})
	}

	get := func(t *testing.T, target string) *http.Request {
		t.Helper()

		return newRequest(t, http.MethodGet, target, "", nil)
	}

	tests := []struct {
		name       string
		give       func(t *testing.T) *http.Request
		wantStatus int
		wantBody   string
	}{
		{
			name:       "the first twin claims its path",
			give:       func(t *testing.T) *http.Request { t.Helper(); return twinCall(t, "/v1/things") },
			wantStatus: http.StatusOK,
			wantBody:   nameTwinA,
		},
		{
			name:       "the second twin claims its path",
			give:       func(t *testing.T) *http.Request { t.Helper(); return twinCall(t, "/v2/things") },
			wantStatus: http.StatusOK,
			wantBody:   nameTwinB,
		},
		{
			name:       "a path neither twin claims is ambiguous",
			give:       func(t *testing.T) *http.Request { t.Helper(); return twinCall(t, "/") },
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error": "Bad Request", "message": "ambiguous request: claimed by twin-a, twin-b"}`,
		},
		{
			name: "a claimed host wins over the signing name",
			give: func(t *testing.T) *http.Request {
				t.Helper()

				return newRequest(t, http.MethodPost, functionURL+"/", "{}", map[string]string{
					headerAuth: signed("test", usEast1, nameSQS), headerType: jsonType, headerTarget: targetSendMessage,
				})
			},
			wantStatus: http.StatusOK,
			wantBody:   nameEdge,
		},
		{
			name:       "a claimed host wins over a path another service mounts",
			give:       func(t *testing.T) *http.Request { t.Helper(); return get(t, functionURL+queuePath) },
			wantStatus: http.StatusOK,
			wantBody:   nameEdge,
		},
		{
			name:       "the mounted path is the mount's on any other host",
			give:       func(t *testing.T) *http.Request { t.Helper(); return get(t, localURL+queuePath) },
			wantStatus: http.StatusOK,
			wantBody:   mounted,
		},
		{
			name:       "a claimed host keeps the internal routes",
			give:       func(t *testing.T) *http.Request { t.Helper(); return get(t, functionURL+"/_localstack/health") },
			wantStatus: http.StatusOK,
			wantBody:   "up",
		},
	}

	orders := map[string][]service.Registration{
		"in order": regs,
		"reversed": {regs[3], regs[2], regs[1], regs[0]},
	}

	for order, ordered := range orders {
		g := newGateway(t, newSettings(), ordered, internal)

		for _, tt := range tests {
			t.Run(order+", "+tt.name, func(t *testing.T) {
				resp, body := call(t, g.Handler(), tt.give(t))

				assert.Equal(t, tt.wantStatus, resp.StatusCode)

				if strings.HasPrefix(tt.wantBody, "{") {
					assert.JSONEq(t, tt.wantBody, body)

					return
				}

				assert.Equal(t, tt.wantBody, body)
			})
		}
	}
}
