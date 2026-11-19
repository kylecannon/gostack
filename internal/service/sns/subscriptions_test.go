package sns_test

import (
	"encoding/base64"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	queueARN  = "arn:aws:sqs:" + region + ":" + account + ":queue"
	principal = "arn:aws:iam::" + account + ":user/DummySNSPrincipal"

	paramTopicArn = "TopicArn"
	paramProtocol = "Protocol"
	paramEndpoint = "Endpoint"
	paramSubArn   = "SubscriptionArn"
	paramReturn   = "ReturnSubscriptionArn"

	opSubscribe = "Subscribe"
	opSetAttrs  = "SetSubscriptionAttributes"
	opConfirm   = "ConfirmSubscription"
	opUnsub     = "Unsubscribe"

	invalidParameter = "InvalidParameter"

	textTrue    = "true"
	attrRaw     = "RawMessageDelivery"
	attrScope   = "FilterPolicyScope"
	attrRedrive = "RedrivePolicy"
)

func (c *client) subscribe(params ...string) string {
	c.t.Helper()

	var out struct {
		SubscriptionArn string `xml:"SubscribeResult>SubscriptionArn"`
	}

	c.ok(&out, opSubscribe, params...)

	return out.SubscriptionArn
}

func (c *client) subscribeSQS(topic string, attrs ...string) string {
	c.t.Helper()

	params := []string{paramTopicArn, topic, paramProtocol, "sqs", paramEndpoint, queueARN, paramReturn, textTrue}
	for i := 0; i+1 < len(attrs); i += 2 {
		n := strconv.Itoa(i/2 + 1)
		params = append(params, "Attributes.entry."+n+".key", attrs[i], "Attributes.entry."+n+".value", attrs[i+1])
	}

	return c.subscribe(params...)
}

func (c *client) subAttributes(arn string) map[string]string {
	c.t.Helper()

	var out struct {
		Entries []mapEntry `xml:"GetSubscriptionAttributesResult>Attributes>entry"`
	}

	c.ok(&out, "GetSubscriptionAttributes", paramSubArn, arn)

	attrs := make(map[string]string, len(out.Entries))
	for _, e := range out.Entries {
		attrs[e.Key] = e.Value
	}

	return attrs
}

type listedSub struct {
	SubscriptionArn string `xml:"SubscriptionArn"`
	Endpoint        string `xml:"Endpoint"`
	Protocol        string `xml:"Protocol"`
	Owner           string `xml:"Owner"`
	TopicArn        string `xml:"TopicArn"`
}

type subPage struct {
	Subs []listedSub `xml:"Subscriptions>member"`
	Next string      `xml:"NextToken"`
}

func (c *client) listSubs(topic, token string) subPage {
	c.t.Helper()

	var out struct {
		ByTopic subPage `xml:"ListSubscriptionsByTopicResult"`
		All     subPage `xml:"ListSubscriptionsResult"`
	}

	var params []string
	if token != "" {
		params = append(params, "NextToken", token)
	}

	if topic == "" {
		c.ok(&out, "ListSubscriptions", params...)

		return out.All
	}

	c.ok(&out, "ListSubscriptionsByTopic", append(params, paramTopicArn, topic)...)

	return out.ByTopic
}

func Test_Provider_Subscribe_sqs(t *testing.T) {
	c := newClient(t)
	topic := c.createTopic(paramName, "t")

	c.fails(http.StatusBadRequest, invalidParameter,
		"Invalid parameter: Attributes Reason: RawMessageDelivery: Invalid value [wrongvalue]. Must be true or false.",
		opSubscribe, paramTopicArn, topic, paramProtocol, "sqs", paramEndpoint, queueARN,
		attrKey1, attrRaw, attrValue1, "wrongvalue")

	arn := c.subscribeSQS(topic, attrRaw, "TrUe", attrScope, "MessageBody", "FilterPolicy", "")
	assert.True(t, strings.HasPrefix(arn, topic+":"), arn)

	// An empty FilterPolicy hides the scope too.
	assert.Equal(t, map[string]string{
		"ConfirmationWasAuthenticated": textTrue,
		"Endpoint":                     queueARN,
		"Owner":                        account,
		"PendingConfirmation":          "false",
		"Protocol":                     "sqs",
		attrRaw:                        textTrue,
		"SubscriptionArn":              arn,
		"SubscriptionPrincipal":        principal,
		"TopicArn":                     topic,
	}, c.subAttributes(arn))

	c.fails(http.StatusNotFound, "NotFound", "Subscription does not exist",
		"GetSubscriptionAttributes", paramSubArn, arn[:len(arn)-8]+"deadbeef")
}

