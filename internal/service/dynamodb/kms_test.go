package dynamodb_test

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/kylecannon/gostack/internal/service/kms"
	"github.com/stretchr/testify/require"
)

const (
	keySSE         = "SSEDescription"
	keyKMSMaster   = "KMSMasterKeyArn"
	sseOn          = `"SSESpecification":{"Enabled":true}`
	sseOff         = `"SSESpecification":{"Enabled":false}`
	managedKeyText = "Default key that protects my DynamoDB data when no other key is defined"
	arnPattern     = `^arn:aws:kms:us-east-1:000000000000:key/[0-9a-f-]{36}$`
)

// kmsStack is DynamoDB bound to a gateway that serves a real KMS.
type kmsStack struct {
	*stack

	kms service.Registration
}

// newKMSStack is DynamoDB with KMS behind the same gateway, as gostack's own
// gateway binds it.
func newKMSStack(tb testing.TB) *kmsStack {
	tb.Helper()

	reg := kms.New(func(string) {}).Registration()

	return &kmsStack{stack: bindStack(tb, reg), kms: reg}
}

// bindStack is DynamoDB beside others behind one gateway, with DynamoDB's calls
// to other services bound to it.
func bindStack(tb testing.TB, others ...service.Registration) *stack {
	tb.Helper()

	s := &stack{}
	s.settings.Store(&dynamodb.Settings{Host: testHost})
	s.provider = dynamodb.New(func() dynamodb.Settings { return *s.settings.Load() }, func(string) {})
	tb.Cleanup(func() { s.provider.Registration().Close(context.Background()) })

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{"0.0.0.0:4566"}} },
		append([]service.Registration{s.provider.Registration()}, others...),
		nil,
		nil,
	)
	require.NoError(tb, err)

	s.handler = gw.Handler()
	s.provider.Bind(gw.Handler())

	return s
}

// kmsCall sends a KMS operation as the test account in region, through the
// gateway as an outside client would.
func (s *kmsStack) kmsCall(tb testing.TB, region, operation, body string, header http.Header) map[string]any {
	tb.Helper()

	req := httptest.NewRequestWithContext(tb.Context(), http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "TrentService."+operation)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20130524/"+region+
		"/kms/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc")
	maps.Copy(req.Header, header)

	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	require.Equal(tb, http.StatusOK, rec.Code, rec.Body.String())

	var out map[string]any
	require.NoError(tb, json.Unmarshal(rec.Body.Bytes(), &out))

	return out
}

// keyMetadata is the KeyMetadata of a KMS answer.
func keyMetadata(tb testing.TB, answer map[string]any) map[string]any {
	tb.Helper()

	meta, ok := answer["KeyMetadata"].(map[string]any)
	require.True(tb, ok, "no KeyMetadata in %v", answer)

	return meta
}

// sseOf is the SSEDescription of the TableDescription of a CreateTable or
// UpdateTable answer, as json; "null" when it has none.
func sseOf(tb testing.TB, answer map[string]any) string {
	tb.Helper()

	described, ok := path(tb, answer, keyDescription).(map[string]any)
	require.True(tb, ok)

	out, err := json.Marshal(described[keySSE])
	require.NoError(tb, err)

	return string(out)
}

// keyOf is the key ARN of the SSEDescription of an answer.
func keyOf(tb testing.TB, answer map[string]any) string {
	tb.Helper()

	var sse map[string]string
	require.NoError(tb, json.Unmarshal([]byte(sseOf(tb, answer)), &sse))

	return sse[keyKMSMaster]
}

// wantSSE is the SSEDescription of a table with status, as json.
func wantSSE(status, arn string) string {
	return `{"Status":"` + status + `","SSEType":"KMS","` + keyKMSMaster + `":"` + arn + `"}`
}

func makeTable(name string, members ...string) string {
	return body(append([]string{named(name), idKey, idDefs, throughput}, members...)...)
}

// keysIn is how many keys KMS holds in region.
func (s *kmsStack) keysIn(tb testing.TB, region string) int {
	tb.Helper()

	keys, ok := s.kmsCall(tb, region, "ListKeys", `{}`, nil)["Keys"].([]any)
	require.True(tb, ok)

	return len(keys)
}

