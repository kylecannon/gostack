package sqs_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	hugeBatchEntries = 65_536
	queueURLPath     ="http://localhost.localstack.cloud:4566/000000000000/huge-batch"
)

// Test_SendMessageBatch_of_65536_entries_is_refused sends the largest
// batch a 16 MiB form body can carry over the query protocol, through the real
// gateway. The decoder reads every entry before the service can count them, and
// it used to look for the keys below each entry in the whole form, so this one
// request kept a core busy for minutes. It is now answered at once.
func Test_SendMessageBatch_of_65536_entries_is_refused(t *testing.T) {
	provider := sqs.New(func() sqs.Settings {
		return sqs.Settings{EndpointStrategy: sqs.StrategyStandard, Host: "localhost.localstack.cloud:4566"}
	}, nil)

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{"0.0.0.0:4566"}} },
		[]service.Registration{provider.Registration()},
		nil,
		nil,
	)
	require.NoError(t, err)

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20130524/us-east-1/sqs/aws4_request, "+
			"SignedHeaders=host;x-amz-date, Signature=abc")

		rec := httptest.NewRecorder()
		gw.Handler().ServeHTTP(rec, req)

		return rec
	}

	created := post(url.Values{"Action": {"CreateQueue"}, "Version": {"2012-11-05"}, "QueueName": {"huge-batch"}}.Encode())
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())

	form := url.Values{"Action": {"SendMessageBatch"}, "Version": {"2012-11-05"}, "QueueUrl": {queueURLPath}}

	for i := 1; i <= hugeBatchEntries; i++ {
		prefix := "SendMessageBatchRequestEntry." + strconv.Itoa(i) + "."
		form.Set(prefix+"Id", strconv.Itoa(i))
		form.Set(prefix+"MessageBody", "x")
	}

	// The refusal is asserted by its answer, not by how long it took: a decoder
	// quadratic in the entries took about 30 s here, which wall time cannot catch
	// without failing a loaded host. Test_Decoder_BatchScalesWithEntries in the
	// query package measures that growth on CPU time.
	rec := post(form.Encode())

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "<Code>AWS.SimpleQueueService.TooManyEntriesInBatchRequest</Code>")
	assert.Contains(t, rec.Body.String(), "You have sent "+strconv.Itoa(hugeBatchEntries)+".")
}
