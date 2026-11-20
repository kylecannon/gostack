//go:build unix

package gateway_test

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/stretchr/testify/require"
)

// Test_Gateway_BatchScalesWithEntries pins the refusal of an oversized batch to
// near-linear CPU time, from the gateway to the service. A SendMessageBatch of
// 65,536 entries is the most a 16 MiB form carries, and the service counts the
// entries only after the generated decode has built every one and checkBatch has
// been given their ids. The decoder's own growth is Test_Decoder_BatchScalesWithEntries,
// in the query package, which cannot see the rest of that path; this test serves
// the whole request, at five sizes that each double the one before, and asserts
// the two figures of the cost against the size: the exponent over all of them,
// and the slope from 4,000 to 16,000 entries.
//
// The smallest defect it is proven to catch is a nested loop over the entries'
// ids ahead of the count check in checkBatch, one string comparison per pair:
// planted so, it failed both runs it was run in, at an exponent of 1.78 and 1.80
// and a tail slope of 1.88, with 1.7 s of CPU for the 16,000-entry call against
// 0.11 s (SQ-372, darwin/arm64 under -race). A defect that costs less than that
// pair loop at 16,000 entries was not tried and may pass.
//
// A healthy path measured an exponent of 1.054 to 1.078 and a tail slope of 1.043
// to 1.095 over 20 runs of 15 rounds, and passed all of them, at load average 7
// to 16 on 10 cores, darwin/arm64 under -race. Like the decoder's, the test does
// not call t.Parallel(): its ceiling counts the CPU time of the whole process.
func Test_Gateway_BatchScalesWithEntries(t *testing.T) {
	const (
		queueURL   = "http://localhost.localstack.cloud:4566/000000000000/growth-batch"
		sqsVersion = "2012-11-05"
	)

	provider := sqs.New(func() sqs.Settings {
		return sqs.Settings{EndpointStrategy: sqs.StrategyStandard, Host: "localhost.localstack.cloud:4566"}
	}, nil)
	handler := newGateway(t, newSettings(), []service.Registration{provider.Registration()}).Handler()
	headers := map[string]string{headerAuth: signed("test", usEast1, nameSQS), headerType: formType}

	created, body := call(t, handler, newRequest(t, http.MethodPost, localhost,
		url.Values{"Action": {"CreateQueue"}, "Version": {sqsVersion}, "QueueName": {"growth-batch"}}.Encode(), headers))
	require.Equal(t, http.StatusOK, created.StatusCode, body)

	build := func(entries int) *http.Request {
		form := url.Values{"Action": {"SendMessageBatch"}, "Version": {sqsVersion}, "QueueUrl": {queueURL}}

		for i := 1; i <= entries; i++ {
			prefix := "SendMessageBatchRequestEntry." + strconv.Itoa(i) + "."
			form.Set(prefix+"Id", strconv.Itoa(i))
			form.Set(prefix+"MessageBody", "x")
		}

		return newRequest(t, http.MethodPost, localhost, form.Encode(), headers)
	}

	sizes := []int{1_000, 2_000, 4_000, 8_000, 16_000}
	costs := bestCosts(t, handler, sizes, http.StatusBadRequest, build)

	requireNearLinear(t, "SendMessageBatch", sizes, costs)
}

// Test_Gateway_NestedBodiesScaleWithDepth pins what a body that only nests costs
// to near-linear CPU time in its size, which is its depth: a body of 1, 2, 4 and
// 8 MiB and one at the 16 MiB cap, nested as deeply as its syntax allows, in
// json objects (refused at 10,000 levels), rest-xml elements (refused at
// restxml.MaxDepth) and query keys (a key nested past the shape, ignored). The
// figures are those of Test_Gateway_BatchScalesWithEntries. Test_Gateway_DeeplyNestedBodies
// bounds what the same bodies allocate and that none of them waits; neither sees
// a decoder that re-reads the nesting it has passed, which is this test.
//
// The smallest defect it is proven to catch is a busy loop of 31 s on the first
// body over 8 MiB, which the ceiling ends after 6 s of CPU (SQ-372, darwin/arm64
// under -race): it failed the one run it was run in, in all three shapes, since
// the abandoned loop goes on charging the process. A cost that grows by less
// than the ceiling at the cap was not tried and may pass. A wait is not CPU: a
// body that sleeps is seen by the fake clock of Test_Gateway_DeeplyNestedBodies.
//
// A healthy gateway measured, over 20 runs of 15 rounds that all passed, at load
// average 7 to 16 on 10 cores, darwin/arm64 under -race: json objects, an
// exponent of 0.56 to 0.57 and a tail slope of 0.71 to 0.74 (the body is refused
// at 10,000 levels however long it is); rest-xml, 0.02 to 0.07 and 0.02 to 0.15
// (refused at restxml.MaxDepth, in under a millisecond at every size); query,
// 1.00 to 1.02 and 1.01 to 1.03.
func Test_Gateway_NestedBodiesScaleWithDepth(t *testing.T) {
	const wrap = 200

	bodyCap := int(protocol.DefaultMaxBody)
	sizes := []int{1 << 20, 2 << 20, 4 << 20, 8 << 20, bodyCap - wrap}
	handler := nestingHandler(t)

	sqsJSON := jsonCall(nameSQS, targetSendMessage, "1.0")
	s3Call := map[string]string{headerAuth: signed("test", usEast1, nameS3)}
	form := map[string]string{headerAuth: signed("test", usEast1, nameSQS), headerType: formType}

	const queryPrefix = "Action=SendMessage&QueueUrl=q&MessageBody=b&MessageAttribute.1.Value"

	shapes := []struct {
		name       string
		wantStatus int
		build      func(size int) *http.Request
	}{
		{"json objects", http.StatusBadRequest, func(size int) *http.Request {
			return newRequest(t, http.MethodPost, localhost, nestTo(size, `{"a":`, "1", "}"), sqsJSON)
		}},
		{"rest-xml elements", http.StatusBadRequest, func(size int) *http.Request {
			body := "<LifecycleConfiguration><Rule>" + nestTo(size, "<a>", "", "</a>") + "</Rule></LifecycleConfiguration>"

			return newRequest(t, http.MethodPut, localhost+"bucket-a?lifecycle", body, s3Call)
		}},
		{"query keys", http.StatusNotImplemented, func(size int) *http.Request {
			body := queryPrefix + strings.Repeat(".Value", (size-len(queryPrefix))/len(".Value")) + "=1"

			return newRequest(t, http.MethodPost, localhost, body, form)
		}},
	}

	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			costs := bestCosts(t, handler, sizes, shape.wantStatus, shape.build)

			requireNearLinear(t, shape.name, sizes, costs)
		})
	}
}
