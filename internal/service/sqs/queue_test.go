package sqs_test

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const standardURLPrefix = "http://sqs.us-east-1.localhost.localstack.cloud:4566/" + defaultAccount + "/"

func Test_Queue_lifecycle(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			s := newStack(t)
			c := s.as(protocol)

			url := c.createQueue(labelFirst, nil)
			assert.Equal(t, standardURLPrefix+labelFirst, url)

			t.Run("creating it again returns the same URL", func(t *testing.T) {
				assert.Equal(t, url, c.createQueue(labelFirst, nil))
			})

			t.Run("creating it with attributes it already has succeeds", func(t *testing.T) {
				assert.Equal(t, url, c.createQueue(labelFirst, map[string]string{attrVisibilityTimeout: "30"}))
			})

			t.Run("GetQueueUrl finds it", func(t *testing.T) {
				res := c.call("GetQueueUrl", map[string]any{paramQueueName: labelFirst})

				require.True(t, res.ok())
				assert.Equal(t, url, res.str(paramQueueURL))
			})

			t.Run("DeleteQueue removes it", func(t *testing.T) {
				require.True(t, c.call("DeleteQueue", map[string]any{paramQueueURL: url}).ok())

				res := c.call("GetQueueUrl", map[string]any{paramQueueName: labelFirst})

				assert.Equal(t, http.StatusBadRequest, res.status)
				assert.Equal(t, nonExistentCode(protocol), res.errorCode())
			})
		})
	}
}

// nonExistentCode is the code QueueDoesNotExist has on the wire.
func nonExistentCode(protocol string) string {
	if protocol == protoQuery {
		return "AWS.SimpleQueueService.NonExistentQueue"
	}

	return "QueueDoesNotExist"
}

func Test_Queue_create_with_other_attributes(t *testing.T) {
	tests := []struct {
		protocol string
		wantCode string
	}{
		{protocol: protoJSON, wantCode: "QueueNameExists"},
		{protocol: protoQuery, wantCode: "QueueAlreadyExists"},
	}

	for _, tt := range tests {
		t.Run(tt.protocol, func(t *testing.T) {
			c := newStack(t).as(tt.protocol)
			c.createQueue("q", map[string]string{attrDelaySeconds: "5"})

			res := c.call("CreateQueue", map[string]any{
				paramQueueName:  "q",
				paramAttributes: map[string]string{attrDelaySeconds: "6"},
			})

			assert.Equal(t, http.StatusBadRequest, res.status)
			assert.Equal(t, tt.wantCode, res.errorCode())
			assert.Equal(t,
				"A queue already exists with the same name and a different value for attribute DelaySeconds",
				res.errorMessage())
		})
	}
}

func Test_Queue_GetQueueUrl_missing(t *testing.T) {
	tests := []struct {
		protocol    string
		wantMessage string
	}{
		{protocol: protoJSON, wantMessage: "The specified queue does not exist."},
		{protocol: protoQuery, wantMessage: "The specified queue does not exist for this wsdl version."},
	}

	for _, tt := range tests {
		t.Run(tt.protocol, func(t *testing.T) {
			res := newStack(t).as(tt.protocol).call("GetQueueUrl", map[string]any{paramQueueName: "nothing"})

			assert.Equal(t, http.StatusBadRequest, res.status)
			assert.Equal(t, nonExistentCode(tt.protocol), res.errorCode())
			assert.Equal(t, tt.wantMessage, res.errorMessage())
		})
	}
}

func Test_Queue_names(t *testing.T) {
	tests := []struct {
		name      string
		giveName  string
		giveAttrs map[string]string
		wantOK    bool
		wantMsg   string
	}{
		{name: "standard", giveName: "queue_1-a", wantOK: true},
		{name: "empty", giveName: "", wantMsg: "Can only include alphanumeric characters, hyphens, or " +
			"underscores. 1 to 80 in length"},
		{name: "dot in a standard name", giveName: "queue.fifo", wantMsg: "Can only include alphanumeric " +
			"characters, hyphens, or underscores. 1 to 80 in length"},
		{name: "space", giveName: "a b", wantMsg: "Can only include alphanumeric characters, hyphens, or " +
			"underscores. 1 to 80 in length"},
		{name: "81 characters", giveName: strings.Repeat("a", 81), wantMsg: "Can only include alphanumeric characters, " +
			"hyphens, or underscores. 1 to 80 in length"},
		{name: "fifo", giveName: "queue.fifo", giveAttrs: map[string]string{attrFifoQueue: valueTrue}, wantOK: true},
		{
			name: "fifo without the suffix", giveName: "queue", giveAttrs: map[string]string{attrFifoQueue: valueTrue},
			wantMsg: "The name of a FIFO queue can only include alphanumeric characters, hyphens, or underscores, " +
				"must end with .fifo suffix and be 1 to 80 in length",
		},
		{
			name: "fifo suffix counts towards the limit", giveName: strings.Repeat("a", 76) + ".fifo",
			giveAttrs: map[string]string{attrFifoQueue: valueTrue},
			wantMsg:   "Can only include alphanumeric characters, hyphens, or underscores. 1 to 80 in length",
		},
	}

	for _, protocol := range protocols {
		c := newStack(t).as(protocol)

		for _, tt := range tests {
			t.Run(protocol+"/"+tt.name, func(t *testing.T) {
				params := map[string]any{paramQueueName: tt.giveName}
				if tt.giveAttrs != nil {
					params[paramAttributes] = tt.giveAttrs
				}

				res := c.call("CreateQueue", params)

				if tt.wantOK {
					assert.True(t, res.ok())

					return
				}

				assert.Equal(t, http.StatusBadRequest, res.status)
				assert.Equal(t, invalidParameterCode(protocol), res.errorCode())
				assert.Equal(t, tt.wantMsg, res.errorMessage())
			})
		}
	}
}

