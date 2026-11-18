package route_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/route"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	nameTwinA     = "twin-a"
	nameTwinB     = "twin-b"
	nameEdge      = "edge"
	signingTwin   = "twin"
	functionURL   = "abc.lambda-url.us-east-1.localhost.localstack.cloud:4566"
	pathV1        = "/v1/things"
	pathV2        = "/v2/things"
	lambdaURLPart = ".lambda-url."
)

// twin is a query service signed as "twin" that claims the paths under prefix,
// as s3control claims /v20180820 among the services signed s3.
func twin(name, prefix string) service.Registration {
	return service.Registration{
		Metadata: service.Metadata{
			Name: name, SigningName: signingTwin, APIVersion: "2020-01-01", Protocols: []protocol.Kind{protocol.KindQuery},
		},
		Claim: func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, prefix) },
	}
}

// edge is a service that claims requests by host, as Lambda claims its
// function URLs.
func edge() service.Registration {
	return service.Registration{
		Metadata: service.Metadata{
			Name: nameEdge, SigningName: nameEdge, Protocols: []protocol.Kind{protocol.KindRestJSON},
		},
		ClaimHost: func(host string) bool { return strings.Contains(strings.ToLower(host), lambdaURLPart) },
	}
}

func signedRequest(t *testing.T, host, path, signingName string) *http.Request {
	t.Helper()

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+host+path, nil)
	if signingName != "" {
		r.Header.Set(headerAuth, scope(signingName))
	}

	r.Header.Set(headerType, "application/x-www-form-urlencoded")

	return r
}

func Test_Table_Detect_claimBreaksASigningNameTie(t *testing.T) {
	alwaysA := twin(nameTwinA, pathV1)
	everything := twin(nameTwinB, "/")

	tests := []struct {
		name         string
		giveRegs     []service.Registration
		givePath     string
		wantService  string
		wantServices []string
	}{
		{
			name:        "the first twin claims its path",
			giveRegs:    []service.Registration{twin(nameTwinA, "/v1"), twin(nameTwinB, "/v2")},
			givePath:    pathV1,
			wantService: nameTwinA,
		},
		{
			name:        "the second twin claims its path",
			giveRegs:    []service.Registration{twin(nameTwinA, "/v1"), twin(nameTwinB, "/v2")},
			givePath:    pathV2,
			wantService: nameTwinB,
		},
		{
			name:         "a path neither claims is still ambiguous",
			giveRegs:     []service.Registration{twin(nameTwinA, "/v1"), twin(nameTwinB, "/v2")},
			givePath:     "/",
			wantServices: []string{nameTwinA, nameTwinB},
		},
		{
			name:         "a path both claim is ambiguous",
			giveRegs:     []service.Registration{alwaysA, everything},
			givePath:     pathV1,
			wantServices: []string{nameTwinA, nameTwinB},
		},
		{
			name:        "a twin without a claim loses to one that claims",
			giveRegs:    []service.Registration{{Metadata: alwaysA.Metadata}, twin(nameTwinB, "/v2")},
			givePath:    pathV2,
			wantService: nameTwinB,
		},
	}

	for _, tt := range tests {
		for _, order := range []string{"in order", "reversed"} {
			t.Run(tt.name+", "+order, func(t *testing.T) {
				regs := tt.giveRegs
				if order == "reversed" {
					regs = []service.Registration{regs[1], regs[0]}
				}

				table, err := route.NewTable(regs)
				require.NoError(t, err)

				match, err := table.Detect(signedRequest(t, localHost, tt.givePath, signingTwin), nil)

				if tt.wantServices != nil {
					var ambiguous *route.AmbiguousError
					require.ErrorAs(t, err, &ambiguous)
					assert.Equal(t, tt.wantServices, ambiguous.Services)

					return
				}

				require.NoError(t, err)
				assert.Equal(t, tt.wantService, match.Service)
				assert.Equal(t, tt.wantService, regs[match.Index].Metadata.Name, "Index points into the registrations")
			})
		}
	}
}

func Test_Table_Detect_asksClaimOnlyOnATie(t *testing.T) {
	asked := 0
	reg := service.Registration{Metadata: sqs, Claim: func(*http.Request) bool { asked++; return false }}

	table, err := route.NewTable([]service.Registration{reg, {Metadata: sts}})
	require.NoError(t, err)

	match, err := table.Detect(signedRequest(t, localHost, "/", nameSQS), nil)
	require.NoError(t, err)
	assert.Equal(t, nameSQS, match.Service, "a signing name of its own decides without the claim")
	assert.Zero(t, asked)
}

func Test_Table_Claim(t *testing.T) {
	other := service.Registration{
		Metadata:  service.Metadata{Name: "rival", Protocols: []protocol.Kind{protocol.KindRestJSON}},
		ClaimHost: func(host string) bool { return strings.HasPrefix(host, "both.") },
	}
	greedy := edge()
	greedy.ClaimHost = func(host string) bool {
		return strings.Contains(host, lambdaURLPart) || strings.HasPrefix(host, "both.")
	}

	tests := []struct {
		name        string
		giveRegs    []service.Registration
		giveHost    string
		wantService string
	}{
		{
			name:        "the host is the service's",
			giveRegs:    []service.Registration{{Metadata: sqs}, edge()},
			giveHost:    functionURL,
			wantService: nameEdge,
		},
		{
			name:     "a host nobody claims",
			giveRegs: []service.Registration{{Metadata: sqs}, edge()},
			giveHost: localHost,
		},
		{
			name:     "no service claims hosts",
			giveRegs: registered(sqs, sts),
			giveHost: functionURL,
		},
		{
			name:     "a host two services claim is nobody's",
			giveRegs: []service.Registration{greedy, other},
			giveHost: "both.localhost.localstack.cloud:4566",
		},
		{
			name:        "the other host of the greedy one is still its own",
			giveRegs:    []service.Registration{greedy, other},
			giveHost:    functionURL,
			wantService: nameEdge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			table, err := route.NewTable(tt.giveRegs)
			require.NoError(t, err)

			// The signature names sqs, which the host overrides.
			match, err := table.Claim(signedRequest(t, tt.giveHost, "/", nameSQS))

			if tt.wantService == "" {
				require.ErrorIs(t, err, route.ErrNoService)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantService, match.Service)
			assert.Equal(t, protocol.KindRestJSON, match.Protocol)
		})
	}
}
