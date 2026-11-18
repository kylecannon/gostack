package sqs_test

import (
	"crypto/tls"
	"encoding/base64"
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
	"sync/atomic"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/stretchr/testify/require"
)

const (
	defaultAccount = "000000000000"
	usEast1        = "us-east-1"
	euWest1        = "eu-west-1"
	localstackHost = "localhost.localstack.cloud:4566"
	formType       = "application/x-www-form-urlencoded"
	jsonType       = "application/x-amz-json-1.0"
)

// protocols are the wire protocols every operation test runs over.
var protocols = []string{protoJSON, protoQuery}

// liveSettings holds the settings of the provider under test and lets a test
// change them while it runs.
type liveSettings struct {
	current atomic.Pointer[sqs.Settings]
}

func newSettings() *liveSettings {
	s := &liveSettings{}
	s.update(func(settings *sqs.Settings) {
		settings.EndpointStrategy = sqs.StrategyStandard
		settings.Host = localstackHost
	})

	return s
}

func (s *liveSettings) get() sqs.Settings { return *s.current.Load() }

func (s *liveSettings) update(mutate func(*sqs.Settings)) {
	var next sqs.Settings
	if current := s.current.Load(); current != nil {
		next = *current
	}

	mutate(&next)
	s.current.Store(&next)
}

// stack is the SQS provider behind the real gateway, called in process.
type stack struct {
	t        *testing.T
	handler  http.Handler
	settings *liveSettings
	provider *sqs.Provider
}

func newStack(t *testing.T) *stack {
	t.Helper()

	settings := newSettings()
	provider := sqs.New(settings.get)

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{"0.0.0.0:4566"}} },
		[]service.Registration{provider.Registration()},
		nil,
		nil,
	)
	require.NoError(t, err)

	// A message move task is a goroutine, and a test leaves none behind.
	t.Cleanup(provider.Close)

	return &stack{t: t, handler: gw.Handler(), settings: settings, provider: provider}
}

// httpCall describes one request to the gateway.
type httpCall struct {
	method  string
	target  string
	host    string
	body    string
	headers map[string]string
	tls     bool
}

// reply is a response with its body read.
type reply struct {
	status int
	header http.Header
	body   string
}

func (s *stack) send(c httpCall) reply {
	s.t.Helper()

	method := c.method
	if method == "" {
		method = http.MethodPost
	}

	target := c.target
	if target == "" {
		target = "/"
	}

	req := httptest.NewRequestWithContext(s.t.Context(), method, target, strings.NewReader(c.body))
	if c.host != "" {
		req.Host = c.host
	}

	if c.tls {
		req.TLS = &tls.ConnectionState{}
	}

	for name, value := range c.headers {
		req.Header.Set(name, value)
	}

	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)

	resp := rec.Result()

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(s.t, err)

	return reply{status: resp.StatusCode, header: resp.Header, body: string(body)}
}

// signed returns a SigV4 Authorization header for sqs.
func signed(accessKey, region string) string {
	return "AWS4-HMAC-SHA256 Credential=" + accessKey + "/20130524/" + region + "/sqs/aws4_request, " +
		"SignedHeaders=host;x-amz-date, Signature=abc"
}

// caller is who makes the calls: an access key and a region, over one
// protocol.
type caller struct {
	s        *stack
	protocol string
	key      string
	region   string
	host     string
	tls      bool
}

func (s *stack) as(protocol string) caller {
	return caller{s: s, protocol: protocol, key: "test", region: usEast1}
}

func (c caller) inRegion(region string) caller {
	c.region = region

	return c
}

func (c caller) withKey(key string) caller {
	c.key = key

	return c
}

func (c caller) onHost(host string) caller {
	c.host = host

	return c
}

func (c caller) overTLS() caller {
	c.tls = true

	return c
}

// result is a decoded response, read the same way for both protocols.
type result struct {
	t      *testing.T
	status int
	header http.Header
	json   map[string]any
	xml    node
	query  bool
}

// node is an XML element, whatever it is.
type node struct {
	XMLName  xml.Name
	Text     string `xml:",chardata"`
	Children []node `xml:",any"`
}

func (n node) child(name string) node {
	for _, c := range n.Children {
		if c.XMLName.Local == name {
			return c
		}
	}

	return node{}
}

func (n node) all(name string) []node {
	var found []node

	for _, c := range n.Children {
		if c.XMLName.Local == name {
			found = append(found, c)
		}
	}

	return found
}

func (n node) text(name string) string {
	return n.child(name).Text
}

// call makes one operation call. params use the json names of the members.
func (c caller) call(operation string, params map[string]any) result {
	c.s.t.Helper()

	headers := map[string]string{headerAuthorization: signed(c.key, c.region)}

	var body string

	if c.protocol == protoJSON {
		encoded, err := json.Marshal(params)
		require.NoError(c.s.t, err)

		body = string(encoded)
		headers["Content-Type"] = jsonType
		headers["X-Amz-Target"] = "AmazonSQS." + operation
	} else {
		body = queryForm(operation, params).Encode()
		headers["Content-Type"] = formType
	}

	return c.decode(c.s.send(httpCall{host: c.host, body: body, headers: headers, tls: c.tls}))
}

func (c caller) decode(r reply) result {
	c.s.t.Helper()

	out := result{t: c.s.t, status: r.status, header: r.header, query: c.protocol == protoQuery}

	if out.query {
		require.NoError(c.s.t, xml.Unmarshal([]byte(r.body), &out.xml), r.body) //nolint:musttag // node reads any element

		return out
	}

	if r.body != "" {
		require.NoError(c.s.t, json.Unmarshal([]byte(r.body), &out.json), r.body)
	}

	return out
}

