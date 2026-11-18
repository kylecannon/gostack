package sqs_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/api/openapi"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

const (
	devCompat    = "/_aws/sqs/messages"
	devCanonical = "/_gostack/services/sqs/messages"

	devIsVisible = "IsVisible"
	devIsDelayed = "IsDelayed"
	devHandle    = "SQS/BACKDOOR/ACCESS"
	devNoQueue   = "AWS.SimpleQueueService.NonExistentQueue"

	devTaken   = "taken"
	devVisible = "visible"
	devLater   = "later"

	devQueueName = "dev-queue"
	devQueuePath = "/us-east-1/" + defaultAccount + "/" + devQueueName
	devBothFlags = "&ShowInvisible=true&ShowDelayed=true"

	devHeaderAccept = "Accept"
	devHeaderType   = "Content-Type"
	devHeaderTarget = "X-Amz-Target"
	devTargetRecv   = "AmazonSQS.ReceiveMessage"
	devTypeAppJS    = "application/json"
	devTypeXML      = "application/xml"
	devTypeText     = "text/xml"
	devTypeProtocol = "application/x-amz-json-1.0"

	devText   = "text"
	devBinary = "Binary"
	devName   = "Name"
	devValue  = "Value"

	devLocalstackSpec = "../../../specs/localstack/openapi.yaml"
	devSpecResource   = "openapi.json"
)

var devPaths = []string{devCompat, devCanonical}

// newDevStack is the SQS provider behind the gateway, with the settings changed
// by mutate before the routes are mounted.
func newDevStack(t *testing.T, mutate func(*sqs.Settings)) *stack {
	t.Helper()

	settings := newSettings()
	if mutate != nil {
		settings.update(mutate)
	}

	provider := sqs.New(settings.get)

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{"0.0.0.0:4566"}} },
		[]service.Registration{provider.Registration()},
		nil,
		nil,
	)
	require.NoError(t, err)

	return &stack{t: t, handler: gw.Handler(), settings: settings, provider: provider}
}

// devFixture is a queue with one message in each state: "taken" was received
// and is in flight, "visible" can be received, and "later" is delayed.
type devFixture struct {
	s     *stack
	c     caller
	url   string
	taken received
}

func newDevFixture(t *testing.T, mutate func(*sqs.Settings)) devFixture {
	t.Helper()

	s := newDevStack(t, mutate)
	c := s.as(protoJSON)
	queueURL := c.createQueue(devQueueName, nil)

	c.send(queueURL, devTaken, nil)

	got := c.receive(queueURL, map[string]any{paramMaxMessages: 1, paramVisibilityTimeout: 300})
	require.Len(t, got, 1)

	c.send(queueURL, devVisible, nil)
	c.send(queueURL, devLater, map[string]any{paramDelaySeconds: 900})

	return devFixture{s: s, c: c, url: queueURL, taken: got[0]}
}

// get calls the developer API with a GET.
func (f devFixture) get(target string, headers map[string]string) reply {
	f.s.t.Helper()

	return f.s.send(httpCall{method: http.MethodGet, target: target, headers: headers})
}

// withQueue is a target with the QueueUrl of the fixture's queue.
func (f devFixture) withQueue(path, extra string) string {
	return path + "?QueueUrl=" + url.QueryEscape(f.url) + extra
}

// list is the XML listing of the queue with the given extra query string.
func (f devFixture) list(path, extra string) []received {
	f.s.t.Helper()

	res := f.s.as(protoQuery).decode(f.get(f.withQueue(path, extra), nil))
	require.True(f.s.t, res.ok(), "%s: %s", res.errorCode(), res.errorMessage())

	return res.messages()
}

// asJSON is the header of a caller that wants the JSON form of the XML.
func asJSON() map[string]string { return map[string]string{devHeaderAccept: devTypeAppJS} }

// asProtocol is the header that makes a call a json protocol call.
func asProtocol() map[string]string { return map[string]string{devHeaderType: devTypeProtocol} }

// hdr reads a response header whatever case it was written in: the AWS headers
// are written as LocalStack spells them, not canonicalized.
func hdr(r reply, name string) string {
	for key, values := range r.header {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}

	return ""
}

func bodies(messages []received) []string {
	found := make([]string, len(messages))
	for i, m := range messages {
		found[i] = m.body
	}

	return found
}

