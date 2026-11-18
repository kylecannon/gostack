package route_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/route"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	nameSQS  = "sqs"
	nameSTS  = "sts"
	nameSNS  = "sns"
	nameTwin = "sqs-twin"

	nameDynamoDB          = "dynamodb"
	localHost             = "localhost:4566"
	targetListTables      = "DynamoDB_20120810.ListTables"
	formListQueues        = "Action=ListQueues"
	formGetCallerIdentity = "Action=GetCallerIdentity"

	headerAuth   = "Authorization"
	headerTarget = "X-Amz-Target"
	headerType   = "Content-Type"

	targetSendMessage = "AmazonSQS.SendMessage"
	targetListQueues  = "AmazonSQS.ListQueues"
	opSendMessage     = "SendMessage"
)

var (
	sqs = service.Metadata{
		Name: nameSQS, SigningName: nameSQS, TargetPrefix: "AmazonSQS", EndpointPrefix: nameSQS,
		APIVersion: "2012-11-05", Protocols: []protocol.Kind{protocol.KindJSON, protocol.KindQuery},
	}
	sts = service.Metadata{
		Name: nameSTS, SigningName: nameSTS, EndpointPrefix: nameSTS,
		APIVersion: "2011-06-15", Protocols: []protocol.Kind{protocol.KindQuery},
	}
	sns = service.Metadata{
		Name: nameSNS, SigningName: nameSNS, EndpointPrefix: nameSNS,
		APIVersion: "2010-03-31", Protocols: []protocol.Kind{protocol.KindQuery},
	}
)

// registered wraps metadata in Registrations without claims.
func registered(metas ...service.Metadata) []service.Registration {
	regs := make([]service.Registration, len(metas))
	for i, meta := range metas {
		regs[i] = service.Registration{Metadata: meta}
	}

	return regs
}

func scope(signingName string) string {
	return "AWS4-HMAC-SHA256 Credential=test/20130524/us-east-1/" + signingName +
		"/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc"
}

func newRequest(t *testing.T, host string, headers map[string]string) *http.Request {
	t.Helper()

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+host+"/", nil)
	for name, value := range headers {
		r.Header.Set(name, value)
	}

	return r
}

func formOf(values string) func() url.Values {
	return func() url.Values {
		parsed, err := url.ParseQuery(values)
		if err != nil {
			panic(err)
		}

		return parsed
	}
}

