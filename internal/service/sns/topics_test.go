package sns_test

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	region    = "us-east-1"
	account   = "000000000000"
	arnPrefix = "arn:aws:sns:" + region + ":" + account + ":"

	paramName   = "Name"
	displayName = "DisplayName"
	attrKey1    = "Attributes.entry.1.key"
	attrValue1  = "Attributes.entry.1.value"
	tagKey1     = "Tags.member.1.Key"
	tagValue1   = "Tags.member.1.Value"
)

// client sends SNS query calls to the real gateway with only SNS registered.
type client struct {
	t      *testing.T
	server *httptest.Server
}

func newClient(t *testing.T) *client {
	t.Helper()

	g, err := gateway.New(func() gateway.Settings { return gateway.Settings{Listen: []string{"0.0.0.0:4566"}} },
		[]service.Registration{sns.New(nil).Registration()}, nil, nil)
	require.NoError(t, err)

	server := httptest.NewServer(g.Handler())
	t.Cleanup(server.Close)

	return &client{t: t, server: server}
}

type reply struct {
	status int
	body   string
}

type errorReply struct {
	Type    string `xml:"Error>Type"`
	Code    string `xml:"Error>Code"`
	Message string `xml:"Error>Message"`
}

// call sends one action with params in region regionName and returns the reply.
func (c *client) callIn(regionName, action string, params ...string) reply {
	form := url.Values{"Action": {action}, "Version": {"2010-03-31"}}
	for i := 0; i+1 < len(params); i += 2 {
		form.Add(params[i], params[i+1])
	}

	req, err := http.NewRequestWithContext(c.t.Context(), http.MethodPost, c.server.URL+"/",
		strings.NewReader(form.Encode()))
	if err != nil {
		c.t.Errorf("request: %v", err)

		return reply{}
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20130524/"+regionName+
		"/sns/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc")

	resp, err := c.server.Client().Do(req)
	if err != nil {
		c.t.Errorf("call %s: %v", action, err)

		return reply{}
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Errorf("read %s: %v", action, err)
	}

	return reply{status: resp.StatusCode, body: string(body)}
}

// with returns the client for a subtest's t.
func (c *client) with(t *testing.T) *client {
	return &client{t: t, server: c.server}
}

func (c *client) call(action string, params ...string) reply {
	return c.callIn(region, action, params...)
}

// ok calls action, requires a 200 and decodes the body into out when out is not nil.
func (c *client) ok(out any, action string, params ...string) {
	c.t.Helper()

	r := c.call(action, params...)
	require.Equal(c.t, http.StatusOK, r.status, r.body)

	if out != nil {
		require.NoError(c.t, xml.Unmarshal([]byte(r.body), out))
	}
}

// fails calls action and requires the error with status, code and message.
func (c *client) fails(status int, code, message, action string, params ...string) {
	c.t.Helper()

	r := c.call(action, params...)
	require.Equal(c.t, status, r.status, r.body)

	var got errorReply
	require.NoError(c.t, xml.Unmarshal([]byte(r.body), &got))
	assert.Equal(c.t, errorReply{Type: "Sender", Code: code, Message: message}, got)
}

func (c *client) createTopic(params ...string) string {
	c.t.Helper()

	var out struct {
		TopicArn string `xml:"CreateTopicResult>TopicArn"`
	}

	c.ok(&out, "CreateTopic", params...)

	return out.TopicArn
}

func (c *client) attributes(arn string) map[string]string {
	c.t.Helper()

	var out struct {
		Entries []mapEntry `xml:"GetTopicAttributesResult>Attributes>entry"`
	}

	c.ok(&out, "GetTopicAttributes", "TopicArn", arn)

	attrs := make(map[string]string, len(out.Entries))
	for _, e := range out.Entries {
		attrs[e.Key] = e.Value
	}

	return attrs
}

type mapEntry struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}

type tagMember struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