func Test_DeveloperAPI_flags(t *testing.T) {
	f := newDevFixture(t, nil)

	tests := []struct {
		name      string
		give      string
		want      []string
		invisible bool
		delayed   bool
	}{
		{name: "no flags", give: "", want: []string{devVisible}},
		{name: "ShowInvisible", give: "&ShowInvisible=true", want: []string{devTaken, devVisible}, invisible: true},
		{name: "ShowInvisible as 1", give: "&ShowInvisible=1", want: []string{devTaken, devVisible}, invisible: true},
		{
			name: "ShowInvisible in any case", give: "&ShowInvisible=TRUE",
			want: []string{devTaken, devVisible}, invisible: true,
		},
		{name: "ShowInvisible false", give: "&ShowInvisible=false", want: []string{devVisible}},
		{name: "ShowInvisible yes is not true", give: "&ShowInvisible=yes", want: []string{devVisible}},
		{name: "ShowInvisible empty", give: "&ShowInvisible=", want: []string{devVisible}},
		{name: "ShowDelayed", give: "&ShowDelayed=true", want: []string{devVisible, devLater}, delayed: true},
		{name: "ShowDelayed as 1", give: "&ShowDelayed=1", want: []string{devVisible, devLater}, delayed: true},
		{
			name: "both flags", give: "&ShowInvisible=1&ShowDelayed=1",
			want: []string{devTaken, devVisible, devLater}, invisible: true, delayed: true,
		},
		{name: "ShowDeleted is not a flag", give: "&ShowDeleted=true", want: []string{devVisible}},
	}

	for _, path := range devPaths {
		for _, tt := range tests {
			t.Run(path+" "+tt.name, func(t *testing.T) {
				listed := f.list(path, tt.give)

				require.Equal(t, tt.want, bodies(listed))

				for _, m := range listed {
					require.Equal(t, devHandle, m.handle)

					_, hasVisible := m.system[devIsVisible]
					_, hasDelayed := m.system[devIsDelayed]
					require.Equal(t, tt.invisible, hasVisible, "IsVisible of %s", m.body)
					require.Equal(t, tt.delayed, hasDelayed, "IsDelayed of %s", m.body)

					if tt.invisible {
						require.Equal(t, strconv.FormatBool(m.body != devTaken), m.system[devIsVisible], m.body)
					}

					if tt.delayed {
						require.Equal(t, strconv.FormatBool(m.body == devLater), m.system[devIsDelayed], m.body)
					}
				}
			})
		}
	}
}

func Test_DeveloperAPI_deletedMessagesAreGone(t *testing.T) {
	f := newDevFixture(t, nil)

	res := f.c.call(opDeleteMessage, map[string]any{paramQueueURL: f.url, paramReceiptHandle: f.taken.handle})
	require.True(t, res.ok())

	require.Equal(t, []string{devVisible, devLater}, bodies(f.list(devCompat, devBothFlags+"&ShowDeleted=true")))
}

func Test_DeveloperAPI_peekHasNoSideEffects(t *testing.T) {
	f := newDevFixture(t, nil)

	counts := func() map[string]string {
		res := f.c.call(opGetQueueAttributes, map[string]any{paramQueueURL: f.url, paramAttributeNames: []string{attrAll}})
		require.True(t, res.ok())

		return res.attributes()
	}
	before := counts()

	for range 3 {
		listed := f.list(devCompat, devBothFlags)
		require.Len(t, listed, 3)

		for _, m := range listed {
			if m.body == devTaken {
				require.Equal(t, "1", m.system["ApproximateReceiveCount"])
				require.NotEqual(t, "0", m.system["ApproximateFirstReceiveTimestamp"])

				continue
			}

			require.Equal(t, "0", m.system["ApproximateReceiveCount"], m.body)
			require.Equal(t, "0", m.system["ApproximateFirstReceiveTimestamp"], m.body)
		}
	}

	after := counts()
	for _, name := range []string{
		"ApproximateNumberOfMessages", "ApproximateNumberOfMessagesNotVisible", "ApproximateNumberOfMessagesDelayed",
	} {
		require.Equal(t, before[name], after[name], name)
	}

	require.Equal(t, "1", after["ApproximateNumberOfMessages"])

	// The message the peeks showed is still there to receive, once.
	got := f.c.receive(f.url, map[string]any{paramSystemAttrNames: []string{attrAll}})
	require.Len(t, got, 1)
	require.Equal(t, devVisible, got[0].body)
	require.Equal(t, "1", got[0].system["ApproximateReceiveCount"])
	require.Empty(t, f.c.receive(f.url, nil))
}

