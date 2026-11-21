package sns_test

import (
	"bytes"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var requestIDElement = regexp.MustCompile(`<RequestId>[^<]*</RequestId>`)

// apiRead is one read of SNS state: a query action in a region, or a GET of a
// developer route when dev is set.
type apiRead struct {
	label  string
	region string
	action string
	params []string
	dev    string
}

// call is a read by the query action with params, labelled by both.
func call(action string, params ...string) apiRead {
	return apiRead{label: action + " " + strings.Join(params, " "), action: action, params: params}
}

// answers makes each read and keeps its status and body, without the request
// id, by label.
func (s *stack) answers(reads []apiRead) map[string]string {
	out := make(map[string]string, len(reads))

	for _, r := range reads {
		if r.dev != "" {
			status, body := s.devCall(http.MethodGet, r.dev, nil, "")
			out[r.label] = http.StatusText(status) + " " + body

			continue
		}

		in := r.region
		if in == "" {
			in = region
		}

		rep := s.callIn(in, r.action, r.params...)
		out[r.label] = http.StatusText(rep.status) + " " + requestIDElement.ReplaceAllString(rep.body, "")
	}

	return out
}

// restartedFrom is a new stack whose SNS restored snapshot.
func restartedFrom(t *testing.T, snapshot []byte) *stack {
	t.Helper()

	r := newStack(t, nil)
	require.NoError(t, r.sns.Restore(bytes.NewReader(snapshot)))

	return r
}

// TestRestoreAnswersAsBefore builds every kind of SNS state, a subscription's
// DeliveryPolicy and SubscriptionRoleArn and a topic's data protection policy
// set to the empty string among them, snapshots it and restores it into a new
// stack: every read answers as it did before, and again after a second
// snapshot and restore.
func TestRestoreAnswersAsBefore(t *testing.T) {
	s := newStack(t, nil)

	var reads []apiRead

	topicReads := func(arn string) {
		reads = append(reads,
			call("GetTopicAttributes", paramTopicArn, arn),
			call("ListTagsForResource", "ResourceArn", arn),
			call("GetDataProtectionPolicy", "ResourceArn", arn),
			call("ListSubscriptionsByTopic", paramTopicArn, arn),
		)
	}
	subReads := func(arn string) {
		reads = append(reads,
			call("GetSubscriptionAttributes", paramSubArn, arn),
			apiRead{label: "token " + arn, dev: pathGostack + tokensURL + arn},
		)
	}

	q := s.createQueue(account, region, "restored", nil)
	dlq := s.createQueue(account, region, "restored-dlq", nil)

	t1 := s.createTopic(paramName, "t1", tagKey1, "zeta", tagValue1, "1",
		"Tags.member.2.Key", "alpha", "Tags.member.2.Value", "",
		attrKey1, "DisplayName", attrValue1, "Display name")
	topicReads(t1)
	s.ok(nil, "SetTopicAttributes", paramTopicArn, t1, "AttributeName", "DeliveryPolicy",
		"AttributeValue", `{"http":{"defaultHealthyRetryPolicy":{"numRetries":5}}}`)
	s.ok(nil, "SetTopicAttributes", paramTopicArn, t1, "AttributeName", "KmsMasterKeyId", "AttributeValue", "alias/x")
	s.ok(nil, "SetTopicAttributes", paramTopicArn, t1, "AttributeName", "SignatureVersion", "AttributeValue", "2")
	s.ok(nil, "SetTopicAttributes", paramTopicArn, t1, "AttributeName", "TracingConfig", "AttributeValue", "Active")
	s.ok(nil, "AddPermission", paramTopicArn, t1, "Label", "lbl", "AWSAccountId.member.1", "111111111111",
		"ActionName.member.1", "Publish")
	s.ok(nil, "TagResource", "ResourceArn", t1, "Tags.member.1.Key", "zeta", "Tags.member.1.Value", "2",
		"Tags.member.2.Key", "mid", "Tags.member.2.Value", "m")

	// A data protection policy set to the empty string, and one set to a
	// policy.
	emptyPolicy := s.createTopic(paramName, "empty-policy")
	topicReads(emptyPolicy)
	s.ok(nil, "PutDataProtectionPolicy", "ResourceArn", emptyPolicy, "DataProtectionPolicy", "")

	withPolicy := s.createTopic(paramName, "with-policy",
		"DataProtectionPolicy", `{"Name":"p","Version":"2021-06-01","Statement":[]}`)
	topicReads(withPolicy)

	// A topic deleted, then made again.
	gone := s.createTopic(paramName, "gone")
	s.subscribeQueue(gone, q.arn)
	s.ok(nil, "DeleteTopic", paramTopicArn, gone)
	gone = s.createTopic(paramName, "gone")
	topicReads(gone)

	fifo := s.createTopic(paramName, "f.fifo", attrKey1, "FifoTopic", attrValue1, textTrue,
		"Attributes.entry.2.key", "ContentBasedDeduplication", "Attributes.entry.2.value", textTrue)
	topicReads(fifo)

	rep := s.callIn(otherRegion, "CreateTopic", paramName, "west")
	require.Equal(t, http.StatusOK, rep.status, rep.body)

	reads = append(reads, apiRead{label: "topics in another region", region: otherRegion, action: "ListTopics"})

	withAttributes := s.subscribeQueue(t1, q.arn, attrPolicy, `{"a":{"b":["x",{"prefix":"p"}]}}`,
		attrScope, "MessageBody", attrRaw, textTrue, attrRedrive, `{"deadLetterTargetArn":"`+dlq.arn+`"}`)
	subReads(withAttributes)

	// The two subscription attributes a caller can set to the empty string.
	empties := s.subscribe(paramTopicArn, emptyPolicy, paramProtocol, "sqs", paramEndpoint, q.arn, paramReturn, textTrue)
	s.ok(nil, opSetAttrs, paramSubArn, empties, "AttributeName", "DeliveryPolicy", "AttributeValue", "")
	s.ok(nil, opSetAttrs, paramSubArn, empties, "AttributeName", "SubscriptionRoleArn", "AttributeValue", "")
	subReads(empties)

	set := s.subscribe(paramTopicArn, withPolicy, paramProtocol, "sqs", paramEndpoint, q.arn, paramReturn, textTrue)
	s.ok(nil, opSetAttrs, paramSubArn, set, "AttributeName", "DeliveryPolicy",
		"AttributeValue", `{"healthyRetryPolicy":{"numRetries":1}}`)
	s.ok(nil, opSetAttrs, paramSubArn, set, "AttributeName", "SubscriptionRoleArn",
		"AttributeValue", "arn:aws:iam::000000000000:role/r")
	s.ok(nil, opSetAttrs, paramSubArn, set, "AttributeName", "FilterPolicy", "AttributeValue", `{}`)
	subReads(set)

	pending := s.subscribe(paramTopicArn, t1, paramProtocol, "http", paramEndpoint, "http://127.0.0.1:1/never",
		paramReturn, textTrue)
	subReads(pending)
	subReads(s.subscribe(paramTopicArn, t1, paramProtocol, "email", paramEndpoint, "a@example.com", paramReturn, textTrue))

	confirmed := s.subscribe(paramTopicArn, t1, paramProtocol, "https", paramEndpoint, "https://127.0.0.1:1/c",
		paramReturn, textTrue)
	s.ok(nil, opConfirm, paramTopicArn, t1, "Token", s.token(confirmed))
	subReads(confirmed)
	subReads(s.subscribe(paramTopicArn, t1, paramProtocol, "sms", paramEndpoint, "+15551230000", paramReturn, textTrue))
	subReads(s.subscribe(paramTopicArn, t1, paramProtocol, "lambda",
		paramEndpoint, "arn:aws:lambda:us-east-1:000000000000:function:f", paramReturn, textTrue))

	unsubscribed := s.subscribe(paramTopicArn, t1, paramProtocol, "email", paramEndpoint, "gone@example.com",
		paramReturn, textTrue)
	s.ok(nil, opUnsub, paramSubArn, unsubscribed)
	subReads(unsubscribed)

	deletedApp := s.createApp("deleted")
	app := s.createApp("live")
	s.ok(nil, "DeletePlatformApplication", paramAppARN, deletedApp)

	e1 := s.createEndpoint(app, "tok-1", "CustomUserData", "user data")
	e2 := s.createEndpoint(app, "tok-2")
	e3 := s.createEndpoint(app, "tok-3")
	s.ok(nil, "DeleteEndpoint", paramEndpointAR, e2)
	s.ok(nil, opSetEndpointAttrs, join([]string{paramEndpointAR, e3}, disabled)...)
	subReads(s.subscribe(paramTopicArn, t1, paramProtocol, "application", paramEndpoint, e1, paramReturn, textTrue))

	for _, e := range []string{e1, e2, e3} {
		reads = append(reads, call("GetEndpointAttributes", paramEndpointAR, e))
	}

	reads = append(reads,
		call("ListPlatformApplications"),
		call("GetPlatformApplicationAttributes", paramAppARN, app),
		call("GetPlatformApplicationAttributes", paramAppARN, deletedApp),
		call("ListEndpointsByPlatformApplication", paramAppARN, app),
		call("ListTopics"),
		call("ListSubscriptions"),
		call("GetSMSAttributes"),
		call("ListPhoneNumbersOptedOut"),
		apiRead{label: "endpoint messages", dev: pathGostack + endpointMessagesURL},
		apiRead{label: "sms messages", dev: pathGostack + smsMessagesURL},
		apiRead{label: "endpoint messages, aws path", dev: pathAWS + endpointMessagesURL},
	)

	s.ok(nil, "SetSMSAttributes", pairs("attributes", senderID, "Sender", attrSMSType, "Transactional")...)

	for _, optOut := range []string{`{"phoneNumber":"+15550000001"}`, `{"phoneNumber":"+15550000002"}`} {
		status, body := s.devCall(http.MethodPost, pathGostack+optOutsURL, nil, optOut)
		require.Equal(t, http.StatusOK, status, body)
	}

	s.ok(nil, "OptInPhoneNumber", "phoneNumber", "+15550000002")

	s.publish(join([]string{
		paramTargetARN, e1, paramMessage, `{"default":"d","ADM":"x"}`, "MessageStructure", "json",
		paramSubject, "a subject",
	},
		attr(1, "key", "v"),
		[]string{
			"MessageAttributes.entry.2.Name", "bin", "MessageAttributes.entry.2.Value.DataType", "Binary",
			"MessageAttributes.entry.2.Value.BinaryValue", "AAEC/w==",
		},
		[]string{
			"MessageAttributes.entry.3.Name", "space", "MessageAttributes.entry.3.Value.DataType", "String.custom",
			"MessageAttributes.entry.3.Value.StringValue", " ",
		})...)
	s.publish(paramPhone, number, paramMessage, "an sms")
	s.publish(paramTopicArn, t1, paramMessage, "to devices and sms")
	s.publish(paramTopicArn, fifo, paramMessage, "once", paramGroup, "g")
	s.publish(paramTopicArn, fifo, paramMessage, "two", paramGroup, "g", paramDedup, "d2")
	s.sns.AwaitDeliveries(t.Context())

	before := s.answers(reads)

	var first bytes.Buffer
	require.NoError(t, s.sns.Snapshot(&first))

	r := restartedFrom(t, first.Bytes())

	var second bytes.Buffer
	require.NoError(t, r.sns.Snapshot(&second))

	again := restartedFrom(t, second.Bytes())

	for _, after := range []map[string]string{r.answers(reads), again.answers(reads)} {
		for _, label := range slices.Sorted(maps.Keys(before)) {
			assert.Equal(t, before[label], after[label], label)
		}
	}

	assert.Equal(t, s.publish(paramTopicArn, fifo, paramMessage, "once", paramGroup, "g"),
		r.publish(paramTopicArn, fifo, paramMessage, "once", paramGroup, "g"),
		"a repeat by content answers alike before and after the restore")
}