// Test_managedKey is test_dynamodb_create_table_with_partial_sse_specification
// and test_dynamodb_update_table_without_sse_specification_change: a table with
// SSE enabled and no key reports the ARN of a key KMS holds, an AWS managed one
// with LocalStack's description.
func Test_managedKey(t *testing.T) {
	t.Run("a table with SSE and no key gets the AWS managed key KMS made for it", func(t *testing.T) {
		s := newKMSStack(t)

		arn := keyOf(t, s.ok(t, opCreate, makeTable(tab1, sseOn)))
		require.Regexp(t, arnPattern, arn)

		meta := keyMetadata(t, s.kmsCall(t, usEast1, "DescribeKey", `{"KeyId":"`+arn+`"}`, nil))
		require.Equal(t, "AWS", meta["KeyManager"])
		require.Equal(t, managedKeyText, meta["Description"])
		require.Equal(t, "Enabled", meta["KeyState"])
	})

	t.Run("every table of an account and region shares one key, and another region has its own", func(t *testing.T) {
		s := newKMSStack(t)

		first := keyOf(t, s.ok(t, opCreate, makeTable(tab1, sseOn)))
		require.Equal(t, first, keyOf(t, s.ok(t, opCreate, makeTable("tab2", sseOn))))

		other := s.call(t, "eu-west-1", opCreate, makeTable("tab3", sseOn))
		require.Equal(t, http.StatusOK, other.status, other.body)

		var made map[string]any
		require.NoError(t, json.Unmarshal([]byte(other.body), &made))
		require.NotEqual(t, first, keyOf(t, made))
		require.Contains(t, keyOf(t, made), ":kms:eu-west-1:")
		require.Equal(t, 1, s.keysIn(t, usEast1))
	})

	t.Run("a named key makes no key and is reported as an ARN", func(t *testing.T) {
		s := newKMSStack(t)

		made := s.ok(t, opCreate, makeTable(tab1, `"SSESpecification":{"Enabled":true,"KMSMasterKeyId":"k1"}`))
		require.JSONEq(t, wantSSE("ENABLED", "arn:aws:kms:us-east-1:000000000000:key/k1"), sseOf(t, made))
		require.Equal(t, 0, s.keysIn(t, usEast1))
	})

	t.Run("an update that leaves SSE alone keeps it, and one that turns it off answers it UPDATING", func(t *testing.T) {
		s := newKMSStack(t)

		arn := keyOf(t, s.ok(t, opCreate, makeTable(tab1, sseOn)))

		kept := s.ok(t, opUpdate, `{"TableName":"tab1","BillingMode":"PAY_PER_REQUEST"}`)
		require.JSONEq(t, wantSSE("ENABLED", arn), sseOf(t, kept))

		off := s.ok(t, opUpdate, `{"TableName":"tab1",`+sseOff+`}`)
		require.JSONEq(t, wantSSE("UPDATING", arn), sseOf(t, off))

		described, ok := path(t, s.ok(t, opDescribe, describeTab1), keyTable).(map[string]any)
		require.True(t, ok)
		require.NotContains(t, described, keySSE)

		again := s.ok(t, opUpdate, `{"TableName":"tab1",`+sseOff+`}`)
		require.Equal(t, "null", sseOf(t, again))
	})

	t.Run("an update that turns SSE on gives the table the managed key", func(t *testing.T) {
		s := newKMSStack(t)

		s.ok(t, opCreate, hashTable(tab1))

		on := s.ok(t, opUpdate, `{"TableName":"tab1",`+sseOn+`}`)
		require.Regexp(t, arnPattern, keyOf(t, on))
		require.JSONEq(t, wantSSE("ENABLED", keyOf(t, on)), sseOf(t, on))
	})

	t.Run("a reset makes the next table a new key", func(t *testing.T) {
		s := newKMSStack(t)

		first := keyOf(t, s.ok(t, opCreate, makeTable(tab1, sseOn)))

		s.provider.Registration().Reset("", "")
		s.kms.Reset("", "")

		require.NotEqual(t, first, keyOf(t, s.ok(t, opCreate, makeTable(tab1, sseOn))))
	})

	t.Run("a key KMS no longer has, or has not enabled, is replaced", func(t *testing.T) {
		s := newKMSStack(t)

		first := keyOf(t, s.ok(t, opCreate, makeTable(tab1, sseOn)))
		require.Equal(t, first, keyOf(t, s.ok(t, opCreate, makeTable("tab2", sseOn))), "a usable key is kept")

		// A reset of KMS alone.
		s.kms.Reset("", "")

		second := keyOf(t, s.ok(t, opCreate, makeTable("tab3", sseOn)))
		require.NotEqual(t, first, second)
		require.Equal(t, 1, s.keysIn(t, usEast1))

		// A scheduled deletion leaves the key PendingDeletion, which is not enabled.
		s.kmsCall(t, usEast1, "ScheduleKeyDeletion", `{"KeyId":"`+second+`","PendingWindowInDays":7}`, nil)

		third := keyOf(t, s.ok(t, opCreate, makeTable("tab4", sseOn)))
		require.NotEqual(t, second, third)
		require.Regexp(t, arnPattern, third)
	})

	t.Run("under DYNAMODB_SHARE_DB a reset of KMS alone, or a scoped one, still makes a new key", func(t *testing.T) {
		s := newKMSStack(t)
		s.settings.Store(&dynamodb.Settings{Host: testHost, ShareDB: true})

		first := keyOf(t, s.ok(t, opCreate, makeTable(tab1, sseOn)))

		s.kms.Reset(accountID, usEast1)
		// The shared state lives at 000000000000/ddblocal, which a reset of an
		// account and region does not reach.
		s.provider.Registration().Reset(accountID, usEast1)

		require.NotEqual(t, first, keyOf(t, s.ok(t, opCreate, makeTable("tab2", sseOn))))
	})

	t.Run("an update that is refused makes no key", func(t *testing.T) {
		s := newKMSStack(t)

		s.ok(t, opCreate, hashTable(tab1))

		// Pay per request with a throughput is refused by the billing check.
		refused := s.call(t, usEast1, opUpdate, `{"TableName":"tab1","BillingMode":"PAY_PER_REQUEST",`+
			`"ProvisionedThroughput":{"ReadCapacityUnits":1,"WriteCapacityUnits":1},`+sseOn+`}`)
		require.Equal(t, http.StatusBadRequest, refused.status, refused.body)
		require.Equal(t, 0, s.keysIn(t, usEast1))

		// A stream that is not there to disable is refused by the stream check.
		refused = s.call(t, usEast1, opUpdate, `{"TableName":"tab1","StreamSpecification":{"StreamEnabled":false},`+
			sseOn+`}`)
		require.Equal(t, http.StatusBadRequest, refused.status, refused.body)
		require.Equal(t, 0, s.keysIn(t, usEast1))

		on := s.ok(t, opUpdate, `{"TableName":"tab1",`+sseOn+`}`)
		require.Regexp(t, arnPattern, keyOf(t, on))
		require.Equal(t, 1, s.keysIn(t, usEast1))
	})

	t.Run("a table updated in a replica region gets a key of its owning region", func(t *testing.T) {
		const owner, replica = "ap-south-1", "eu-west-1"

		s := newKMSStack(t)

		created := s.call(t, owner, opCreate, hashTable(tab1))
		require.Equal(t, http.StatusOK, created.status, created.body)

		made := s.call(t, owner, opUpdate,
			`{"TableName":"tab1","ReplicaUpdates":[{"Create":{"RegionName":"`+replica+`","KMSMasterKeyId":"k"}}]}`)
		require.Equal(t, http.StatusOK, made.status, made.body)

		update := s.call(t, replica, opUpdate, `{"TableName":"tab1",`+sseOn+`}`)
		require.Equal(t, http.StatusOK, update.status, update.body)

		var answer map[string]any
		require.NoError(t, json.Unmarshal([]byte(update.body), &answer))
		require.Contains(t, keyOf(t, answer), ":kms:"+owner+":")
		require.Equal(t, 1, s.keysIn(t, owner))
		require.Equal(t, 0, s.keysIn(t, replica))
	})

	t.Run("a gateway that serves no KMS leaves the key out", func(t *testing.T) {
		for name, s := range map[string]*stack{"unbound": newStack(t), "no KMS registered": bindStack(t)} {
			require.JSONEq(t, `{"Status":"ENABLED","SSEType":"KMS"}`,
				sseOf(t, s.ok(t, opCreate, makeTable(tab1, sseOn))), name)
		}
	})

	t.Run("a client that sends the key manager header makes a customer key", func(t *testing.T) {
		s := newKMSStack(t)

		made := s.kmsCall(t, usEast1, "CreateKey", `{}`, http.Header{kms.KeyManagerHeader: {kms.ManagerAWS}})
		require.Equal(t, "CUSTOMER", keyMetadata(t, made)["KeyManager"])
	})
}