func (c *client) tags(arn string) [][2]string {
	c.t.Helper()

	var out struct {
		Tags []tagMember `xml:"ListTagsForResourceResult>Tags>member"`
	}

	c.ok(&out, "ListTagsForResource", "ResourceArn", arn)

	tags := make([][2]string, 0, len(out.Tags))
	for _, tag := range out.Tags {
		tags = append(tags, [2]string{tag.Key, tag.Value})
	}

	return tags
}

type listPage struct {
	Topics    []string `xml:"ListTopicsResult>Topics>member>TopicArn"`
	NextToken string   `xml:"ListTopicsResult>NextToken"`
}

func (c *client) listTopics(token string) listPage {
	c.t.Helper()

	var page listPage
	if token == "" {
		c.ok(&page, "ListTopics")
	} else {
		c.ok(&page, "ListTopics", "NextToken", token)
	}

	return page
}

type statement struct {
	Sid       string         `json:"Sid"`
	Effect    string         `json:"Effect"`
	Principal map[string]any `json:"Principal"`
	Action    any            `json:"Action"`
	Resource  string         `json:"Resource"`
	Condition map[string]any `json:"Condition,omitempty"`
}

func policyOf(t *testing.T, attrs map[string]string) []statement {
	t.Helper()

	var doc struct {
		Version   string      `json:"Version"`
		ID        string      `json:"Id"`
		Statement []statement `json:"Statement"`
	}

	require.NoError(t, json.Unmarshal([]byte(attrs["Policy"]), &doc))
	assert.Equal(t, "2008-10-17", doc.Version)
	assert.Equal(t, "__default_policy_ID", doc.ID)

	return doc.Statement
}

func Test_Provider_CreateTopic(t *testing.T) {
	t.Parallel()

	c := newClient(t)

	arn := c.createTopic(paramName, "topic-a")
	require.Equal(t, arnPrefix+"topic-a", arn)

	attrs := c.attributes(arn)
	assert.Equal(t, []string{
		displayName, "EffectiveDeliveryPolicy", "Owner", "Policy",
		"SubscriptionsConfirmed", "SubscriptionsDeleted", "SubscriptionsPending", "TopicArn",
	}, slices.Sorted(maps.Keys(attrs)))
	assert.Equal(t, account, attrs["Owner"])
	assert.Equal(t, arn, attrs["TopicArn"])
	assert.Equal(t, "0", attrs["SubscriptionsPending"])
	assert.True(t, json.Valid([]byte(attrs["EffectiveDeliveryPolicy"])))

	statements := policyOf(t, attrs)
	require.Len(t, statements, 1)
	assert.Equal(t, "__default_statement_ID", statements[0].Sid)
	assert.Equal(t, arn, statements[0].Resource)
	assert.Equal(t, map[string]any{"StringEquals": map[string]any{"AWS:SourceOwner": account}},
		statements[0].Condition)

	t.Run("FIFO topic keeps its attributes over the defaults", func(t *testing.T) {
		t.Parallel()

		sub := c.with(t)

		fifo := sub.createTopic(paramName, "topictest.fifo", attrKey1, "FifoTopic",
			attrValue1, "true", "Attributes.entry.2.key", displayName,
			"Attributes.entry.2.value", "TestTopic", "Attributes.entry.3.key", "SignatureVersion",
			"Attributes.entry.3.value", "2")

		got := sub.attributes(fifo)
		assert.Equal(t, "true", got["FifoTopic"])
		assert.Equal(t, "false", got["ContentBasedDeduplication"])
		assert.Equal(t, "TestTopic", got[displayName])
		assert.Equal(t, "2", got["SignatureVersion"])
	})

	t.Run("FifoTopic=false is not kept", func(t *testing.T) {
		t.Parallel()

		sub := c.with(t)

		standard := sub.createTopic(paramName, "standard-topic", attrKey1, "FifoTopic",
			attrValue1, "false")
		assert.NotContains(t, sub.attributes(standard), "FifoTopic")
		sub.fails(http.StatusBadRequest, "InvalidParameter", "Invalid parameter: AttributeName",
			"SetTopicAttributes", "TopicArn", standard, "AttributeName", "FifoTopic", "AttributeValue", "false")
	})
}

