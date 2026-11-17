package sqs_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_URL_strategies(t *testing.T) {
	tests := []struct {
		name         string
		giveStrategy string
		giveRegion   string
		giveHost     string
		giveTLS      bool
		want         string
	}{
		{
			name: "standard", giveStrategy: sqs.StrategyStandard, giveRegion: euWest1,
			want: "http://sqs.eu-west-1.localhost.localstack.cloud:4566/000000000000/q",
		},
		{
			name: "standard takes the scheme of the request", giveStrategy: sqs.StrategyStandard, giveRegion: usEast1,
			giveTLS: true, want: "https://sqs.us-east-1.localhost.localstack.cloud:4566/000000000000/q",
		},
		{
			name: "domain leaves out us-east-1", giveStrategy: sqs.StrategyDomain, giveRegion: usEast1,
			want: "http://queue.localhost.localstack.cloud:4566/000000000000/q",
		},
		{
			name: "domain names another region", giveStrategy: sqs.StrategyDomain, giveRegion: euWest1,
			want: "http://eu-west-1.queue.localhost.localstack.cloud:4566/000000000000/q",
		},
		{
			name: "path", giveStrategy: sqs.StrategyPath, giveRegion: euWest1,
			want: "http://localhost.localstack.cloud:4566/queue/eu-west-1/000000000000/q",
		},
		{
			name: "off", giveStrategy: sqs.StrategyOff, giveRegion: euWest1,
			want: "http://localhost.localstack.cloud:4566/000000000000/q",
		},
		{
			name: "an unknown strategy is off", giveStrategy: "bogus", giveRegion: euWest1,
			want: "http://localhost.localstack.cloud:4566/000000000000/q",
		},
		{
			name: "dynamic from a standard host", giveStrategy: sqs.StrategyDynamic, giveRegion: euWest1,
			giveHost: "sqs.eu-west-1.example.com:8080", want: "http://sqs.eu-west-1.example.com:8080/000000000000/q",
		},
		{
			name: "dynamic from a domain host", giveStrategy: sqs.StrategyDynamic, giveRegion: euWest1,
			giveHost: "eu-west-1.queue.example.com:8080", want: "http://eu-west-1.queue.example.com:8080/000000000000/q",
		},
		{
			name: "dynamic from a queue host", giveStrategy: sqs.StrategyDynamic, giveRegion: usEast1,
			giveHost: "queue.example.com", want: "http://queue.example.com/000000000000/q",
		},
		{
			name: "dynamic from any other host is the path strategy", giveStrategy: sqs.StrategyDynamic,
			giveRegion: euWest1, giveHost: hostLocalstack, want: "http://localhost:4566/queue/eu-west-1/000000000000/q",
		},
		{
			name: "dynamic takes the scheme of the request", giveStrategy: sqs.StrategyDynamic, giveRegion: usEast1,
			giveHost: hostLocalstack, giveTLS: true, want: "https://localhost:4566/queue/us-east-1/000000000000/q",
		},
	}

	for _, protocol := range protocols {
		for _, tt := range tests {
			t.Run(protocol+"/"+tt.name, func(t *testing.T) {
				s := newStack(t)
				s.settings.update(func(settings *sqs.Settings) { settings.EndpointStrategy = tt.giveStrategy })

				c := s.as(protocol).inRegion(tt.giveRegion).onHost(tt.giveHost)
				if tt.giveTLS {
					c = c.overTLS()
				}

				created := c.createQueue("q", nil)
				assert.Equal(t, tt.want, created)

				fetched := c.call("GetQueueUrl", map[string]any{paramQueueName: "q"})
				require.True(t, fetched.ok())
				assert.Equal(t, tt.want, fetched.str(paramQueueURL))

				listed := c.call("ListQueues", nil)
				assert.Equal(t, []string{tt.want}, listed.urls())
			})
		}
	}
}

