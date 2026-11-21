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
// The limits are the decoder test's, 1.5 and 1.6 (SQ-451), which give the CI
// reading behind them. Planted in hasBelow of the query decoder, darwin/arm64
// under -race at load average above 60: a full key scan on every lookup failed
// on the ceiling (6 s of CPU in one call); a scan on 1 lookup in 256 read an
// exponent of 1.236 and a tail slope of 1.348 and passed, as did 1 in 768 (1.152
// and 1.207). The smaller path cost of the decoder test's plants is not the
// gateway's: here the whole call costs 135 ms at 16,000 entries, so only a scan on
// far more lookups than 1 in 256 moves the figures past 1.6.
//
// A healthy path measured an exponent of 1.054 to 1.078 and a tail slope of 1.043
// to 1.095 over 20 runs of 15 rounds, and passed all of them, at load average 7
// to 16 on 10 cores, darwin/arm64 under -race. Under load, at average 18 to 49 on
// the same 10 cores, 20 further runs passed all of them, at an exponent of 1.054
// to 1.073 and a tail slope of 1.048 to 1.086: a false-failure rate under 14% at
// 95% confidence (SQ-373). Like the decoder's, the test does
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
// The smallest defect it is proven to catch is a busy loop that spends 1 s of CPU
// on every body over 8 MiB, a sixth of the ceiling: planted so, it failed a whole
// run of this package, in all three shapes, on the growth figures (json objects
// 1.13 s at the cap against 62 ms at 4 MiB, a tail slope of 2.10; rest-xml
// elements an exponent of 3.69; query keys failed too), and no call reached the
// ceiling (SQ-373, darwin/arm64 under -race). A busy loop of 31 s on the first
// body over 8 MiB only is not the proof: it fails this test run alone, and passes
// a run of the package, because an earlier test sends the first such body and
// spends the 31 s unasserted. A cost that grows by less than the 1 s at the cap
// was not tried and may pass. A wait is not CPU: a body that sleeps is seen by
// the fake clock of Test_Gateway_DeeplyNestedBodies.
//
// A healthy gateway measured, over 20 runs of 15 rounds that all passed, at load
// average 7 to 16 on 10 cores, darwin/arm64 under -race: json objects, an
// exponent of 0.56 to 0.57 and a tail slope of 0.71 to 0.74 (the body is refused
// at 10,000 levels however long it is); rest-xml, 0.02 to 0.07 and 0.02 to 0.15
// (refused at restxml.MaxDepth, in under a millisecond at every size); query,
// 1.00 to 1.02 and 1.01 to 1.03. Under load, at average 18 to 49 on the same 10
// cores, 20 further runs passed all of them: a false-failure rate under 14% at 95%
// confidence, and the worst figure was 0.23 under its limit (SQ-373).
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