// invalidParameterCode is the code InvalidParameterValueException has on the
// wire.
func invalidParameterCode(protocol string) string {
	if protocol == protoQuery {
		return "InvalidParameterValue"
	}

	return "InvalidParameterValueException"
}

func Test_Queue_ListQueues(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)

			names := []string{"zeta", "alpha", "alpine", "beta"}
			for _, name := range names {
				c.createQueue(name, nil)
			}

			t.Run("lists in creation order", func(t *testing.T) {
				res := c.call("ListQueues", nil)

				require.True(t, res.ok())
				assert.Equal(t, urlsOf(names...), res.urls())
				assert.Empty(t, res.str("NextToken"))
			})

			t.Run("filters by prefix", func(t *testing.T) {
				res := c.call("ListQueues", map[string]any{"QueueNamePrefix": "al"})

				assert.Equal(t, urlsOf("alpha", "alpine"), res.urls())
			})

			t.Run("an empty result has no URLs", func(t *testing.T) {
				res := c.call("ListQueues", map[string]any{"QueueNamePrefix": "nothing"})

				require.True(t, res.ok())
				assert.Empty(t, res.urls())
			})

			t.Run("pages carry the URL of the next page's first queue", func(t *testing.T) {
				first := c.call("ListQueues", map[string]any{paramMaxResults: 3})

				require.True(t, first.ok())
				assert.Equal(t, urlsOf("zeta", "alpha", "alpine"), first.urls())
				assert.Equal(t, base64.StdEncoding.EncodeToString([]byte(standardURLPrefix+"beta")),
					first.str("NextToken"))

				second := c.call("ListQueues", map[string]any{paramMaxResults: 3, "NextToken": first.str("NextToken")})

				require.True(t, second.ok())
				assert.Equal(t, urlsOf("beta"), second.urls())
				assert.Empty(t, second.str("NextToken"))
			})
		})
	}
}

func urlsOf(names ...string) []string {
	urls := make([]string, 0, len(names))
	for _, name := range names {
		urls = append(urls, standardURLPrefix+name)
	}

	return urls
}

func Test_Queue_accounts_and_regions_are_separate(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			s := newStack(t)
			main := s.as(protocol)
			other := main.withKey("000000000002")
			abroad := main.inRegion(euWest1)

			main.createQueue("shared", nil)
			otherURL := other.createQueue("shared", nil)
			abroad.createQueue("shared", nil)

			assert.Equal(t, "http://sqs.us-east-1.localhost.localstack.cloud:4566/000000000002/shared", otherURL)
			assert.Len(t, main.call("ListQueues", nil).urls(), 1)
			assert.Len(t, other.call("ListQueues", nil).urls(), 1)

			t.Run("QueueOwnerAWSAccountId reaches another account", func(t *testing.T) {
				res := main.call("GetQueueUrl", map[string]any{
					paramQueueName: "shared", "QueueOwnerAWSAccountId": "000000000002",
				})

				require.True(t, res.ok())
				assert.Equal(t, otherURL, res.str(paramQueueURL))
			})

			t.Run("a queue of another region is not found", func(t *testing.T) {
				res := main.call("GetQueueUrl", map[string]any{paramQueueName: "other-region-only"})
				require.False(t, res.ok())

				abroad.createQueue("other-region-only", nil)
				res = main.call("GetQueueUrl", map[string]any{paramQueueName: "other-region-only"})

				assert.False(t, res.ok())
			})

			t.Run("a queue URL names its region", func(t *testing.T) {
				url := abroad.createQueue("regional", nil)
				assert.Equal(t, "http://sqs.eu-west-1.localhost.localstack.cloud:4566/"+defaultAccount+"/regional", url)

				// Called from us-east-1, the URL still reaches the eu-west-1 queue.
				res := main.call(opGetQueueAttributes, map[string]any{
					paramQueueURL: url, paramAttributeNames: []string{attrQueueArn},
				})

				require.True(t, res.ok())
				assert.Equal(t, "arn:aws:sqs:eu-west-1:"+defaultAccount+":regional", res.attributes()[attrQueueArn])
			})
		})
	}
}

func Test_Queue_QueueUrl_as_a_name(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			c.createQueue("by-name", nil)

			res := c.call(opGetQueueAttributes, map[string]any{
				paramQueueURL: "by-name", paramAttributeNames: []string{attrQueueArn},
			})

			require.True(t, res.ok())
			assert.Equal(t, "arn:aws:sqs:us-east-1:"+defaultAccount+":by-name", res.attributes()[attrQueueArn])
		})
	}
}

func Test_Queue_many_creations_keep_the_order(t *testing.T) {
	c := newStack(t).as(protoJSON)

	const count = 25

	names := make([]string, 0, count)

	for i := range count {
		name := "q" + strconv.Itoa(count-i)
		names = append(names, name)
		c.createQueue(name, nil)
	}

	assert.Equal(t, urlsOf(names...), c.call("ListQueues", nil).urls())
}