func Test_Provider_CreateTopic_repeated(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	const (
		diffAttrs = "Invalid parameter: Attributes Reason: Topic already exists with different attributes"
		diffTags  = "Invalid parameter: Tags Reason: Topic already exists with different tags"
	)

	arn := c.createTopic(paramName, "repeated", attrKey1, displayName,
		attrValue1, "Value1", tagKey1, "a", tagValue1, "1",
		"Tags.member.2.Key", "b", "Tags.member.2.Value", "2")

	tests := []struct {
		name     string
		give     []string
		wantFail string
	}{
		{name: "no attributes", give: nil},
		{name: "same attribute", give: []string{attrKey1, displayName, attrValue1, "Value1"}},
		{name: "FifoTopic false", give: []string{attrKey1, "FifoTopic", attrValue1, "false"}},
		{name: "one of the tags", give: []string{tagKey1, "a", tagValue1, "1"}},
		{name: "empty tag list", give: []string{"Tags", ""}},
		{
			name: "different attribute", wantFail: diffAttrs,
			give: []string{attrKey1, displayName, attrValue1, "Value2"},
		},
		{
			name: "new attribute", wantFail: diffAttrs,
			give: []string{attrKey1, "SignatureVersion", attrValue1, "2"},
		},
		{name: "new tag", wantFail: diffTags, give: []string{tagKey1, "c", tagValue1, "3"}},
		{name: "tag value differs", wantFail: diffTags, give: []string{tagKey1, "a", tagValue1, "9"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sub := c.with(t)

			params := append([]string{paramName, "repeated"}, tt.give...)
			if tt.wantFail != "" {
				sub.fails(http.StatusBadRequest, "InvalidParameter", tt.wantFail, "CreateTopic", params...)

				return
			}

			assert.Equal(t, arn, sub.createTopic(params...))
		})
	}

	c.fails(http.StatusBadRequest, "InvalidParameter", "Invalid parameter: Topic Name", "CreateTopic",
		paramName, "repeated", attrKey1, "FifoTopic", attrValue1, "true")
	assert.Equal(t, "Value1", c.attributes(arn)[displayName])
}

func Test_Provider_CreateTopic_names(t *testing.T) {
	t.Parallel()

	c := newClient(t)

	tests := []struct {
		name     string
		giveName string
		giveFIFO bool
		wantOK   bool
	}{
		{name: "longest", giveName: strings.Repeat("a", 256), wantOK: true},
		{name: "FIFO", giveName: "ok-name_1.fifo", giveFIFO: true, wantOK: true},
		{name: "longest FIFO", giveName: strings.Repeat("b", 256) + ".fifo", giveFIFO: true, wantOK: true},
		{name: "empty", giveName: ""},
		{name: "too long", giveName: strings.Repeat("a", 257)},
		{name: "fifo suffix on a standard topic", giveName: "test.fifo"},
		{name: "FIFO without the suffix", giveName: "test", giveFIFO: true},
		{name: "colon", giveName: "bad:name"},
		{name: "space", giveName: "bad name"},
		{name: "percent", giveName: "bad%name"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sub := c.with(t)

			params := []string{paramName, tt.giveName}
			if tt.giveFIFO {
				params = append(params, attrKey1, "FifoTopic", attrValue1, "true")
			}

			if tt.wantOK {
				assert.Equal(t, arnPrefix+tt.giveName, sub.createTopic(params...))

				return
			}

			sub.fails(http.StatusBadRequest, "InvalidParameter", "Invalid parameter: Topic Name", "CreateTopic", params...)
		})
	}
}

