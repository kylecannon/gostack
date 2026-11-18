package route_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/route"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	nameS3         = "s3"
	nameS3Control  = "s3control"
	nameLambda     = "lambda"
	nameKinesis    = "kinesis"
	nameAPIGateway = "apigateway"
	pathObject     = "/bucket/key"
	pathControl    = "/v20180820/configuration/publicAccessBlock"
	vhostSQS       = "sqs.s3.localhost.localstack.cloud:4566"
	presignedV4    = "?X-Amz-Algorithm=AWS4-HMAC-SHA256" +
		"&X-Amz-Credential=test%2F20130524%2Fus-east-1%2Fs3%2Faws4_request" +
		"&X-Amz-Date=20130524T000000Z&X-Amz-Expires=3600&X-Amz-SignedHeaders=host&X-Amz-Signature=abc"
	presignedV2 = "?AWSAccessKeyId=test&Expires=1700000000&Signature=abc"
)

// legacyS3 is a fake of S3's Fallback: three of LocalStack's legacy_s3_rules
// (service_router.py:232-297), enough for the cases below.
func legacyS3(r *http.Request) bool {
	stripped := strings.Trim(r.URL.Path, "/")

	switch {
	case (r.Method == http.MethodGet || r.Method == http.MethodHead) && stripped != "":
		// :244, a GET or HEAD of /<bucket>/<key...>.
		return true
	case r.Method == http.MethodPut && stripped != "" && !strings.Contains(stripped, "/"):
		// :250, a PUT of /<bucket>.
		return true
	default:
		// :295, uses_host_addressing, the last rule.
		return strings.Contains(r.Host, ".s3.")
	}
}

// s3Registration is a fake s3: the default of signing name s3, with the
// legacy rules as its Fallback.
func s3Registration() service.Registration {
	return service.Registration{
		Metadata: service.Metadata{
			Name: nameS3, SigningName: nameS3, EndpointPrefix: nameS3, APIVersion: "2006-03-01",
			Protocols: []protocol.Kind{protocol.KindRestXML},
		},
		SigningDefault: true,
		Fallback:       legacyS3,
	}
}

// s3ControlRegistration is a fake s3control, which shares s3's signing name and
// claims the /v20180820 paths (service_router.py:168).
func s3ControlRegistration() service.Registration {
	return service.Registration{
		Metadata: service.Metadata{
			Name: nameS3Control, SigningName: nameS3, EndpointPrefix: "s3-control", APIVersion: "2018-08-20",
			Protocols: []protocol.Kind{protocol.KindRestXML},
		},
		Claim: func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/v20180820") },
	}
}

func s3Request(t *testing.T, method, host, target, authorization string) *http.Request {
	t.Helper()

	r := httptest.NewRequestWithContext(t.Context(), method, "http://"+host+target, nil)
	if authorization != "" {
		r.Header.Set(headerAuth, authorization)
	}

	return r
}