// jsonAttribute is an Attribute of the JSON form of a ReceiveMessage response.
type jsonAttribute struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

// jsonMessage is a Message of that form. One message is an object and several
// are an array, as xmltodict makes of repeated elements.
type jsonMessage struct {
	Body             string           `json:"Body"`
	ReceiptHandle    string           `json:"ReceiptHandle"`
	Attribute        []jsonAttribute  `json:"Attribute"`
	MessageAttribute []map[string]any `json:"MessageAttribute"`
}

// jsonResponse is that form with the Message member of type M.
type jsonResponse[M any] struct {
	Result   jsonResult[M]     `json:"ReceiveMessageResult"`
	Metadata map[string]string `json:"ResponseMetadata"`
}

type jsonResult[M any] struct {
	Message M `json:"Message"`
}

type jsonDocument[M any] struct {
	Response jsonResponse[M] `json:"ReceiveMessageResponse"`
}

// decodeDocument reads the JSON form of a response whose Message is a M.
func decodeDocument[M any](t *testing.T, got reply) jsonResponse[M] {
	t.Helper()

	var doc jsonDocument[M]
	require.NoError(t, json.Unmarshal([]byte(got.body), &doc), got.body)

	return doc.Response
}

func Test_DeveloperAPI_userAttributesAndMD5(t *testing.T) {
	s := newDevStack(t, nil)
	c := s.as(protoJSON)
	queueURL := c.createQueue("attributes-queue", nil)
	sent := c.send(queueURL, "with attributes", map[string]any{paramMessageAttributes: attrs{
		"s": {DataType: typeString, StringValue: devText},
		"b": {DataType: devBinary, BinaryValue: []byte{1, 2, 3}},
	}})

	f := devFixture{s: s, c: c, url: queueURL}
	listed := f.list(devCanonical, "")

	require.Len(t, listed, 1)
	require.Equal(t, "with attributes", listed[0].body)
	require.Equal(t, attrs{
		"s": {DataType: typeString, StringValue: devText},
		"b": {DataType: devBinary, BinaryValue: []byte{1, 2, 3}},
	}, listed[0].userAttrs)
	require.Equal(t, sent.str("MD5OfMessageAttributes"), listed[0].attrsMD5)
	require.Equal(t, sent.str("MD5OfMessageBody"), listed[0].bodyMD5)
	require.Equal(t, defaultAccount, listed[0].system["SenderId"])

	// The JSON form of the XML keeps a list of attributes as an array of name
	// and value, as LocalStack's does.
	got := f.get(f.withQueue(devCompat, ""), asJSON())
	message := decodeDocument[jsonMessage](t, got).Result.Message

	require.Equal(t, []map[string]any{
		{devName: "b", devValue: map[string]any{"BinaryValue": "AQID", "DataType": devBinary}},
		{devName: "s", devValue: map[string]any{"StringValue": devText, "DataType": typeString}},
	}, message.MessageAttribute)
	require.Contains(t, message.Attribute, jsonAttribute{Name: "SenderId", Value: defaultAccount})
}