func Test_Provider_topicLookups(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	arn := c.createTopic(paramName, "lookup")

	c.fails(http.StatusNotFound, "NotFound", "Topic does not exist", "GetTopicAttributes", "TopicArn", arn+"x")
	c.fails(http.StatusBadRequest, "InvalidParameter",
		"Invalid parameter: TopicArn Reason: An ARN must have at least 6 elements, not 1",
		"GetTopicAttributes", "TopicArn", "test-topic")
	c.fails(http.StatusBadRequest, "InvalidParameter", "Invalid parameter: TopicArn",
		"GetTopicAttributes", "TopicArn", "arn:aws:sns:eu-west-1:"+account+":lookup")

	c.ok(nil, "SetTopicAttributes", "TopicArn", arn, "AttributeName", displayName, "AttributeValue", "AlreadySet")
	assert.Equal(t, arn, c.createTopic(paramName, "lookup"))
	assert.Equal(t, "AlreadySet", c.attributes(arn)[displayName])

	c.ok(nil, "DeleteTopic", "TopicArn", arn)
	c.fails(http.StatusNotFound, "NotFound", "Topic does not exist", "GetTopicAttributes", "TopicArn", arn)
	c.ok(nil, "DeleteTopic", "TopicArn", arn)
	c.ok(nil, "DeleteTopic", "TopicArn", arnPrefix+"never-made")
}

func Test_Provider_tags(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	arn := c.createTopic(paramName, "tagged")

	c.fails(http.StatusBadRequest, "InvalidParameter", "Invalid parameter: Duplicated keys are not allowed.",
		"TagResource", "ResourceArn", arn, tagKey1, "k1", tagValue1, "v1",
		"Tags.member.2.Key", "k1", "Tags.member.2.Value", "v1")
	assert.Empty(t, c.tags(arn))

	c.ok(nil, "TagResource", "ResourceArn", arn, tagKey1, "k1", tagValue1, "v1",
		"Tags.member.2.Key", "k2", "Tags.member.2.Value", "v2")
	assert.Equal(t, [][2]string{{"k1", "v1"}, {"k2", "v2"}}, c.tags(arn))

	c.ok(nil, "UntagResource", "ResourceArn", arn, "TagKeys.member.1", "k1")
	assert.Equal(t, [][2]string{{"k2", "v2"}}, c.tags(arn))

	c.ok(nil, "TagResource", "ResourceArn", arn, tagKey1, "k2", tagValue1, "v2b")
	assert.Equal(t, [][2]string{{"k2", "v2b"}}, c.tags(arn))

	c.ok(nil, "DeleteTopic", "TopicArn", arn)
	assert.Equal(t, arn, c.createTopic(paramName, "tagged", tagKey1, paramName, tagValue1, "abc"))
	assert.Equal(t, [][2]string{{paramName, "abc"}}, c.tags(arn))
}

func Test_Provider_ListTopics(t *testing.T) {
	t.Parallel()

	c := newClient(t)

	arns := make([]string, 0, 120)
	for i := range 120 {
		arns = append(arns, c.createTopic(paramName, "paging-"+strconv.Itoa(i)))
	}

	first := c.listTopics("")
	require.Equal(t, arns[:100], first.Topics)
	require.NotEmpty(t, first.NextToken)

	second := c.listTopics(first.NextToken)
	assert.Equal(t, arns[100:], second.Topics)
	assert.Empty(t, second.NextToken)

	assert.Equal(t, arns[:100], c.listTopics("not-a-token").Topics, "a token that names no topic starts over")

	c.ok(nil, "DeleteTopic", "TopicArn", arns[0])
	c.createTopic(paramName, "paging-0")

	last := c.listTopics(c.listTopics("").NextToken)
	assert.Equal(t, append(arns[101:], arns[0]), last.Topics, "a topic made again goes last")

	var other listPage
	r := c.callIn("eu-west-1", "ListTopics")
	require.Equal(t, http.StatusOK, r.status)
	require.NoError(t, xml.Unmarshal([]byte(r.body), &other))
	assert.Empty(t, other.Topics, "topics belong to their region")
}