// Test_Table_Detect_s3 checks each way LocalStack routes an S3 call, with s3,
// s3control, sqs and sts all registered.
func Test_Table_Detect_s3(t *testing.T) {
	signedS3 := scope(nameS3)

	tests := []struct {
		name        string
		giveMethod  string
		giveHost    string
		giveTarget  string
		giveAuth    string
		giveForm    string
		wantService string
	}{
		{
			// service_router.py:204, the signing name s3 with no rule matching.
			name: "a signed GET of an object", giveMethod: http.MethodGet, giveHost: localHost,
			giveTarget: pathObject, giveAuth: signedS3, wantService: nameS3,
		},
		{
			name: "a signed GET of /v20180820 is s3control", giveMethod: http.MethodGet, giveHost: localHost,
			giveTarget: pathControl, giveAuth: signedS3, wantService: nameS3Control,
		},
		{
			// The router reads no signing name from X-Amz-Credential
			// (service_router.py:64-77); the Fallback takes it at :244.
			name: "a presigned GET", giveMethod: http.MethodGet, giveHost: localHost,
			giveTarget: pathObject + presignedV4, wantService: nameS3,
		},
		{
			name: "a presigned GET of /v20180820 is s3's", giveMethod: http.MethodGet, giveHost: localHost,
			giveTarget: pathControl + presignedV4, wantService: nameS3,
		},
		{
			name: "a presigned GET of a bucket that starts v20180820 is not s3control's", giveMethod: http.MethodGet,
			giveHost: localHost, giveTarget: "/v20180820abc/k" + presignedV4, wantService: nameS3,
		},
		{
			name: "a presigned GET with an sqs scope is not sqs's", giveMethod: http.MethodGet, giveHost: localHost,
			giveTarget: pathObject + "?X-Amz-Credential=test%2F20130524%2Fus-east-1%2Fsqs%2Faws4_request" +
				"&X-Amz-Signature=x",
			wantService: nameS3,
		},
		{
			name: "a SigV2 header keeps an sqs scope in the query out", giveMethod: http.MethodGet,
			giveHost: localHost, giveAuth: "AWS test:sig",
			giveTarget:  pathObject + "?X-Amz-Credential=test%2F20130524%2Fus-east-1%2Fsqs%2Faws4_request",
			wantService: nameS3,
		},
		{
			// SigV2 has no signing name: only the Fallback can take it.
			name: "a presigned SigV2 GET", giveMethod: http.MethodGet, giveHost: localHost,
			giveTarget: pathObject + presignedV2, wantService: nameS3,
		},
		{
			name: "an unsigned GET of an object", giveMethod: http.MethodGet, giveHost: localHost,
			giveTarget: pathObject, wantService: nameS3,
		},
		{
			name: "an unsigned PUT of a bucket", giveMethod: http.MethodPut, giveHost: localHost,
			giveTarget: "/bucket", wantService: nameS3,
		},
		{
			// :428, a bucket named sqs is not the sqs endpoint; :295 makes it s3.
			name: "a virtual-host bucket named after another service", giveMethod: http.MethodGet,
			giveHost: vhostSQS, giveTarget: "/", wantService: nameS3,
		},
		{
			name: "a signed sqs call is not the Fallback's", giveMethod: http.MethodGet, giveHost: localHost,
			giveTarget: pathObject, giveAuth: scope(nameSQS), wantService: nameSQS,
		},
		{
			name: "an unsigned form call is decided before the Fallback", giveMethod: http.MethodPost,
			giveHost: localHost, giveTarget: "/", giveForm: "Action=GetCallerIdentity&Version=2011-06-15",
			wantService: nameSTS,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			table, err := route.NewTable([]service.Registration{
				{Metadata: sqs}, {Metadata: sts}, s3ControlRegistration(), s3Registration(),
			})
			require.NoError(t, err)

			got, err := table.Detect(s3Request(t, tt.giveMethod, tt.giveHost, tt.giveTarget, tt.giveAuth),
				formOf(tt.giveForm))
			require.NoError(t, err)

			assert.Equal(t, tt.wantService, got.Service)
		})
	}
}

// Test_Table_Detect_s3VirtualHostGuard covers service_router.py:428: a host
// that contains ".s3." never matches another service's endpoint prefix.
func Test_Table_Detect_s3VirtualHostGuard(t *testing.T) {
	table, err := route.NewTable(registered(sqs, sts))
	require.NoError(t, err)

	for _, host := range []string{vhostSQS, "SQS.S3.localhost.localstack.cloud", "sts.s3.us-east-1.amazonaws.com"} {
		t.Run(host, func(t *testing.T) {
			_, detectErr := table.Detect(s3Request(t, http.MethodGet, host, "/key", ""), formOf(""))
			require.ErrorIs(t, detectErr, route.ErrNoService)
		})
	}

	got, err := table.Detect(s3Request(t, http.MethodGet, "sqs.s3-website.localhost", "/", ""), nil)
	require.NoError(t, err)
	assert.Equal(t, nameSQS, got.Service, "only the label .s3. is guarded")
}