func Test_DeveloperAPI_formats(t *testing.T) {
	f := newDevFixture(t, nil)
	target := f.withQueue(devCompat, "")

	t.Run("xml by default", func(t *testing.T) {
		got := f.get(target, nil)

		require.Equal(t, http.StatusOK, got.status)
		require.Equal(t, devTypeText, hdr(got, devHeaderType))
		require.NotEmpty(t, hdr(got, "x-amzn-RequestId"))
		require.Contains(t, got.body, `<ReceiveMessageResponse xmlns="http://queue.amazonaws.com/doc/2012-11-05/">`)
		require.Contains(t, got.body, "<ReceiptHandle>"+devHandle+"</ReceiptHandle>")
	})

	t.Run("application/xml", func(t *testing.T) {
		got := f.get(target, map[string]string{devHeaderAccept: devTypeXML})

		require.Equal(t, http.StatusOK, got.status)
		require.Equal(t, devTypeXML, hdr(got, devHeaderType))
		require.Contains(t, got.body, "<ReceiveMessageResponse")
	})

	t.Run("json when asked, in the shape of the xml", func(t *testing.T) {
		got := f.get(target, asJSON())

		require.Equal(t, http.StatusOK, got.status)
		require.Equal(t, devTypeAppJS, hdr(got, devHeaderType))
		require.NotContains(t, got.body, "xmlns")

		// One message is an object, as xmltodict makes of a single element.
		response := decodeDocument[jsonMessage](t, got)

		require.Equal(t, hdr(got, "x-amzn-RequestId"), response.Metadata["RequestId"])
		require.Equal(t, devVisible, response.Result.Message.Body)
		require.Equal(t, devHandle, response.Result.Message.ReceiptHandle)
		require.NotEmpty(t, response.Result.Message.Attribute)
	})

	t.Run("json lists several messages as an array", func(t *testing.T) {
		got := f.get(target+devBothFlags, asJSON())

		messages := decodeDocument[[]jsonMessage](t, got).Result.Message

		require.Len(t, messages, 3)
		require.Equal(t, devTaken, messages[0].Body)
	})

	t.Run("json of an empty queue has a null result", func(t *testing.T) {
		empty := f.c.createQueue("empty-queue", nil)
		got := f.get(devCompat+"?QueueUrl="+url.QueryEscape(empty), asJSON())

		require.Equal(t, http.StatusOK, got.status)
		require.JSONEq(t, `{"ReceiveMessageResponse":{"ReceiveMessageResult":null,"ResponseMetadata":{"RequestId":"`+
			hdr(got, "x-amzn-RequestId")+`"}}}`, got.body)
	})

	t.Run("the json protocol answers with Messages", func(t *testing.T) {
		got := f.s.send(httpCall{
			method: http.MethodPost, target: devCompat + "?ShowInvisible=true",
			headers: map[string]string{devHeaderType: devTypeProtocol, devHeaderTarget: devTargetRecv},
			body:    `{"QueueUrl":"` + f.url + `"}`,
		})

		require.Equal(t, http.StatusOK, got.status, got.body)
		require.Equal(t, devTypeProtocol, hdr(got, devHeaderType))
		require.Equal(t, []string{devTaken, devVisible}, bodies(f.s.as(protoJSON).decode(got).messages()))
	})

	t.Run("the json protocol on the path of a queue ignores an Accept header", func(t *testing.T) {
		headers := asProtocol()
		headers[devHeaderAccept] = devTypeText

		got := f.get(devCanonical+devQueuePath, headers)

		require.Equal(t, http.StatusOK, got.status, got.body)
		require.Equal(t, devTypeProtocol, hdr(got, devHeaderType))
		require.Equal(t, []string{devVisible}, bodies(f.s.as(protoJSON).decode(got).messages()))
	})

	t.Run("the json protocol lists nothing as an empty array", func(t *testing.T) {
		f.c.createQueue("empty-json-queue", nil)

		got := f.get(devCompat+"/us-east-1/"+defaultAccount+"/empty-json-queue", asProtocol())

		require.Equal(t, http.StatusOK, got.status, got.body)
		require.JSONEq(t, `{"Messages":[]}`, got.body)
	})

	t.Run("a GET that claims to be the json protocol has no operation", func(t *testing.T) {
		got := f.get(target, asProtocol())

		require.Equal(t, http.StatusBadRequest, got.status, got.body)
		require.Equal(t, "InvalidRequest;Sender", hdr(got, "x-amzn-query-error"))
	})
}

func Test_DeveloperAPI_acceptNegotiation(t *testing.T) {
	f := newDevFixture(t, nil)
	target := f.withQueue(devCanonical, "")

	tests := []struct {
		name    string
		accept  string
		content string
		want    string
	}{
		{name: "no header", want: devTypeText},
		{name: "any", accept: "*/*", want: devTypeText},
		{name: "json", accept: devTypeAppJS, want: devTypeAppJS},
		{name: "json preferred by quality", accept: "text/xml;q=0.5, application/json", want: devTypeAppJS},
		{name: "xml preferred by quality", accept: "application/json;q=0.5, application/xml", want: devTypeXML},
		{name: "text group", accept: "text/*", want: devTypeText},
		{name: "application group", accept: "application/*", want: devTypeXML},
		{name: "unsupported falls back to text/xml", accept: "image/png", want: devTypeText},
		{name: "exact beats any at equal quality", accept: "*/*, application/json", want: devTypeAppJS},
		{name: "content type of the call stands in", content: devTypeAppJS, want: devTypeAppJS},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := map[string]string{}
			if tt.accept != "" {
				headers[devHeaderAccept] = tt.accept
			}

			if tt.content != "" {
				headers[devHeaderType] = tt.content
			}

			got := f.get(target, headers)

			require.Equal(t, http.StatusOK, got.status, got.body)
			require.Equal(t, tt.want, hdr(got, devHeaderType))
		})
	}
}