// queryForm turns json-named parameters into the form fields of the query
// protocol.
func queryForm(operation string, params map[string]any) url.Values {
	form := url.Values{paramAction: {operation}, paramVersion: {"2012-11-05"}}

	for name, value := range params {
		addParam(form, operation, "", name, value)
	}

	return form
}

// queryListNames are the member names of the lists a test passes.
var queryListNames = map[string]string{
	paramAttributeNames:   "AttributeName",
	"AWSAccountIds":       "AWSAccountId",
	"Actions":             "ActionName",
	paramTagKeys:          "TagKey",
	paramMessageAttrNames: "MessageAttributeName",
	paramSystemAttrNames:  "AttributeName",
}

// addParam writes one parameter in the query form. prefix is "" for a member of
// the request and "<Entry>.<n>." for a member of a batch entry.
func addParam(form url.Values, operation, prefix, name string, value any) {
	mapNames := map[string][2]string{
		paramAttributes: {"Attribute", "Name"},
		"tags":          {"Tag", "Key"},
		paramTags:       {"Tag", "Key"},
	}

	switch typed := value.(type) {
	case string:
		form.Set(prefix+name, typed)
	case int:
		form.Set(prefix+name, strconv.Itoa(typed))
	case []string:
		for i, item := range typed {
			form.Set(prefix+queryListNames[name]+"."+strconv.Itoa(i+1), item)
		}
	case map[string]string:
		element, key := mapNames[name][0], mapNames[name][1]

		for i, k := range sortedKeys(typed) {
			n := strconv.Itoa(i + 1)
			form.Set(prefix+element+"."+n+"."+key, k)
			form.Set(prefix+element+"."+n+".Value", typed[k])
		}
	case attrs:
		element := "MessageAttribute"
		if name == paramMessageSystemAttrs {
			element = "MessageSystemAttribute"
		}

		for i, k := range slices.Sorted(maps.Keys(typed)) {
			n := strconv.Itoa(i + 1)
			form.Set(prefix+element+"."+n+".Name", k)
			form.Set(prefix+element+"."+n+".Value.DataType", typed[k].DataType)

			if typed[k].StringValue != "" {
				form.Set(prefix+element+"."+n+".Value.StringValue", typed[k].StringValue)
			}

			if len(typed[k].BinaryValue) > 0 {
				form.Set(prefix+element+"."+n+".Value.BinaryValue", base64.StdEncoding.EncodeToString(typed[k].BinaryValue))
			}
		}
	case []map[string]any:
		for i, entry := range typed {
			for key, member := range entry {
				addParam(form, operation, prefix+operation+"RequestEntry."+strconv.Itoa(i+1)+".", key, member)
			}
		}
	default:
		// Every parameter of these tests is one of the types above.
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	return keys
}

// ok reports whether the call succeeded.
func (r result) ok() bool { return r.status == http.StatusOK }

// errorCode is the code a client sees: the json __type, or the Code of the
// query error.
func (r result) errorCode() string {
	if r.query {
		errNode := r.xml.child("Error")

		return errNode.text("Code")
	}

	code, _ := r.json["__type"].(string)

	return code
}

func (r result) errorMessage() string {
	if r.query {
		errNode := r.xml.child("Error")

		return errNode.text("Message")
	}

	message, _ := r.json["message"].(string)

	return message
}

// member is the node the output members are in: the <Operation>Result element.
func (r result) member() node {
	for _, c := range r.xml.Children {
		if strings.HasSuffix(c.XMLName.Local, "Result") {
			return c
		}
	}

	return node{}
}

// str reads a string member such as QueueUrl or NextToken. It is "" when absent.
func (r result) str(name string) string {
	if r.query {
		return r.member().text(name)
	}

	value, _ := r.json[name].(string)

	return value
}

// urls reads the queue URLs of a ListQueues result.
func (r result) urls() []string {
	var urls []string

	if r.query {
		for _, c := range r.member().all(paramQueueURL) {
			urls = append(urls, c.Text)
		}

		return urls
	}

	items, _ := r.json["QueueUrls"].([]any)
	for _, item := range items {
		text, _ := item.(string)
		urls = append(urls, text)
	}

	return urls
}

// attributes reads the Attributes of a GetQueueAttributes result.
func (r result) attributes() map[string]string {
	return r.pairs(paramAttributes, "Attribute", "Name")
}

// tags reads the Tags of a ListQueueTags result.
func (r result) tags() map[string]string {
	return r.pairs(paramTags, "Tag", "Key")
}

func (r result) pairs(jsonName, element, keyName string) map[string]string {
	pairs := map[string]string{}

	if r.query {
		for _, c := range r.member().all(element) {
			pairs[c.text(keyName)] = c.text("Value")
		}

		return pairs
	}

	fields, _ := r.json[jsonName].(map[string]any)
	for key, value := range fields {
		text, _ := value.(string)
		pairs[key] = text
	}

	return pairs
}

// createQueue creates a queue and returns its URL, failing the test when the
// call fails.
func (c caller) createQueue(name string, attrs map[string]string) string {
	c.s.t.Helper()

	params := map[string]any{paramQueueName: name}
	if attrs != nil {
		params[paramAttributes] = attrs
	}

	res := c.call("CreateQueue", params)
	require.True(c.s.t, res.ok(), "CreateQueue %s: %s %s", name, res.errorCode(), res.errorMessage())

	return res.str(paramQueueURL)
}