func Test_Table_Detect_fallback(t *testing.T) {
	claimAll := func(name string, asked *int) service.Registration {
		return service.Registration{
			Metadata: service.Metadata{Name: name, Protocols: []protocol.Kind{protocol.KindRestJSON}},
			Fallback: func(*http.Request) bool { *asked++; return true },
		}
	}

	t.Run("the first in registration order wins", func(t *testing.T) {
		var askedFirst, askedSecond int

		for _, order := range [][]string{{nameTwinA, nameTwinB}, {nameTwinB, nameTwinA}} {
			regs := []service.Registration{claimAll(order[0], &askedFirst), claimAll(order[1], &askedSecond)}

			table, err := route.NewTable(regs)
			require.NoError(t, err)

			got, err := table.Detect(s3Request(t, http.MethodGet, localHost, "/", ""), nil)
			require.NoError(t, err)
			assert.Equal(t, order[0], got.Service)
		}

		assert.Equal(t, 2, askedFirst)
		assert.Zero(t, askedSecond, "a later Fallback is not asked once one claims")
	})

	t.Run("it is not asked when an earlier step decides", func(t *testing.T) {
		var asked int

		table, err := route.NewTable([]service.Registration{{Metadata: sqs}, claimAll(nameEdge, &asked)})
		require.NoError(t, err)

		got, err := table.Detect(s3Request(t, http.MethodPost, localHost, "/", scope(nameSQS)), nil)
		require.NoError(t, err)
		assert.Equal(t, nameSQS, got.Service)
		assert.Zero(t, asked)
	})

	t.Run("it wins over services the earlier steps leave tied", func(t *testing.T) {
		// legacy_s3_rules (:491) runs before the signing-name and candidate
		// fallbacks that would otherwise pick among the tied ones.
		var asked int

		table, err := route.NewTable([]service.Registration{
			twin(nameTwinA, pathV1), twin(nameTwinB, pathV2), claimAll(nameEdge, &asked),
		})
		require.NoError(t, err)

		got, err := table.Detect(signedRequest(t, localHost, "/", signingTwin), nil)
		require.NoError(t, err)
		assert.Equal(t, nameEdge, got.Service)
	})

	t.Run("a tie stays one when no Fallback claims", func(t *testing.T) {
		never := service.Registration{
			Metadata: service.Metadata{Name: nameEdge, Protocols: []protocol.Kind{protocol.KindRestJSON}},
			Fallback: func(*http.Request) bool { return false },
		}

		table, err := route.NewTable([]service.Registration{twin(nameTwinA, pathV1), twin(nameTwinB, pathV2), never})
		require.NoError(t, err)

		_, err = table.Detect(signedRequest(t, localHost, "/", signingTwin), nil)

		var ambiguous *route.AmbiguousError
		require.ErrorAs(t, err, &ambiguous)
		assert.Equal(t, []string{nameTwinA, nameTwinB}, ambiguous.Services)
	})
}