func Test_Provider_permissions(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	arn := c.createTopic(paramName, "permitted")

	c.ok(nil, "AddPermission", "TopicArn", arn, "Label", "test", "AWSAccountId.member.1", account,
		"ActionName.member.1", "Publish", "ActionName.member.2", "Subscribe")

	statements := policyOf(t, c.attributes(arn))
	require.Len(t, statements, 2)
	assert.Equal(t, statement{
		Sid: "test", Effect: "Allow", Principal: map[string]any{"AWS": "arn:aws:iam::" + account + ":root"},
		Action: []any{"SNS:Publish", "SNS:Subscribe"}, Resource: arn,
	}, statements[1])

	c.fails(http.StatusBadRequest, "InvalidParameter", "Invalid parameter: Statement already exists",
		"AddPermission", "TopicArn", arn, "Label", "test", "AWSAccountId.member.1", account,
		"ActionName.member.1", "AddPermission")
	c.fails(http.StatusNotFound, "NotFound", "Topic does not exist",
		"AddPermission", "TopicArn", arn+"-not-existing", "Label", "test-2", "AWSAccountId.member.1", account,
		"ActionName.member.1", "AddPermission")
	c.fails(http.StatusBadRequest, "InvalidParameter", "Invalid parameter: Policy statement action out of service scope!",
		"AddPermission", "TopicArn", arn, "Label", "test-2", "AWSAccountId.member.1", account,
		"ActionName.member.1", "InvalidAction")
	c.fails(http.StatusNotFound, "NotFound", "Topic does not exist",
		"RemovePermission", "TopicArn", arn+"-not-existing", "Label", "test")

	c.ok(nil, "RemovePermission", "TopicArn", arn, "Label", "test")
	statements = policyOf(t, c.attributes(arn))
	require.Len(t, statements, 1)
	assert.Equal(t, "__default_statement_ID", statements[0].Sid)
}

func Test_Provider_dataProtectionPolicy(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	arn := c.createTopic(paramName, "protected")

	type policyReply struct {
		Policy *string `xml:"GetDataProtectionPolicyResult>DataProtectionPolicy"`
	}

	var none policyReply
	c.ok(&none, "GetDataProtectionPolicy", "ResourceArn", arn)
	assert.Nil(t, none.Policy)

	policy := `{"Name":"data_protection_policy","Version":"2021-06-01","Statement":[]}`
	c.ok(nil, "PutDataProtectionPolicy", "ResourceArn", arn, "DataProtectionPolicy", policy)

	var got policyReply
	c.ok(&got, "GetDataProtectionPolicy", "ResourceArn", arn)
	require.NotNil(t, got.Policy)
	assert.JSONEq(t, policy, *got.Policy)
	assert.NotContains(t, c.attributes(arn), "DataProtectionPolicy")
}

// Test_Provider_concurrent runs the topic operations on one topic at once, for
// the race detector.
func Test_Provider_concurrent(t *testing.T) {
	t.Parallel()

	c := newClient(t)
	arn := arnPrefix + "contended"

	var wg sync.WaitGroup
	for i := range 8 {
		label := "label-" + strconv.Itoa(i)

		wg.Go(func() {
			for range 10 {
				c.call("CreateTopic", paramName, "contended")
				c.call("TagResource", "ResourceArn", arn, tagKey1, label, tagValue1, "v")
				c.call("AddPermission", "TopicArn", arn, "Label", label, "AWSAccountId.member.1", account,
					"ActionName.member.1", "Publish")
				c.call("RemovePermission", "TopicArn", arn, "Label", label)
				c.call("GetTopicAttributes", "TopicArn", arn)
				c.call("ListTopics")
				c.call("DeleteTopic", "TopicArn", arn)
			}
		})
	}

	wg.Wait()

	c.createTopic(paramName, "contended")
	assert.Len(t, policyOf(t, c.attributes(arn)), 1)
}