func Test_Detect(t *testing.T) {
	const (
		formType = "application/x-www-form-urlencoded; charset=utf-8"
		jsonType = "application/x-amz-json-1.0"
		anyHost  = localHost
	)

	tests := []struct {
		name         string
		giveServices []service.Metadata
		giveHost     string
		giveHeaders  map[string]string
		giveForm     string
		wantService  string
		wantProtocol protocol.Kind
	}{
		{
			name:         "signing name and target prefix pick sqs json",
			giveServices: []service.Metadata{sqs, sts, sns},
			giveHost:     anyHost,
			giveHeaders: map[string]string{
				headerAuth: scope(nameSQS), headerTarget: targetSendMessage, headerType: jsonType,
			},
			wantService:  nameSQS,
			wantProtocol: protocol.KindJSON,
		},
		{
			name:         "signing name and a form body pick sqs query",
			giveServices: []service.Metadata{sqs, sts, sns},
			giveHost:     anyHost,
			giveHeaders:  map[string]string{headerAuth: scope(nameSQS), headerType: formType},
			giveForm:     "Action=SendMessage&Version=2012-11-05",
			wantService:  nameSQS,
			wantProtocol: protocol.KindQuery,
		},
		{
			name:         "signing name picks sts",
			giveServices: []service.Metadata{sqs, sts, sns},
			giveHost:     anyHost,
			giveHeaders:  map[string]string{headerAuth: scope(nameSTS), headerType: formType},
			wantService:  nameSTS,
			wantProtocol: protocol.KindQuery,
		},
		{
			name:         "scheme name is case insensitive",
			giveServices: []service.Metadata{sqs, sts},
			giveHost:     anyHost,
			giveHeaders: map[string]string{
				headerAuth: strings.Replace(scope(nameSTS), "AWS4-HMAC-SHA256", "aws4-hmac-sha256", 1),
			},
			wantService:  nameSTS,
			wantProtocol: protocol.KindQuery,
		},
		{
			name:         "target prefix alone picks sqs json",
			giveServices: []service.Metadata{sqs, sts},
			giveHost:     anyHost,
			giveHeaders:  map[string]string{headerTarget: targetListQueues},
			wantService:  nameSQS,
			wantProtocol: protocol.KindJSON,
		},
		{
			name:         "signing name wins over a conflicting target",
			giveServices: []service.Metadata{sqs, sts},
			giveHost:     anyHost,
			giveHeaders:  map[string]string{headerAuth: scope(nameSTS), headerTarget: targetListQueues},
			wantService:  nameSTS,
			wantProtocol: protocol.KindQuery,
		},
		{
			name:         "host picks sqs for a form request",
			giveServices: []service.Metadata{sqs, sts, sns},
			giveHost:     "sqs.us-east-1.localhost.localstack.cloud:4566",
			giveHeaders:  map[string]string{headerType: formType},
			wantService:  nameSQS,
			wantProtocol: protocol.KindQuery,
		},
		{
			name:         "host picks sts",
			giveServices: []service.Metadata{sqs, sts, sns},
			giveHost:     "STS.localhost.localstack.cloud",
			giveHeaders:  map[string]string{headerType: formType},
			wantService:  nameSTS,
			wantProtocol: protocol.KindQuery,
		},
		{
			name:         "host prefix needs the dot",
			giveServices: []service.Metadata{sqs, sts},
			giveHost:     "sqsx.localhost.localstack.cloud",
			giveHeaders:  map[string]string{headerType: formType},
			giveForm:     "Action=GetCallerIdentity&Version=2011-06-15",
			wantService:  nameSTS,
			wantProtocol: protocol.KindQuery,
		},
		{
			name:         "Version breaks the tie between query services",
			giveServices: []service.Metadata{sqs, sts, sns},
			giveHost:     anyHost,
			giveHeaders:  map[string]string{headerType: formType},
			giveForm:     "Action=AssumeRoleWithWebIdentity&Version=2011-06-15",
			wantService:  nameSTS,
			wantProtocol: protocol.KindQuery,
		},
		{
			name:         "an Action alone picks the only query service",
			giveServices: []service.Metadata{sts},
			giveHost:     anyHost,
			giveHeaders:  map[string]string{headerType: formType},
			giveForm:     formGetCallerIdentity,
			wantService:  nameSTS,
			wantProtocol: protocol.KindQuery,
		},
		{
			name:         "a json content type without a target is json",
			giveServices: []service.Metadata{sqs, sts},
			giveHost:     anyHost,
			giveHeaders:  map[string]string{headerAuth: scope(nameSQS), headerType: jsonType},
			wantService:  nameSQS,
			wantProtocol: protocol.KindJSON,
		},
		{
			name:         "no content type without a target is query",
			giveServices: []service.Metadata{sqs, sts},
			giveHost:     anyHost,
			giveHeaders:  map[string]string{headerAuth: scope(nameSQS)},
			wantService:  nameSQS,
			wantProtocol: protocol.KindQuery,
		},
		{
			name:         "a target for another service does not make json",
			giveServices: []service.Metadata{sqs, sts},
			giveHost:     anyHost,
			giveHeaders:  map[string]string{headerAuth: scope(nameSQS), headerTarget: "Other.Op", headerType: formType},
			wantService:  nameSQS,
			wantProtocol: protocol.KindQuery,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			table, err := route.NewTable(registered(tt.giveServices...))
			require.NoError(t, err)

			got, err := table.Detect(newRequest(t, tt.giveHost, tt.giveHeaders), formOf(tt.giveForm))
			require.NoError(t, err)

			assert.Equal(t, tt.wantService, got.Service)
			assert.Equal(t, tt.wantProtocol, got.Protocol)
			assert.Equal(t, tt.giveServices[got.Index].Name, got.Service)
		})
	}
}

func Test_Detect_errors(t *testing.T) {
	const formType = "application/x-www-form-urlencoded"

	twin := service.Metadata{
		Name: nameTwin, SigningName: nameSQS, APIVersion: "2012-11-05",
		Protocols: []protocol.Kind{protocol.KindQuery},
	}
	sameTarget := service.Metadata{
		Name: "other", TargetPrefix: "AmazonSQS", Protocols: []protocol.Kind{protocol.KindJSON},
	}
	noHint := service.Metadata{
		Name: "x", SigningName: "x", Protocols: []protocol.Kind{protocol.KindJSON, protocol.KindRestXML},
	}
	jsonOnly := service.Metadata{
		Name: nameDynamoDB, SigningName: nameDynamoDB, TargetPrefix: "DynamoDB_20120810",
		Protocols: []protocol.Kind{protocol.KindJSON, protocol.KindRestJSON},
	}

	tests := []struct {
		name         string
		giveServices []service.Metadata
		giveHeaders  map[string]string
		giveForm     string
		wantErr      error
		wantServices []string
	}{
		{
			name:         "an Action without Version is ambiguous between query services",
			giveServices: []service.Metadata{sqs, sts, sns},
			giveHeaders:  map[string]string{headerType: formType},
			giveForm:     formGetCallerIdentity,
			wantServices: []string{nameSNS, nameSQS, nameSTS},
		},
		{
			name:         "an unknown Version leaves nothing",
			giveServices: []service.Metadata{sqs, sts},
			giveHeaders:  map[string]string{headerType: formType},
			giveForm:     "Action=GetCallerIdentity&Version=1999-01-01",
			wantErr:      route.ErrNoService,
		},
		{
			name:         "two services with one signing name and no other hint",
			giveServices: []service.Metadata{sqs, twin},
			giveHeaders:  map[string]string{headerAuth: scope(nameSQS)},
			wantServices: []string{nameSQS, nameTwin},
		},
		{
			name:         "the same collision with the registrations swapped",
			giveServices: []service.Metadata{twin, sqs},
			giveHeaders:  map[string]string{headerAuth: scope(nameSQS)},
			wantServices: []string{nameSQS, nameTwin},
		},
		{
			name:         "two services with one target prefix",
			giveServices: []service.Metadata{sqs, sameTarget},
			giveHeaders:  map[string]string{headerTarget: targetListQueues},
			wantServices: []string{"other", nameSQS},
		},
		{
			name:         "no hints at all",
			giveServices: []service.Metadata{sqs, sts},
			wantErr:      route.ErrNoService,
		},
		{
			name:         "an unknown signing name",
			giveServices: []service.Metadata{sqs, sts},
			giveHeaders:  map[string]string{headerAuth: scope("s3")},
			wantErr:      route.ErrNoService,
		},
		{
			name:         "a malformed credential scope",
			giveServices: []service.Metadata{sqs, sts},
			giveHeaders:  map[string]string{headerAuth: "AWS4-HMAC-SHA256 Credential=test/20130524/us-east-1/sqs"},
			wantErr:      route.ErrNoService,
		},
		{
			name:         "a target without a dot names no prefix",
			giveServices: []service.Metadata{sqs, sts},
			giveHeaders:  map[string]string{headerTarget: opSendMessage},
			wantErr:      route.ErrNoService,
		},
		{
			name:         "a service with no query protocol and no json hint",
			giveServices: []service.Metadata{jsonOnly, noHint},
			giveHeaders:  map[string]string{headerAuth: scope("x")},
			wantErr:      route.ErrProtocol,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			table, err := route.NewTable(registered(tt.giveServices...))
			require.NoError(t, err)

			_, err = table.Detect(newRequest(t, localHost, tt.giveHeaders), formOf(tt.giveForm))
			require.Error(t, err)

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)

				return
			}

			var ambiguous *route.AmbiguousError

			require.ErrorAs(t, err, &ambiguous)
			assert.Equal(t, tt.wantServices, ambiguous.Services)
		})
	}
}