func Test_URL_follows_a_settings_change(t *testing.T) {
	s := newStack(t)
	c := s.as(protoJSON).inRegion(euWest1)

	assert.Equal(t, "http://sqs.eu-west-1.localhost.localstack.cloud:4566/000000000000/q", c.createQueue("q", nil))

	s.settings.update(func(settings *sqs.Settings) {
		settings.EndpointStrategy = sqs.StrategyPath
		settings.Host = "gostack.test:9000"
	})

	res := c.call("GetQueueUrl", map[string]any{paramQueueName: "q"})

	assert.Equal(t, "http://gostack.test:9000/queue/eu-west-1/000000000000/q", res.str(paramQueueURL))
}

// Every URL shape reaches the queue it names, whatever SQS_ENDPOINT_STRATEGY
// says.
func Test_URL_routes(t *testing.T) {
	s := newStack(t)
	json := s.as(protoJSON)
	json.createQueue("plain", nil)
	json.inRegion(euWest1).createQueue("plain", map[string]string{attrVisibilityTimeout: "77"})

	arnForm := url.Values{paramAction: {opGetQueueAttributes}, formAttributeName1: {attrAll}}

	tests := []struct {
		name        string
		giveMethod  string
		giveTarget  string
		giveHost    string
		giveRegion  string
		wantTimeout string
	}{
		{
			name: "standard host, GET", giveMethod: http.MethodGet,
			giveTarget: "/000000000000/plain?" + arnForm.Encode(),
			giveHost:   hostStandard, giveRegion: usEast1, wantTimeout: "30",
		},
		{
			name: "standard host in another region, POST", giveMethod: http.MethodPost,
			giveTarget: pathPlain, giveHost: "sqs.eu-west-1.localhost.localstack.cloud:4566",
			giveRegion: usEast1, wantTimeout: "77",
		},
		{
			name: "domain host", giveMethod: http.MethodPost, giveTarget: pathPlain,
			giveHost: "queue.localhost.localstack.cloud:4566", giveRegion: euWest1, wantTimeout: "30",
		},
		{
			name: "domain host with a region", giveMethod: http.MethodGet,
			giveTarget: "/000000000000/plain?" + arnForm.Encode(),
			giveHost:   "eu-west-1.queue.localhost.localstack.cloud:4566", giveRegion: usEast1, wantTimeout: "77",
		},
		{
			name: "path", giveMethod: http.MethodPost, giveTarget: "/queue/eu-west-1/000000000000/plain",
			giveHost: hostLocalstack, giveRegion: usEast1, wantTimeout: "77",
		},
		{
			name: "path, GET", giveMethod: http.MethodGet,
			giveTarget: "/queue/us-east-1/000000000000/plain?" + arnForm.Encode(),
			giveHost:   hostLocalstack, giveRegion: euWest1, wantTimeout: "30",
		},
		{
			name: "no region in the URL takes the region of the credentials", giveMethod: http.MethodPost,
			giveTarget: pathPlain, giveHost: hostLocalstack, giveRegion: euWest1, wantTimeout: "77",
		},
		{
			name: "a signed URL names its region in X-Amz-Credential", giveMethod: http.MethodGet,
			giveTarget: "/000000000000/plain?" + arnForm.Encode() +
				"&X-Amz-Credential=test%2F20130524%2Feu-west-1%2Fsqs%2Faws4_request",
			giveHost: hostLocalstack, giveRegion: usEast1, wantTimeout: "77",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := ""
			headers := map[string]string{headerAuthorization: signed("test", tt.giveRegion)}

			if tt.giveMethod == http.MethodPost {
				body = arnForm.Encode()
				headers["Content-Type"] = formType
			}

			res := s.send(httpCall{
				method: tt.giveMethod, target: tt.giveTarget, host: tt.giveHost, body: body, headers: headers,
			})

			require.Equal(t, http.StatusOK, res.status, res.body)
			assert.Contains(t, res.body, "<GetQueueAttributesResponse")
			assert.Contains(t, res.body, "<Name>VisibilityTimeout</Name><Value>"+tt.wantTimeout+"</Value>")
		})
	}
}