func Test_Provider_Subscribe_idempotent(t *testing.T) {
	c := newClient(t)
	topic := c.createTopic(paramName, "t")

	arn := c.subscribeSQS(topic, attrRaw, "True")

	for _, attrs := range [][]string{
		{attrRaw, textTrue},
		{attrRaw, textTrue, attrScope, "MessageAttributes"},
		nil,
		{attrScope, "MessageAttributes"},
		{"DeliveryPolicy", ""},
	} {
		assert.Equal(t, arn, c.subscribeSQS(topic, attrs...), attrs)
	}

	c.fails(http.StatusBadRequest, invalidParameter,
		"Invalid parameter: Attributes Reason: Subscription already exists with different attributes",
		opSubscribe, paramTopicArn, topic, paramProtocol, "sqs", paramEndpoint, queueARN,
		attrKey1, attrRaw, attrValue1, "false",
		"Attributes.entry.2.key", attrScope, "Attributes.entry.2.value", "MessageBody")

	assert.Len(t, c.listSubs(topic, "").Subs, 1)
}

func Test_Provider_Subscribe_refusals(t *testing.T) {
	c := newClient(t)
	topic := c.createTopic(paramName, "t")
	sub := func(protocol, endpoint string) []string {
		return []string{paramTopicArn, topic, paramProtocol, protocol, paramEndpoint, endpoint}
	}

	tests := []struct {
		name    string
		status  int
		code    string
		message string
		params  []string
	}{
		{
			"protocol", 400, invalidParameter,
			"Invalid parameter: Amazon SNS does not support this protocol string: test-protocol",
			sub("test-protocol", "localstack@yopmail.com"),
		},
		{"no endpoint", 404, "NotFound", "Endpoint not specified in subscription", sub("sqs", "")},
		{
			"http endpoint", 400, invalidParameter, "Invalid parameter: Endpoint must match the specified protocol",
			sub("https", "http://example.com"),
		},
		{"sms", 400, invalidParameter, "Invalid SMS endpoint: +15--551234567", sub("sms", "+15--551234567")},
		{
			"sqs url", 400, invalidParameter, "Invalid parameter: SQS endpoint ARN",
			sub("sqs", "http://localhost:4566/000000000000/queue"),
		},
		{"application", 400, invalidParameter, "Invalid parameter: ApplicationEndpoint ARN", sub("application", "x")},
		{"fifo queue", 400, invalidParameter, "Invalid parameter: Invalid parameter: Endpoint Reason: " +
			"FIFO SQS Queues can not be subscribed to standard SNS topics", sub("sqs", queueARN+".fifo")},
		{
			"short topic", 400, invalidParameter,
			"Invalid parameter: TopicArn Reason: An ARN must have at least 6 elements, not 1",
			[]string{paramTopicArn, "randomstring", paramProtocol, "email", paramEndpoint, "a@b.c"},
		},
		{
			"unknown topic", 404, "NotFound", "Topic does not exist",
			[]string{paramTopicArn, topic + "aaa", paramProtocol, "email", paramEndpoint, "a@b.c"},
		},
		{
			"filter policy", 400, invalidParameter,
			"Invalid parameter: Attributes Reason: FilterPolicy: failed to parse JSON.",
			append(sub("email", "a@b.c"), attrKey1, "FilterPolicy", attrValue1, "invalid-json"),
		},
		{
			"attribute name", 400, invalidParameter, "Invalid parameter: Attributes Reason: AttributeName",
			append(sub("email", "a@b.c"), attrKey1, "Fake", attrValue1, "x"),
		},
		{
			"nested policy", 400, invalidParameter, "Invalid parameter: Attributes Reason: " +
				"Filter policy scope MessageAttributes does not support nested filter policy",
			append(sub("email", "a@b.c"), attrKey1, "FilterPolicy", attrValue1, `{"a":{"b":["c"]}}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c.with(t).fails(tt.status, tt.code, tt.message, opSubscribe, tt.params...)
		})
	}

	assert.Empty(t, c.listSubs(topic, "").Subs)
}

func Test_Provider_Subscribe_confirmation(t *testing.T) {
	c := newClient(t)
	topic := c.createTopic(paramName, "t")

	assert.Equal(t, "pending confirmation",
		c.subscribe(paramTopicArn, topic, paramProtocol, "http", paramEndpoint, "http://example.com/"))

	arn := c.subscribe(paramTopicArn, topic, paramProtocol, "http", paramEndpoint, "http://example.com/",
		paramReturn, textTrue)
	attrs := c.subAttributes(arn)
	assert.Equal(t, textTrue, attrs["PendingConfirmation"])
	assert.NotContains(t, attrs, "ConfirmationWasAuthenticated")

	email := c.subscribe(paramTopicArn, topic, paramProtocol, "email", paramEndpoint, "a@b.c")
	assert.Equal(t, textTrue, c.subAttributes(email)["PendingConfirmation"])

	all := c.listSubs("", "").Subs
	require.Len(t, all, 2)
	assert.Equal(t, "PendingConfirmation", all[0].SubscriptionArn)
	assert.Equal(t, arn, c.listSubs(topic, "").Subs[0].SubscriptionArn)

	c.fails(http.StatusBadRequest, invalidParameter, "Invalid parameter: Token",
		opConfirm, paramTopicArn, topic, "Token", strings.Repeat("ab", 32))
	c.fails(http.StatusBadRequest, invalidParameter, "Invalid token", opConfirm, paramTopicArn, topic, "Token", "random")
	c.fails(http.StatusBadRequest, invalidParameter, "Invalid parameter: Topic",
		opConfirm, paramTopicArn, "nope", "Token", "random")
}

func Test_Provider_Unsubscribe(t *testing.T) {
	c := newClient(t)
	topic := c.createTopic(paramName, "t")
	arn := c.subscribeSQS(topic)

	c.ok(nil, opUnsub, paramSubArn, arn)
	c.ok(nil, opUnsub, paramSubArn, arn)
	assert.Empty(t, c.listSubs(topic, "").Subs)
	c.fails(http.StatusNotFound, "NotFound", "Subscription does not exist",
		opSetAttrs, paramSubArn, arn, "AttributeName", attrRaw, "AttributeValue", textTrue)

	for _, tt := range []struct{ arn, message string }{
		{"randomstring", "Invalid parameter: SubscriptionArn Reason: An ARN must have at least 6 elements, not 1"},
		{
			"arn:aws:sns:us-east-1:random",
			"Invalid parameter: SubscriptionArn Reason: An ARN must have at least 6 elements, not 5",
		},
		{"arn:aws:sns:us-east-1:111111111111:random", "Invalid parameter: SubscriptionId"},
	} {
		c.fails(http.StatusBadRequest, invalidParameter, tt.message, opUnsub, paramSubArn, tt.arn)
	}

	c.fails(http.StatusBadRequest, invalidParameter,
		"Invalid parameter: SubscriptionArn Reason: no value for required parameter", opUnsub)
}

// Test_Provider_Unsubscribe_link: the UnsubscribeURL of a delivered message is
// an unsigned GET, and it deletes a subscription, pending or not.
func Test_Provider_Unsubscribe_link(t *testing.T) {
	c := newClient(t)
	topic := c.createTopic(paramName, "t")
	arn := c.subscribe(paramTopicArn, topic, paramProtocol, "https", paramEndpoint, "https://example.com/",
		paramReturn, textTrue)

	link := c.server.URL + "/?Action=Unsubscribe&SubscriptionArn=" + url.QueryEscape(arn)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, link, nil)
	require.NoError(t, err)

	resp, err := c.server.Client().Do(req)
	require.NoError(t, err)

	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Contains(t, string(body), "UnsubscribeResponse")
	assert.Empty(t, c.listSubs(topic, "").Subs)
}

func Test_Provider_SetSubscriptionAttributes(t *testing.T) {
	c := newClient(t)
	topic := c.createTopic(paramName, "t")
	arn := c.subscribeSQS(topic)

	set := func(name, value string) []string {
		return []string{paramSubArn, arn, "AttributeName", name, "AttributeValue", value}
	}

	for _, tt := range []struct{ name, value, message string }{
		{"FakeAttribute", "v", "Invalid parameter: AttributeName"},
		{attrRaw, "test-ValUe", "Invalid parameter: RawMessageDelivery: Invalid value [test-ValUe]. Must be true or false."},
		{attrRaw, "", "Invalid parameter: RawMessageDelivery: Invalid value []. Must be true or false."},
		{
			attrRedrive, `{"deadLetterTargetArn": "fake-arn"}`,
			"Invalid parameter: RedrivePolicy: deadLetterTargetArn is an invalid arn",
		},
		{attrRedrive, "null", "Invalid parameter: RedrivePolicy: deadLetterTargetArn is an invalid arn"},
		{attrRedrive, "{invalidjson}", "Invalid parameter: RedrivePolicy: failed to parse JSON."},
		{"FilterPolicy", "{invalidjson}", "Invalid parameter: FilterPolicy: failed to parse JSON."},
		{attrScope, "RandomValue", "Invalid parameter: FilterPolicyScope: Invalid value [RandomValue]. " +
			"Please use either MessageBody or MessageAttributes"},
	} {
		c.fails(http.StatusBadRequest, invalidParameter, tt.message, opSetAttrs, set(tt.name, tt.value)...)
	}

	c.ok(nil, opSetAttrs, set(attrRaw, "TRUE")...)
	c.ok(nil, opSetAttrs, set(attrRedrive, `{"deadLetterTargetArn": "`+queueARN+`-dlq"}`)...)
	c.ok(nil, opSetAttrs, set("DeliveryPolicy", "")...)

	attrs := c.subAttributes(arn)
	assert.Equal(t, textTrue, attrs[attrRaw])
	assert.JSONEq(t, `{"deadLetterTargetArn": "`+queueARN+`-dlq"}`, attrs[attrRedrive])
	assert.Empty(t, attrs["DeliveryPolicy"])
	assert.Contains(t, attrs, "DeliveryPolicy")
	assert.NotContains(t, attrs, "SubscriptionRoleArn")
}

// Test_Provider_filterPolicyScope is TestSNSFilterPolicyCrud: the scope shows
// only with a policy, and a nested policy needs the MessageBody scope.
func Test_Provider_filterPolicyScope(t *testing.T) {
	c := newClient(t)
	topic := c.createTopic(paramName, "t")
	arn := c.subscribeSQS(topic)
	set := func(name, value string) []string {
		return []string{paramSubArn, arn, "AttributeName", name, "AttributeValue", value}
	}

	assert.NotContains(t, c.subAttributes(arn), attrScope)

	nested := `{"object": {"key": [{"prefix": "auto-"}]}}`
	c.fails(http.StatusBadRequest, invalidParameter,
		"Invalid parameter: Filter policy scope MessageAttributes does not support nested filter policy",
		opSetAttrs, set("FilterPolicy", nested)...)

	c.ok(nil, opSetAttrs, set(attrScope, "MessageBody")...)
	assert.NotContains(t, c.subAttributes(arn), attrScope)

	c.ok(nil, opSetAttrs, set("FilterPolicy", nested)...)
	attrs := c.subAttributes(arn)
	assert.Equal(t, "MessageBody", attrs[attrScope])
	assert.Equal(t, nested, attrs["FilterPolicy"])

	c.fails(http.StatusBadRequest, invalidParameter,
		"Invalid parameter: FilterPolicy: Filter policy can not have more than 5 keys",
		opSetAttrs, set("FilterPolicy", `{"a":[1],"b":[1],"c":[1],"d":[1],"e":[1],"f":[1]}`)...)

	c.ok(nil, opSetAttrs, set("FilterPolicy", "")...)
	attrs = c.subAttributes(arn)
	assert.NotContains(t, attrs, "FilterPolicy")
	assert.NotContains(t, attrs, attrScope)
}

func Test_Provider_ListSubscriptions_pages(t *testing.T) {
	c := newClient(t)
	topic := c.createTopic(paramName, "t")
	other := c.createTopic(paramName, "u")

	arns := make([]string, 0, 101)
	for i := range 101 {
		number := "+12312312" + strconv.Itoa(i)
		arns = append(arns, c.subscribe(paramTopicArn, topic, paramProtocol, "sms", paramEndpoint, number))
	}

	c.subscribe(paramTopicArn, other, paramProtocol, "sms", paramEndpoint, "+1555")

	first := c.listSubs(topic, "")
	require.Len(t, first.Subs, 100)
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte(arns[100])), first.Next)

	second := c.listSubs(topic, first.Next)
	require.Len(t, second.Subs, 1)
	assert.Equal(t, arns[100], second.Subs[0].SubscriptionArn)
	assert.Empty(t, second.Next)

	all := c.listSubs("", "")
	require.Len(t, all.Subs, 100)
	assert.Len(t, c.listSubs("", all.Next).Subs, 2)
	assert.Len(t, c.listSubs("", "bm90LWEtc3Vi").Subs, 100, "a token that names nothing starts from the first")
}

// Test_Provider_DeleteTopic_subscriptions: deleting a topic deletes its
// subscriptions, as AWS's DeleteTopic reference says, and only its own.
func Test_Provider_DeleteTopic_subscriptions(t *testing.T) {
	c := newClient(t)
	topic := c.createTopic(paramName, "t")
	other := c.createTopic(paramName, "u")
	arn := c.subscribeSQS(topic)
	kept := c.subscribeSQS(other)

	c.ok(nil, "DeleteTopic", paramTopicArn, topic)
	c.fails(http.StatusNotFound, "NotFound", "Topic does not exist", "ListSubscriptionsByTopic", paramTopicArn, topic)
	c.fails(http.StatusNotFound, "NotFound", "Subscription does not exist", "GetSubscriptionAttributes", paramSubArn, arn)
	c.ok(nil, opUnsub, paramSubArn, arn)

	all := c.listSubs("", "").Subs
	require.Len(t, all, 1)
	assert.Equal(t, kept, all[0].SubscriptionArn)

	topic = c.createTopic(paramName, "t")
	assert.NotEqual(t, arn, c.subscribeSQS(topic), "the new topic has no subscription to the queue")
	assert.Len(t, c.listSubs(topic, "").Subs, 1)
	assert.Len(t, c.listSubs("", "").Subs, 2)
}

func Test_Provider_Subscribe_concurrent(t *testing.T) {
	c := newClient(t)
	topic := c.createTopic(paramName, "t")

	const workers = 16

	arns := make([]string, workers)

	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			r := c.call(opSubscribe, paramTopicArn, topic, paramProtocol, "sqs", paramEndpoint, queueARN, paramReturn, textTrue)

			var out struct {
				SubscriptionArn string `xml:"SubscribeResult>SubscriptionArn"`
			}

			if assert.Equal(t, http.StatusOK, r.status, r.body) {
				assert.NoError(t, xml.Unmarshal([]byte(r.body), &out))
			}

			arns[i] = out.SubscriptionArn
		})
	}

	wg.Wait()

	for _, arn := range arns {
		assert.Equal(t, arns[0], arn)
	}

	assert.Len(t, c.listSubs(topic, "").Subs, 1)
}
