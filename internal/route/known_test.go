package route_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/route"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_Table_Unregistered covers a request for a service LocalStack knows and
// gostack does not serve: it is told apart by the indicators LocalStack uses
// (signing name, X-Amz-Target prefix, host), and the answer names the protocol
// of the service's spec.
func Test_Table_Unregistered(t *testing.T) {
	const (
		formType = "application/x-www-form-urlencoded"
		jsonType = "application/x-amz-json-1.0"
	)

	tests := []struct {
		name         string
		giveServices []service.Metadata
		giveHost     string
		giveHeaders  map[string]string
		giveForm     string
		wantService  string
		wantProtocol protocol.Kind
		wantFound    bool
	}{
		{
			name:         "dynamodb by signing name and target",
			giveServices: []service.Metadata{sqs, sts},
			giveHost:     localHost,
			giveHeaders: map[string]string{
				headerAuth: scope(nameDynamoDB), headerTarget: targetListTables, headerType: jsonType,
			},
			wantService:  nameDynamoDB,
			wantProtocol: protocol.KindJSON,
			wantFound:    true,
		},
		{
			name:         "dynamodb streams shares the signing name and is told apart by the target",
			giveServices: []service.Metadata{sqs, sts},
			giveHost:     localHost,
			giveHeaders: map[string]string{
				headerAuth: scope(nameDynamoDB), headerTarget: "DynamoDBStreams_20120810.ListStreams", headerType: jsonType,
			},
			wantService:  "dynamodbstreams",
			wantProtocol: protocol.KindJSON,
			wantFound:    true,
		},
		{
			name:         "a target alone names the service",
			giveServices: []service.Metadata{sqs, sts},
			giveHost:     localHost,
			giveHeaders:  map[string]string{headerTarget: targetListTables, headerType: jsonType},
			wantService:  nameDynamoDB,
			wantProtocol: protocol.KindJSON,
			wantFound:    true,
		},
		{
			name:         "iam is a query service",
			giveServices: []service.Metadata{sqs, sts},
			giveHost:     localHost,
			giveHeaders:  map[string]string{headerAuth: scope("iam"), headerType: formType},
			wantService:  "iam",
			wantProtocol: protocol.KindQuery,
			wantFound:    true,
		},
		{
			name:         "the signing name is not always the service name",
			giveServices: []service.Metadata{sqs, sts},
			giveHost:     localHost,
			giveHeaders:  map[string]string{headerAuth: scope("monitoring"), headerType: formType},
			wantService:  "cloudwatch",
			wantProtocol: protocol.KindQuery,
			wantFound:    true,
		},
		{
			name:         "lambda is rest-json",
			giveServices: []service.Metadata{sqs, sts},
			giveHost:     localHost,
			giveHeaders:  map[string]string{headerAuth: scope("lambda")},
			wantService:  "lambda",
			wantProtocol: protocol.KindRestJSON,
			wantFound:    true,
		},
		{
			name:         "route53 is rest-xml",
			giveServices: []service.Metadata{sqs, sts},
			giveHost:     localHost,
			giveHeaders:  map[string]string{headerAuth: scope("route53")},
			wantService:  "route53",
			wantProtocol: protocol.KindRestXML,
			wantFound:    true,
		},
		{
			name:         "the host names the service",
			giveServices: []service.Metadata{sqs, sts},
			giveHost:     "dynamodb.localhost.localstack.cloud:4566",
			giveHeaders:  map[string]string{headerType: jsonType},
			wantService:  nameDynamoDB,
			wantProtocol: protocol.KindJSON,
			wantFound:    true,
		},
		{
			name:         "a service the gateway serves is not unregistered",
			giveServices: []service.Metadata{sqs, sts},
			giveHost:     localHost,
			giveHeaders:  map[string]string{headerAuth: scope(nameSQS), headerType: formType},
		},
		{
			name: "a registered dynamodb is not unregistered",
			giveServices: []service.Metadata{{
				Name: nameDynamoDB, SigningName: nameDynamoDB, TargetPrefix: "DynamoDB_20120810",
				Protocols: []protocol.Kind{protocol.KindJSON},
			}},
			giveHost:    localHost,
			giveHeaders: map[string]string{headerAuth: scope(nameDynamoDB), headerTarget: targetListTables},
		},
		{
			name:         "a signing name two services share and nothing to tell them apart",
			giveServices: []service.Metadata{sqs, sts},
			giveHost:     localHost,
			giveHeaders:  map[string]string{headerAuth: scope("s3")},
		},
		{
			name:         "an unknown signing name",
			giveServices: []service.Metadata{sqs, sts},
			giveHost:     localHost,
			giveHeaders:  map[string]string{headerAuth: scope("bogus")},
		},
		{
			name:         "a bare Action is not enough",
			giveServices: []service.Metadata{sqs, sts},
			giveHost:     localHost,
			giveHeaders:  map[string]string{headerType: formType},
			giveForm:     "Action=ListUsers&Version=2010-05-08",
		},
		{
			name:         "no indicator at all",
			giveServices: []service.Metadata{sqs, sts},
			giveHost:     localHost,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			table, err := route.NewTable(tt.giveServices)
			require.NoError(t, err)

			got, found := table.Unregistered(newRequest(t, tt.giveHost, tt.giveHeaders))

			require.Equal(t, tt.wantFound, found)
			assert.Equal(t, tt.wantService, got.Service)
			assert.Equal(t, tt.wantProtocol, got.Protocol)
		})
	}
}