func Test_Detect_formIsLazy(t *testing.T) {
	table, err := route.NewTable(registered(sqs, sts))
	require.NoError(t, err)

	calls := 0
	form := func() url.Values {
		calls++

		return nil
	}

	_, err = table.Detect(newRequest(t, localHost, map[string]string{headerAuth: scope(nameSTS)}), form)
	require.NoError(t, err)
	assert.Zero(t, calls, "a signing name decides, so the body is not read")

	_, err = table.Detect(newRequest(t, localHost, map[string]string{headerTarget: targetListQueues}), form)
	require.NoError(t, err)
	assert.Zero(t, calls, "a target prefix decides, so the body is not read")

	_, err = table.Detect(newRequest(t, "sqs.localhost", nil), form)
	require.NoError(t, err)
	assert.Zero(t, calls, "the host decides, so the body is not read")

	_, err = table.Detect(newRequest(t, "localhost", nil), form)
	require.ErrorIs(t, err, route.ErrNoService)
	assert.Equal(t, 1, calls)
}

func Test_NewTable(t *testing.T) {
	tests := []struct {
		name string
		give []service.Metadata
		want string
	}{
		{
			name: "no name",
			give: []service.Metadata{{Protocols: []protocol.Kind{protocol.KindQuery}}},
			want: "no name",
		},
		{name: "no protocol", give: []service.Metadata{{Name: "x"}}, want: "no protocol"},
		{name: "duplicate name", give: []service.Metadata{sqs, sqs}, want: "registered twice"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := route.NewTable(registered(tt.give...))
			require.ErrorContains(t, err, tt.want)
		})
	}

	_, err := route.NewTable(nil)
	require.NoError(t, err)
}

func Test_Operation(t *testing.T) {
	tests := []struct {
		name        string
		giveKind    protocol.Kind
		giveHeaders map[string]string
		giveForm    func() url.Values
		want        string
	}{
		{
			name:        "json",
			giveKind:    protocol.KindJSON,
			giveHeaders: map[string]string{headerTarget: targetSendMessage},
			want:        opSendMessage,
		},
		{
			name:        "json target without prefix",
			giveKind:    protocol.KindJSON,
			giveHeaders: map[string]string{headerTarget: opSendMessage},
			want:        opSendMessage,
		},
		{name: "json without target", giveKind: protocol.KindJSON, want: ""},
		{
			name:     "query",
			giveKind: protocol.KindQuery,
			giveForm: formOf("Action=GetCallerIdentity&Version=1"),
			want:     "GetCallerIdentity",
		},
		{name: "query without Action", giveKind: protocol.KindQuery, giveForm: formOf("Version=1"), want: ""},
		{name: "query without form", giveKind: protocol.KindQuery, want: ""},
		{
			name:     "ec2",
			giveKind: protocol.KindEC2,
			giveForm: formOf("Action=DescribeInstances"),
			want:     "DescribeInstances",
		},
		{name: "rest", giveKind: protocol.KindRestJSON, giveForm: formOf("Action=X"), want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRequest(t, "localhost", tt.giveHeaders)

			assert.Equal(t, tt.want, route.Operation(r, tt.giveKind, tt.giveForm))
		})
	}
}