func Test_URL_route_errors(t *testing.T) {
	s := newStack(t)
	s.as(protoJSON).createQueue("plain", nil)

	send := func(method, target string, form url.Values) reply {
		headers := map[string]string{headerAuthorization: signed("test", usEast1)}

		body := ""
		if method == http.MethodPost {
			body = form.Encode()
			headers["Content-Type"] = formType
		} else if len(form) > 0 {
			target += "?" + form.Encode()
		}

		return s.send(httpCall{
			method: method, target: target, host: hostStandard,
			body: body, headers: headers,
		})
	}

	const queuePath = pathPlain

	t.Run("no Action is an unknown operation", func(t *testing.T) {
		res := send(http.MethodGet, queuePath, nil)

		assert.Equal(t, http.StatusNotFound, res.status)
		assert.Contains(t, res.body, "<UnknownOperationException")
	})

	for _, action := range []string{"CreateQueue", "ListQueues", "FooBar"} {
		t.Run(action+" is not valid for a queue URL", func(t *testing.T) {
			res := send(http.MethodGet, queuePath, url.Values{paramAction: {action}, paramVersion: {"2012-11-05"}})

			assert.Equal(t, http.StatusBadRequest, res.status)
			assert.Contains(t, res.body, "<Code>InvalidAction</Code>")
			assert.Contains(t, res.body, "<Type>Sender</Type>")
			assert.Contains(t, res.body, "<Message>The action "+action+" is not valid for this endpoint.</Message>")
		})
	}

	t.Run("a missing member is named", func(t *testing.T) {
		res := send(http.MethodGet, queuePath, url.Values{paramAction: {opSendMessage}})

		assert.Equal(t, http.StatusBadRequest, res.status)
		assert.Contains(t, res.body, "<Code>MissingParameter</Code>")
		assert.Contains(t, res.body, "<Message>The request must contain the parameter MessageBody.</Message>")
	})

	t.Run("a queue that does not exist", func(t *testing.T) {
		res := send(http.MethodGet, "/000000000000/ghost", url.Values{paramAction: {opGetQueueAttributes}})

		assert.Equal(t, http.StatusBadRequest, res.status)
		assert.Contains(t, res.body, "<Code>AWS.SimpleQueueService.NonExistentQueue</Code>")
		assert.Contains(t, res.body, "does not exist for this wsdl version")
	})

	t.Run("a QueueUrl in the form wins over the URL", func(t *testing.T) {
		s.as(protoJSON).createQueue("other", map[string]string{attrVisibilityTimeout: "12"})

		res := send(http.MethodPost, queuePath, url.Values{
			paramAction: {opGetQueueAttributes}, formAttributeName1: {attrVisibilityTimeout},
			paramQueueURL: {standardURLPrefix + "other"},
		})

		require.Equal(t, http.StatusOK, res.status)
		assert.Contains(t, res.body, "<Value>12</Value>")
	})

	t.Run("a fifo queue name routes", func(t *testing.T) {
		s.as(protoJSON).createQueue("ordered.fifo", map[string]string{attrFifoQueue: valueTrue})

		res := send(http.MethodGet, "/000000000000/ordered.fifo",
			url.Values{paramAction: {opGetQueueAttributes}, formAttributeName1: {attrFifoQueue}})

		require.Equal(t, http.StatusOK, res.status)
		assert.Contains(t, res.body, "<Name>FifoQueue</Name><Value>true</Value>")
	})

	t.Run("the json protocol is served on the queue URL", func(t *testing.T) {
		res := s.send(httpCall{
			target: queuePath, host: hostStandard,
			body: `{"QueueUrl":"` + standardURLPrefix + `plain","AttributeNames":["VisibilityTimeout"]}`,
			headers: map[string]string{
				headerAuthorization: signed("test", usEast1), "Content-Type": jsonType,
				"X-Amz-Target": "AmazonSQS.GetQueueAttributes",
			},
		})

		require.Equal(t, http.StatusOK, res.status, res.body)
		assert.JSONEq(t, `{"Attributes":{"VisibilityTimeout":"30"}}`, res.body)
	})

	t.Run("paths that are not queue URLs are not taken", func(t *testing.T) {
		for _, target := range []string{"/12345/plain", "/000000000000/plain/extra", "/000000000000/pl%20ain"} {
			res := send(http.MethodPost, target, url.Values{paramAction: {opGetQueueAttributes}})

			assert.NotContains(t, res.body, "UnknownOperationException", target)
			assert.NotEqual(t, http.StatusOK, res.status, target)
		}
	})
}