// A form call has the content type of a form, which names no response type, so
// the answer is the default.
func Test_DeveloperAPI_formCallGetsXML(t *testing.T) {
	f := newDevFixture(t, nil)

	got := f.s.send(httpCall{
		method: http.MethodPost, target: devCompat, headers: map[string]string{devHeaderType: formType},
		body: "Action=ReceiveMessage&QueueUrl=" + url.QueryEscape(f.url),
	})

	require.Equal(t, http.StatusOK, got.status, got.body)
	require.Equal(t, devTypeText, hdr(got, devHeaderType))
}

func Test_DeveloperAPI_specialCharactersSurviveBothFormats(t *testing.T) {
	s := newDevStack(t, nil)
	c := s.as(protoJSON)
	queueURL := c.createQueue("special-queue", nil)
	body := "quote \" amp & lt < gt > cr \r tab \t unicode é"
	c.send(queueURL, body, nil)

	f := devFixture{s: s, c: c, url: queueURL}
	require.Equal(t, []string{body}, bodies(f.list(devCompat, "")))

	got := f.get(f.withQueue(devCompat, ""), asJSON())
	require.Equal(t, body, decodeDocument[jsonMessage](t, got).Result.Message.Body)
}

func Test_DeveloperAPI_queueIsGivenInEveryWay(t *testing.T) {
	f := newDevFixture(t, nil)
	queue := url.QueryEscape(f.url)
	noRegion := url.QueryEscape("http://localhost:4566/" + defaultAccount + "/" + devQueueName)

	tests := []struct {
		name      string
		call      httpCall
		invisible bool
		protocol  string
	}{
		{
			name: "GET query", protocol: protoQuery,
			call: httpCall{method: http.MethodGet, target: devCompat + "?QueueUrl=" + queue},
		},
		{
			name: "POST query", protocol: protoQuery,
			call: httpCall{method: http.MethodPost, target: devCompat + "?QueueUrl=" + queue},
		},
		{
			name: "POST form of a ReceiveMessage call", protocol: protoQuery, invisible: true,
			call: httpCall{
				method: http.MethodPost, target: devCompat, headers: map[string]string{devHeaderType: formType},
				body: "Action=ReceiveMessage&Version=2012-11-05&QueueUrl=" + queue + "&ShowInvisible=true",
			},
		},
		{
			name: "POST text/plain with the queue in the query", protocol: protoQuery,
			call: httpCall{
				method: http.MethodPost, target: devCompat + "?QueueUrl=" + queue,
				headers: map[string]string{devHeaderType: "text/plain"}, body: "ignored",
			},
		},
		{
			name: "POST json protocol ReceiveMessage", protocol: protoJSON, invisible: true,
			call: httpCall{
				method: http.MethodPost, target: devCompat + "?ShowInvisible=true",
				headers: map[string]string{devHeaderType: devTypeProtocol, devHeaderTarget: devTargetRecv},
				body:    `{"QueueUrl":"` + f.url + `","MaxNumberOfMessages":10}`,
			},
		},
		{
			name: "GET with the region and account in the path", protocol: protoQuery,
			call: httpCall{method: http.MethodGet, target: devCompat + devQueuePath},
		},
		{
			name: "POST with the region and account in the path", protocol: protoQuery, invisible: true,
			call: httpCall{method: http.MethodPost, target: devCanonical + devQueuePath + "?ShowInvisible=1"},
		},
		{
			name: "a queue URL without a region takes the region of the credentials", protocol: protoQuery,
			call: httpCall{
				method: http.MethodGet, target: devCompat + "?QueueUrl=" + noRegion,
				headers: map[string]string{headerAuthorization: signed("test", usEast1)},
			},
		},
		{
			name: "a flag in the form of a call", protocol: protoQuery, invisible: true,
			call: httpCall{
				method: http.MethodPost, target: devCompat + devQueuePath,
				headers: map[string]string{devHeaderType: formType}, body: "ShowInvisible=true",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := f.s.send(tt.call)

			require.Equal(t, http.StatusOK, got.status, got.body)

			want := []string{devVisible}
			if tt.invisible {
				want = []string{devTaken, devVisible}
			}

			require.Equal(t, want, bodies(f.s.as(tt.protocol).decode(got).messages()))
		})
	}
}