// Test_Table_Detect_unregisteredSigningName covers service_router.py:374-379: a
// call signed for a service LocalStack has and gostack does not run is that
// service's, so the gateway answers that it is not enabled. A Fallback that
// claims every request never sees it.
func Test_Table_Detect_unregisteredSigningName(t *testing.T) {
	const awsCLI = "aws-cli/2.15.0 Python/3.11"

	tests := []struct {
		name        string
		giveMethod  string
		giveTarget  string
		giveSigning string
		giveHeaders map[string]string
		wantService string
	}{
		{
			name: nameLambda, giveMethod: http.MethodGet, giveTarget: "/2015-03-31/functions/f",
			giveSigning: nameLambda, wantService: nameLambda,
		},
		{
			// dynamodb shares its signing name with dynamodbstreams; the target
			// tells them apart.
			name: "dynamodb through the aws cli", giveMethod: http.MethodPost, giveTarget: "/",
			giveSigning: nameDynamoDB,
			giveHeaders: map[string]string{
				headerTarget: targetListTables, headerType: "application/x-amz-json-1.0", "User-Agent": awsCLI,
			},
			wantService: nameDynamoDB,
		},
		{name: nameKinesis, giveMethod: http.MethodGet, giveTarget: "/x", giveSigning: nameKinesis, wantService: nameKinesis},
		{
			// apigatewayv2 shares the signing name and nothing tells them apart:
			// the service named after it (service_router.py:496-497).
			name: nameAPIGateway, giveMethod: http.MethodGet, giveTarget: "/restapis",
			giveSigning: nameAPIGateway, wantService: nameAPIGateway,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var asked int

			s3 := s3Registration()
			s3.Fallback = func(*http.Request) bool { asked++; return true }

			table, err := route.NewTable([]service.Registration{{Metadata: sqs}, {Metadata: sts}, s3ControlRegistration(), s3})
			require.NoError(t, err)

			r := s3Request(t, tt.giveMethod, localHost, tt.giveTarget, scope(tt.giveSigning))
			for name, value := range tt.giveHeaders {
				r.Header.Set(name, value)
			}

			_, err = table.Detect(r, formOf(""))
			require.ErrorIs(t, err, route.ErrNoService)
			assert.Zero(t, asked, "the Fallback is not asked")

			got, found := table.Unregistered(r)
			require.True(t, found)
			assert.Equal(t, tt.wantService, got.Service)
		})
	}

	t.Run("a target of a registered service still finds it", func(t *testing.T) {
		table, err := route.NewTable([]service.Registration{{Metadata: sqs}, {Metadata: sts}, s3Registration()})
		require.NoError(t, err)

		r := s3Request(t, http.MethodPost, localHost, "/", scope(nameDynamoDB))
		r.Header.Set(headerTarget, targetSendMessage)
		r.Header.Set(headerType, "application/x-amz-json-1.0")

		got, err := table.Detect(r, formOf(""))
		require.NoError(t, err)
		assert.Equal(t, nameSQS, got.Service)
	})
}

func Test_Table_Detect_signingDefault(t *testing.T) {
	asDefault := func(reg service.Registration) service.Registration {
		reg.SigningDefault = true

		return reg
	}

	tests := []struct {
		name         string
		giveRegs     []service.Registration
		givePath     string
		wantService  string
		wantServices []string
	}{
		{
			name:        "the default takes a path no Claim claims",
			giveRegs:    []service.Registration{asDefault(twin(nameTwinA, pathV1)), twin(nameTwinB, pathV2)},
			givePath:    "/",
			wantService: nameTwinA,
		},
		{
			name:        "a Claim wins over the default",
			giveRegs:    []service.Registration{asDefault(twin(nameTwinA, pathV1)), twin(nameTwinB, pathV2)},
			givePath:    pathV2,
			wantService: nameTwinB,
		},
		{
			name:        "the default's own Claim still counts",
			giveRegs:    []service.Registration{asDefault(twin(nameTwinA, pathV1)), twin(nameTwinB, pathV2)},
			givePath:    pathV1,
			wantService: nameTwinA,
		},
		{
			name:         "two Claims are a tie the default does not break",
			giveRegs:     []service.Registration{asDefault(twin(nameTwinA, "/")), twin(nameTwinB, "/")},
			givePath:     pathV1,
			wantServices: []string{nameTwinA, nameTwinB},
		},
	}

	for _, tt := range tests {
		for _, swap := range []bool{false, true} {
			t.Run(tt.name+", swapped "+strconv.FormatBool(swap), func(t *testing.T) {
				regs := tt.giveRegs
				if swap {
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
			})
		}
	}
}

func Test_NewTable_twoSigningDefaults(t *testing.T) {
	a, b := twin(nameTwinA, pathV1), twin(nameTwinB, pathV2)
	a.SigningDefault, b.SigningDefault = true, true

	assert.PanicsWithValue(t,
		`services "twin-a" and "twin-b" are both the SigningDefault of signing name "twin"`,
		func() { _, _ = route.NewTable([]service.Registration{a, b}) })

	b.Metadata.SigningName = nameS3

	_, err := route.NewTable([]service.Registration{a, b})
	require.NoError(t, err, "one default per signing name")
}