// Test_knownServices_matchTheSpecs keeps the table of services gostack does not
// serve in step with the vendored botocore specs: every spec is in it, with its
// signing name, target prefix, endpoint prefix, API version and protocols.
func Test_knownServices_matchTheSpecs(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "specs", "botocore", "*", "*", "service-2.json"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "the vendored specs are missing")

	known := make(map[string]service.Metadata)
	for _, meta := range route.KnownServices() {
		known[meta.Name] = meta
	}

	assert.Len(t, known, len(route.KnownServices()), "a service is listed twice")
	assert.Len(t, known, len(files), "a spec has no entry, or an entry has no spec")

	for _, file := range files {
		name := filepath.Base(filepath.Dir(filepath.Dir(file)))

		data, err := os.ReadFile(file)
		require.NoError(t, err)

		var spec specFile

		require.NoError(t, json.Unmarshal(data, &spec), name)

		signing := spec.Metadata.SigningName
		if signing == "" {
			signing = spec.Metadata.EndpointPrefix
		}

		got, ok := known[name]
		require.True(t, ok, "%s is not in the table", name)

		assert.Equal(t, signing, got.SigningName, name)
		assert.Equal(t, spec.Metadata.TargetPrefix, got.TargetPrefix, name)
		assert.Equal(t, spec.Metadata.EndpointPrefix, got.EndpointPrefix, name)
		assert.Equal(t, spec.Metadata.APIVersion, got.APIVersion, name)
		assert.Equal(t, specKinds(spec.Metadata.Protocol, spec.Metadata.Protocols), got.Protocols, name)
	}
}

// specFile is the part of a botocore service-2.json that the router's table
// is built from.
type specFile struct {
	Metadata specMetadata `json:"metadata"`
}

type specMetadata struct {
	APIVersion     string   `json:"apiVersion"`
	EndpointPrefix string   `json:"endpointPrefix"`
	SigningName    string   `json:"signingName"`
	TargetPrefix   string   `json:"targetPrefix"`
	Protocol       string   `json:"protocol"`
	Protocols      []string `json:"protocols"`
}

// specKinds maps the protocols a spec declares to the ones the router knows,
// in the spec's order. A protocol the router has no kind for, such as
// smithy-rpc-v2-cbor, is left out.
func specKinds(single string, declared []string) []protocol.Kind {
	if len(declared) == 0 {
		declared = []string{single}
	}

	kinds := map[string]protocol.Kind{
		"query":     protocol.KindQuery,
		"json":      protocol.KindJSON,
		"rest-json": protocol.KindRestJSON,
		"rest-xml":  protocol.KindRestXML,
		"ec2":       protocol.KindEC2,
	}

	var out []protocol.Kind

	for _, name := range declared {
		if kind, ok := kinds[name]; ok && !slices.Contains(out, kind) {
			out = append(out, kind)
		}
	}

	return out
}