func Test_DeveloperAPI_errors(t *testing.T) {
	f := newDevFixture(t, nil)
	missing := url.QueryEscape(strings.Replace(f.url, devQueueName, "no-such-queue", 1))
	form := map[string]string{devHeaderType: formType}

	tests := []struct {
		name   string
		call   httpCall
		status int
		code   string
	}{
		{
			name: "no QueueUrl", call: httpCall{method: http.MethodGet, target: devCompat},
			status: http.StatusBadRequest, code: devNoQueue,
		},
		{
			name: "an empty QueueUrl", call: httpCall{method: http.MethodGet, target: devCompat + "?QueueUrl="},
			status: http.StatusBadRequest, code: devNoQueue,
		},
		{
			name:   "a queue that does not exist",
			call:   httpCall{method: http.MethodGet, target: devCompat + "?QueueUrl=" + missing},
			status: http.StatusBadRequest, code: devNoQueue,
		},
		{
			name:   "a queue of another region",
			call:   httpCall{method: http.MethodGet, target: devCompat + "/eu-west-1/" + defaultAccount + "/" + devQueueName},
			status: http.StatusBadRequest, code: devNoQueue,
		},
		{
			name:   "a queue of another account",
			call:   httpCall{method: http.MethodGet, target: devCanonical + "/us-east-1/111111111111/" + devQueueName},
			status: http.StatusBadRequest, code: devNoQueue,
		},
		{
			name:   "a QueueUrl that is not a URL",
			call:   httpCall{method: http.MethodGet, target: devCompat + "?QueueUrl=" + devQueueName},
			status: http.StatusNotFound, code: "InvalidAddress",
		},
		{
			name:   "a QueueUrl of another shape",
			call:   httpCall{method: http.MethodGet, target: devCompat + "?QueueUrl=http://a/b/c/d/e"},
			status: http.StatusNotFound, code: "InvalidAddress",
		},
		{
			name:   "another operation as a form",
			call:   httpCall{method: http.MethodPost, target: devCompat, headers: form, body: "Action=ListQueues"},
			status: http.StatusBadRequest, code: "InvalidRequest",
		},
		{
			name: "a form without an operation",
			call: httpCall{
				method: http.MethodPost, target: devCompat, headers: form, body: "QueueUrl=" + url.QueryEscape(f.url),
			},
			status: http.StatusBadRequest, code: "InvalidRequest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := f.s.send(tt.call)
			res := f.s.as(protoQuery).decode(got)

			require.Equal(t, tt.status, got.status, got.body)
			require.Equal(t, tt.code, res.errorCode())
			require.Equal(t, devTypeText, hdr(got, devHeaderType))
		})
	}

	t.Run("another operation over the json protocol", func(t *testing.T) {
		got := f.s.send(httpCall{
			method: http.MethodPost, target: devCompat,
			headers: map[string]string{devHeaderType: devTypeProtocol, devHeaderTarget: "AmazonSQS.SendMessage"},
			body:    `{"QueueUrl":"` + f.url + `","MessageBody":"x"}`,
		})
		res := f.s.as(protoJSON).decode(got)

		require.Equal(t, http.StatusBadRequest, got.status)
		require.Equal(t, "CommonServiceException", res.errorCode())
		require.Equal(t, "This endpoint only accepts ReceiveMessage calls", res.errorMessage())
		require.Equal(t, "InvalidRequest;Sender", hdr(got, "x-amzn-query-error"))
	})

	t.Run("a missing queue over the json protocol", func(t *testing.T) {
		got := f.get(devCompat+"/us-east-1/"+defaultAccount+"/no-such-queue", asProtocol())
		res := f.s.as(protoJSON).decode(got)

		require.Equal(t, http.StatusBadRequest, got.status)
		require.Equal(t, "QueueDoesNotExist", res.errorCode())
		require.Equal(t, devNoQueue+";Sender", hdr(got, "x-amzn-query-error"))
	})

	t.Run("errors take the format the caller asked for", func(t *testing.T) {
		got := f.get(devCompat, asJSON())

		require.Equal(t, http.StatusBadRequest, got.status)
		require.Equal(t, devTypeAppJS, hdr(got, devHeaderType))
		require.JSONEq(t, `{"ErrorResponse":{"Error":{"Code":"`+devNoQueue+`","Type":"Sender"},`+
			`"RequestId":"`+hdr(got, "x-amzn-RequestId")+`"}}`, got.body)
	})

	t.Run("other methods are not the developer API", func(t *testing.T) {
		got := f.s.send(httpCall{method: http.MethodPut, target: f.withQueue(devCompat, "")})

		require.NotEqual(t, http.StatusOK, got.status)
		require.NotContains(t, got.body, devHandle)
	})
}

func Test_DeveloperAPI_mounting(t *testing.T) {
	t.Run("both prefixes by default", func(t *testing.T) {
		f := newDevFixture(t, nil)

		for _, path := range devPaths {
			require.Equal(t, []string{devVisible}, bodies(f.list(path, "")), path)
			require.Equal(t, http.StatusOK, f.get(path+devQueuePath, nil).status, path)
		}
	})

	t.Run("compat paths off leaves the canonical ones", func(t *testing.T) {
		f := newDevFixture(t, func(s *sqs.Settings) { s.DisableCompatPaths = true })

		require.Equal(t, []string{devVisible}, bodies(f.list(devCanonical, "")))
		require.Equal(t, http.StatusOK, f.get(devCanonical+devQueuePath, nil).status)

		for _, target := range []string{f.withQueue(devCompat, ""), devCompat + devQueuePath} {
			got := f.get(target, nil)

			require.NotEqual(t, http.StatusOK, got.status, target)
			require.NotContains(t, got.body, devHandle, target)
		}
	})
}

// Test_DeveloperAPI_matchesLocalstackSpec checks the /_aws responses against
// the paths of LocalStack's openapi.yaml.
func Test_DeveloperAPI_matchesLocalstackSpec(t *testing.T) {
	spec := loadDevSpec(t, devLocalstackSpec)
	f := newDevFixture(t, nil)
	pathItem := devCompat + "/{region}/{account_id}/{queue_name}"

	t.Run("the schemas can fail", func(t *testing.T) {
		schema := spec.schema(t, devCompat, http.MethodGet, "responses/200")

		require.Error(t, schema.Validate(decodeJSON(t, `{"Messages": "not a list"}`)))
		require.Error(t, schema.Validate(decodeJSON(t, `{"Messages": [{"MessageId": 1}]}`)))
		require.NoError(t, schema.Validate(decodeJSON(t, `{"Messages": [{"MessageId": "a", "Attributes": {}}]}`)))
	})

	t.Run("GET /_aws/sqs/messages", func(t *testing.T) {
		got := f.get(f.withQueue(devCompat, devBothFlags), asJSON())

		require.Equal(t, http.StatusOK, got.status, got.body)
		spec.requireValid(t, devCompat, http.MethodGet, "responses/200", got.body)

		require.True(t, spec.declares(devCompat, http.MethodGet, "responses/200/content/text/xml"))
		require.Equal(t, devTypeText, hdr(f.get(f.withQueue(devCompat, ""), nil), devHeaderType))

		for _, status := range []string{"400", "404"} {
			require.True(t, spec.declares(devCompat, http.MethodGet, "responses/"+status))
		}
	})

	t.Run("POST /_aws/sqs/messages", func(t *testing.T) {
		request := `{"QueueUrl":"` + f.url + `","MaxNumberOfMessages":10,"VisibilityTimeout":0,"WaitTimeSeconds":0}`
		spec.requireValid(t, devCompat, http.MethodPost, "requestBody", request)

		got := f.s.send(httpCall{
			method: http.MethodPost, target: devCompat + "?ShowInvisible=true",
			headers: map[string]string{devHeaderType: devTypeProtocol, devHeaderTarget: devTargetRecv}, body: request,
		})

		require.Equal(t, http.StatusOK, got.status, got.body)
		spec.requireValid(t, devCompat, http.MethodPost, "responses/200", got.body)
		require.Len(t, f.s.as(protoJSON).decode(got).messages(), 2)

		form := f.s.send(httpCall{
			method: http.MethodPost, target: devCompat, headers: map[string]string{devHeaderType: formType},
			body: "Action=ReceiveMessage&QueueUrl=" + url.QueryEscape(f.url),
		})
		require.Equal(t, http.StatusOK, form.status, form.body)
		require.True(t, spec.declares(devCompat, http.MethodPost, "responses/200/content/text/xml"))
		require.True(t, spec.declares(devCompat, http.MethodPost, "requestBody/content/application/x-www-form-urlencoded"))
	})

	t.Run("GET /_aws/sqs/messages/{region}/{account_id}/{queue_name}", func(t *testing.T) {
		got := f.get(devCompat+devQueuePath+"?ShowInvisible=true&ShowDelayed=true", asProtocol())

		require.Equal(t, http.StatusOK, got.status, got.body)
		spec.requireValid(t, pathItem, http.MethodGet, "responses/200", got.body)
		require.Len(t, f.s.as(protoJSON).decode(got).messages(), 3)

		for _, status := range []string{"400", "404"} {
			require.True(t, spec.declares(pathItem, http.MethodGet, "responses/"+status))
		}
	})
}

// Test_DeveloperAPI_canonicalRoutesAreDocumented keeps api/openapi/gostack.yaml
// in step with the routes the SQS provider mounts under /_gostack/services.
func Test_DeveloperAPI_canonicalRoutesAreDocumented(t *testing.T) {
	spec := loadDevSpecBytes(t, openapi.Spec)

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, path := range []string{devCanonical, devCanonical + "/{region}/{account_id}/{queue_name}"} {
			require.True(t, spec.declares(path, method, "responses/200"), "%s %s", method, path)
			require.True(t, spec.declares(path, method, "responses/400"), "%s %s", method, path)
			require.True(t, spec.declares(path, method, "parameters"), "%s %s", method, path)
		}
	}

	// The documented flags are the ones the handler reads.
	document := string(openapi.Spec)
	for _, name := range []string{"QueueUrl", "ShowInvisible", "ShowDelayed"} {
		require.Contains(t, document, "name: "+name)
	}
}

// devSpec is an OpenAPI 3.1 document whose schemas can validate a body.
type devSpec struct {
	compiler *jsonschema.Compiler
	paths    map[string]map[string]any
}

func loadDevSpec(t *testing.T, path string) *devSpec {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	return loadDevSpecBytes(t, data)
}

func loadDevSpecBytes(t *testing.T, data []byte) *devSpec {
	t.Helper()

	var document map[string]any
	require.NoError(t, yaml.Unmarshal(data, &document))

	asJSONText, err := json.Marshal(document)
	require.NoError(t, err)

	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(asJSONText))
	require.NoError(t, err)

	compiler := jsonschema.NewCompiler()
	require.NoError(t, compiler.AddResource(devSpecResource, parsed))

	paths := map[string]map[string]any{}

	rawPaths, _ := document["paths"].(map[string]any)
	for path, item := range rawPaths {
		operations, _ := item.(map[string]any)
		paths[path] = operations
	}

	return &devSpec{compiler: compiler, paths: paths}
}

func pointerToken(token string) string {
	token = strings.ReplaceAll(token, "~", "~0")
	token = strings.ReplaceAll(token, "/", "~1")

	return url.PathEscape(token)
}

// schema compiles the schema of the json response or request body of an
// operation. section is "responses/<status>" or "requestBody".
func (s *devSpec) schema(t *testing.T, path, method, section string) *jsonschema.Schema {
	t.Helper()

	pointer := devSpecResource + "#/paths/" + pointerToken(path) + "/" + strings.ToLower(method) + "/" + section +
		"/content/" + pointerToken(devTypeAppJS) + "/schema"

	schema, err := s.compiler.Compile(pointer)
	require.NoError(t, err, "no schema at %s", pointer)

	return schema
}

// requireValid checks that body is valid for the operation's json schema.
func (s *devSpec) requireValid(t *testing.T, path, method, section, body string) {
	t.Helper()

	require.NoError(t, s.schema(t, path, method, section).Validate(decodeJSON(t, body)), body)
}

// declares reports whether the operation has something at section, a "/"
// separated path into it. A media type contains a "/" of its own, so the path
// is matched against the keys one by one.
func (s *devSpec) declares(path, method, section string) bool {
	node := s.paths[path][strings.ToLower(method)]

	for section != "" {
		object, ok := node.(map[string]any)
		if !ok {
			return false
		}

		matched := false

		for key, value := range object {
			rest, found := strings.CutPrefix(section, key)
			if !found || (rest != "" && !strings.HasPrefix(rest, "/")) {
				continue
			}

			node, section, matched = value, strings.TrimPrefix(rest, "/"), true

			break
		}

		if !matched {
			return false
		}
	}

	return node != nil
}

func decodeJSON(t *testing.T, body string) any {
	t.Helper()

	instance, err := jsonschema.UnmarshalJSON(strings.NewReader(body))
	require.NoError(t, err, body)

	return instance
}
