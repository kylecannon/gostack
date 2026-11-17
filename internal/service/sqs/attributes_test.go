package sqs_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func attributeCode(protocol, name string) string {
	switch name {
	case codeInvalidAttribute, "InvalidAttributeValue":
		return name
	default:
		return invalidParameterCode(protocol)
	}
}

func Test_Attributes_defaults(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			url := c.createQueue("plain", nil)

			res := c.call(opGetQueueAttributes, map[string]any{paramQueueURL: url, paramAttributeNames: []string{attrAll}})

			require.True(t, res.ok())

			attrs := res.attributes()
			assert.Equal(t, map[string]string{
				attrVisibilityTimeout:                   "30",
				attrDelaySeconds:                        "0",
				"MaximumMessageSize":                    "1048576",
				"MessageRetentionPeriod":                "345600",
				"ReceiveMessageWaitTimeSeconds":         "0",
				"SqsManagedSseEnabled":                  valueTrue,
				attrQueueArn:                            "arn:aws:sqs:us-east-1:" + defaultAccount + ":plain",
				"ApproximateNumberOfMessages":           "0",
				"ApproximateNumberOfMessagesDelayed":    "0",
				"ApproximateNumberOfMessagesNotVisible": "0",
				"CreatedTimestamp":                      attrs["CreatedTimestamp"],
				"LastModifiedTimestamp":                 attrs["LastModifiedTimestamp"],
			}, attrs)
			assert.Regexp(t, `^\d{10}$`, attrs["CreatedTimestamp"])
		})
	}
}

func Test_Attributes_fifo_defaults(t *testing.T) {
	c := newStack(t).as(protoJSON)
	url := c.createQueue("ordered.fifo", map[string]string{attrFifoQueue: valueTrue})

	attrs := c.call(opGetQueueAttributes, map[string]any{paramQueueURL: url, paramAttributeNames: []string{attrAll}}).
		attributes()

	assert.Equal(t, valueTrue, attrs[attrFifoQueue])
	assert.Equal(t, "false", attrs["ContentBasedDeduplication"])
	assert.Equal(t, "queue", attrs["DeduplicationScope"])
	assert.Equal(t, "perQueue", attrs["FifoThroughputLimit"])
}

func Test_Attributes_get(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			url := c.createQueue("q", map[string]string{attrVisibilityTimeout: "45"})

			t.Run("named attributes only", func(t *testing.T) {
				res := c.call(opGetQueueAttributes, map[string]any{
					paramQueueURL: url, paramAttributeNames: []string{attrVisibilityTimeout, attrDelaySeconds},
				})

				require.True(t, res.ok())
				assert.Equal(t, map[string]string{attrVisibilityTimeout: "45", attrDelaySeconds: "0"}, res.attributes())
			})

			t.Run("an attribute the queue does not have is left out", func(t *testing.T) {
				res := c.call(opGetQueueAttributes, map[string]any{
					paramQueueURL: url, paramAttributeNames: []string{attrRedrivePolicy},
				})

				require.True(t, res.ok())
				assert.Empty(t, res.attributes())
			})

			t.Run("no names return none", func(t *testing.T) {
				res := c.call(opGetQueueAttributes, map[string]any{paramQueueURL: url})

				require.True(t, res.ok())
				assert.Empty(t, res.attributes())
			})

			t.Run("an unknown name is refused", func(t *testing.T) {
				res := c.call(opGetQueueAttributes, map[string]any{
					paramQueueURL: url, paramAttributeNames: []string{unknownAttribute},
				})

				assert.Equal(t, http.StatusBadRequest, res.status)
				assert.Equal(t, codeInvalidAttribute, res.errorCode())
				assert.Equal(t, "Unknown Attribute Bogus.", res.errorMessage())
			})
		})
	}
}

func Test_Attributes_create_validation(t *testing.T) {
	tests := []struct {
		name     string
		giveName string
		giveAttr map[string]string
		wantCode string
		wantMsg  string
	}{
		{
			name: "FifoQueue false on a standard queue", giveName: "a", giveAttr: map[string]string{attrFifoQueue: "false"},
			wantCode: codeInvalidAttribute, wantMsg: "Unknown Attribute FifoQueue.",
		},
		{
			name: "ContentBasedDeduplication on a standard queue", giveName: "b",
			giveAttr: map[string]string{"ContentBasedDeduplication": valueTrue},
			wantCode: codeInvalidAttribute, wantMsg: "Unknown Attribute ContentBasedDeduplication.",
		},
		{
			name: "DeduplicationScope on a standard queue", giveName: "c",
			giveAttr: map[string]string{"DeduplicationScope": "queue"},
			wantCode: codeInvalidAttribute,
			wantMsg:  "You can specify the DeduplicationScope only when FifoQueue is set to true.",
		},
		{
			name: "FifoThroughputLimit on a standard queue", giveName: "d",
			giveAttr: map[string]string{"FifoThroughputLimit": "perQueue"},
			wantCode: codeInvalidAttribute,
			wantMsg:  "You can specify the FifoThroughputLimit only when FifoQueue is set to true.",
		},
		{
			name: "a computed attribute", giveName: "e", giveAttr: map[string]string{attrQueueArn: "x"},
			wantCode: codeInvalidAttribute, wantMsg: "Unknown Attribute QueueArn.",
		},
		{
			name: "an unknown attribute", giveName: "f", giveAttr: map[string]string{unknownAttribute: "1"},
			wantCode: codeInvalidAttribute, wantMsg: "Unknown Attribute Bogus.",
		},
		{
			name: "an unknown attribute on a fifo queue", giveName: "g.fifo",
			giveAttr: map[string]string{attrFifoQueue: valueTrue, unknownAttribute: "1"},
			wantCode: codeInvalidAttribute, wantMsg: "Unknown Attribute Bogus.",
		},
	}

	for _, protocol := range protocols {
		c := newStack(t).as(protocol)

		for _, tt := range tests {
			t.Run(protocol+"/"+tt.name, func(t *testing.T) {
				res := c.call("CreateQueue", map[string]any{paramQueueName: tt.giveName, paramAttributes: tt.giveAttr})

				assert.Equal(t, http.StatusBadRequest, res.status)
				assert.Equal(t, attributeCode(protocol, tt.wantCode), res.errorCode())
				assert.Equal(t, tt.wantMsg, res.errorMessage())
			})
		}
	}
}

func Test_Attributes_set(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			url := c.createQueue("q", nil)
			fifoURL := c.createQueue("q.fifo", map[string]string{attrFifoQueue: valueTrue})

			get := func(queueURL, name string) (string, bool) {
				res := c.call(opGetQueueAttributes, map[string]any{
					paramQueueURL: queueURL, paramAttributeNames: []string{name},
				})
				require.True(t, res.ok())

				value, ok := res.attributes()[name]

				return value, ok
			}
			set := func(queueURL string, attrs map[string]string) result {
				return c.call("SetQueueAttributes", map[string]any{paramQueueURL: queueURL, paramAttributes: attrs})
			}

			t.Run("a value changes", func(t *testing.T) {
				require.True(t, set(url, map[string]string{attrVisibilityTimeout: "99"}).ok())

				value, _ := get(url, attrVisibilityTimeout)
				assert.Equal(t, "99", value)
			})

			t.Run("an empty Policy removes it", func(t *testing.T) {
				require.True(t, set(url, map[string]string{attrPolicy: `{"Version":"2012-10-17"}`}).ok())

				value, ok := get(url, attrPolicy)
				require.True(t, ok)
				assert.JSONEq(t, `{"Version":"2012-10-17"}`, value)

				require.True(t, set(url, map[string]string{attrPolicy: ""}).ok())

				_, ok = get(url, attrPolicy)
				assert.False(t, ok)
			})

			t.Run("the default of a Kms attribute removes it", func(t *testing.T) {
				require.True(t, set(url, map[string]string{"KmsMasterKeyId": "alias/x", "KmsDataKeyReusePeriodSeconds": "60"}).ok())

				value, _ := get(url, "KmsMasterKeyId")
				assert.Equal(t, "alias/x", value)

				require.True(t, set(url, map[string]string{"KmsMasterKeyId": "", "KmsDataKeyReusePeriodSeconds": "300"}).ok())

				_, ok := get(url, "KmsMasterKeyId")
				assert.False(t, ok)

				_, ok = get(url, "KmsDataKeyReusePeriodSeconds")
				assert.False(t, ok)
			})

			t.Run("a computed attribute is refused", func(t *testing.T) {
				res := set(url, map[string]string{attrQueueArn: "x"})

				assert.Equal(t, codeInvalidAttribute, res.errorCode())
				assert.Equal(t, "Unknown Attribute QueueArn.", res.errorMessage())
			})

			t.Run("FifoQueue cannot be set on a standard queue", func(t *testing.T) {
				res := set(url, map[string]string{attrFifoQueue: valueTrue})

				assert.Equal(t, codeInvalidAttribute, res.errorCode())
				assert.Equal(t, "Unknown Attribute FifoQueue.", res.errorMessage())
			})

			t.Run("a fifo queue cannot become standard", func(t *testing.T) {
				res := set(fifoURL, map[string]string{attrFifoQueue: "false"})

				assert.Equal(t, "InvalidAttributeValue", res.errorCode())
				assert.Equal(t,
					"Invalid value for the parameter FifoQueue. Reason: Modifying queue type is not supported.",
					res.errorMessage())
			})

			t.Run("setting none changes nothing", func(t *testing.T) {
				assert.True(t, c.call("SetQueueAttributes", map[string]any{paramQueueURL: url}).ok())
			})
		})
	}
}

func Test_Attributes_RedrivePolicy(t *testing.T) {
	const target = "arn:aws:sqs:us-east-1:000000000000:dead"

	valid := `{"deadLetterTargetArn":"` + target + `","maxReceiveCount":5}`
	missingTarget := `{"maxReceiveCount":5}`
	missingCount := `{"deadLetterTargetArn":"` + target + `"}`
	zero := `{"deadLetterTargetArn":"` + target + `","maxReceiveCount":0}`
	huge := `{"deadLetterTargetArn":"` + target + `","maxReceiveCount":1001}`
	word := `{"deadLetterTargetArn":"` + target + `","maxReceiveCount":"many"}`
	stringCount := `{"deadLetterTargetArn":"` + target + `","maxReceiveCount":"42"}`

	const rangeText = ", valid values are from 1 to 1000 both inclusive."

	tests := []struct {
		name    string
		give    string
		wantMsg string
	}{
		{name: "valid", give: valid},
		{name: "a count that is a string", give: stringCount},
		{name: "no target", give: missingTarget, wantMsg: "The required parameter 'deadLetterTargetArn' is missing"},
		{name: "no count", give: missingCount, wantMsg: "The required parameter 'maxReceiveCount' is missing"},
		{
			name: "count zero", give: zero,
			wantMsg: "Value " + zero + " for parameter RedrivePolicy is invalid. Reason: Invalid value for " +
				"maxReceiveCount: 0" + rangeText,
		},
		{
			name: "count over the limit", give: huge,
			wantMsg: "Value " + huge + " for parameter RedrivePolicy is invalid. Reason: Invalid value for " +
				"maxReceiveCount: 1001" + rangeText,
		},
		{
			name: "count that is not a number", give: word,
			wantMsg: "Value " + word + " for parameter RedrivePolicy is invalid. Reason: Invalid value for " +
				"maxReceiveCount: many" + rangeText,
		},
	}

	for _, protocol := range protocols {
		c := newStack(t).as(protocol)
		url := c.createQueue("q", nil)

		for _, tt := range tests {
			t.Run(protocol+"/"+tt.name, func(t *testing.T) {
				res := c.call("SetQueueAttributes", map[string]any{
					paramQueueURL: url, paramAttributes: map[string]string{attrRedrivePolicy: tt.give},
				})

				if tt.wantMsg == "" {
					assert.True(t, res.ok())

					return
				}

				assert.Equal(t, http.StatusBadRequest, res.status)
				assert.Equal(t, invalidParameterCode(protocol), res.errorCode())
				assert.Equal(t, tt.wantMsg, res.errorMessage())
			})
		}

		t.Run(protocol+"/an empty policy removes it", func(t *testing.T) {
			require.True(t, c.call("SetQueueAttributes", map[string]any{
				paramQueueURL: url, paramAttributes: map[string]string{attrRedrivePolicy: valid},
			}).ok())
			require.True(t, c.call("SetQueueAttributes", map[string]any{
				paramQueueURL: url, paramAttributes: map[string]string{attrRedrivePolicy: ""},
			}).ok())

			res := c.call(opGetQueueAttributes, map[string]any{
				paramQueueURL: url, paramAttributeNames: []string{attrRedrivePolicy},
			})

			assert.Empty(t, res.attributes())
		})
	}
}

func Test_Tags(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)

			createRes := c.call("CreateQueue", map[string]any{
				paramQueueName: "tagged", "tags": map[string]string{tagEnv: "dev", tagTeam: "a"},
			})
			require.True(t, createRes.ok())

			url := createRes.str(paramQueueURL)
			list := func() map[string]string {
				res := c.call("ListQueueTags", map[string]any{paramQueueURL: url})
				require.True(t, res.ok())

				return res.tags()
			}

			assert.Equal(t, map[string]string{tagEnv: "dev", tagTeam: "a"}, list())

			require.True(t, c.call("TagQueue", map[string]any{
				paramQueueURL: url, paramTags: map[string]string{tagEnv: tagProd, tagCost: "7"},
			}).ok())
			assert.Equal(t, map[string]string{tagEnv: tagProd, tagTeam: "a", tagCost: "7"}, list())

			require.True(t, c.call("UntagQueue", map[string]any{
				paramQueueURL: url, paramTagKeys: []string{tagTeam, "missing"},
			}).ok())
			assert.Equal(t, map[string]string{tagEnv: tagProd, tagCost: "7"}, list())

			require.True(t, c.call("UntagQueue", map[string]any{
				paramQueueURL: url, paramTagKeys: []string{tagEnv, tagCost},
			}).ok())
			assert.Empty(t, list())

			t.Run("tags are gone with the queue", func(t *testing.T) {
				require.True(t, c.call("TagQueue", map[string]any{
					paramQueueURL: url, paramTags: map[string]string{"k": "v"},
				}).ok())
				require.True(t, c.call("DeleteQueue", map[string]any{paramQueueURL: url}).ok())

				again := c.call("CreateQueue", map[string]any{paramQueueName: "tagged"})
				require.True(t, again.ok())
				assert.Empty(t, c.call("ListQueueTags", map[string]any{paramQueueURL: url}).tags())
			})

			t.Run("tagging a missing queue fails, tagging nothing does not", func(t *testing.T) {
				res := c.call("TagQueue", map[string]any{
					paramQueueURL: standardURLPrefix + "ghost", paramTags: map[string]string{"k": "v"},
				})
				assert.Equal(t, nonExistentCode(protocol), res.errorCode())

				res = c.call("TagQueue", map[string]any{paramQueueURL: standardURLPrefix + "ghost"})
				assert.True(t, res.ok())
			})
		})
	}
}

func Test_Permissions(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			url := c.createQueue("q", nil)

			policy := func() (map[string]any, bool) {
				res := c.call(opGetQueueAttributes, map[string]any{
					paramQueueURL: url, paramAttributeNames: []string{attrPolicy},
				})
				require.True(t, res.ok())

				raw, ok := res.attributes()[attrPolicy]
				if !ok {
					return nil, false
				}

				var doc map[string]any
				require.NoError(t, json.Unmarshal([]byte(raw), &doc))

				return doc, true
			}
			add := func(label string, accounts, actions []string) result {
				return c.call("AddPermission", map[string]any{
					paramQueueURL: url, "Label": label, "AWSAccountIds": accounts, "Actions": actions,
				})
			}

			require.True(t, add(labelFirst, []string{"111111111111", "222222222222"}, []string{"ReceiveMessage"}).ok())

			doc, ok := policy()
			require.True(t, ok)
			assert.Equal(t, map[string]any{
				paramVersion: "2008-10-17",
				"Id":         "arn:aws:sqs:us-east-1:" + defaultAccount + ":q/SQSDefaultPolicy",
				"Statement": []any{map[string]any{
					"Sid":    labelFirst,
					"Effect": "Allow",
					"Principal": map[string]any{"AWS": []any{
						"arn:aws:iam::111111111111:root", "arn:aws:iam::222222222222:root",
					}},
					paramAction: "SQS:ReceiveMessage",
					"Resource":  "arn:aws:sqs:us-east-1:" + defaultAccount + ":q",
				}},
			}, doc)

			require.True(t, add("second", []string{"333333333333"}, []string{opSendMessage, "*"}).ok())

			doc, _ = policy()
			statements, _ := doc["Statement"].([]any)
			require.Len(t, statements, 2)

			second, _ := statements[1].(map[string]any)
			assert.Equal(t, []any{"SQS:SendMessage", "SQS:*"}, second[paramAction])
			assert.Equal(t, map[string]any{"AWS": "arn:aws:iam::333333333333:root"}, second["Principal"])

			t.Run("a label is used once", func(t *testing.T) {
				res := add(labelFirst, []string{"111111111111"}, []string{opSendMessage})

				assert.Equal(t, invalidParameterCode(protocol), res.errorCode())
				assert.Equal(t, "Value first for parameter Label is invalid. Reason: Already exists.", res.errorMessage())
			})

			t.Run("an action has to exist", func(t *testing.T) {
				res := add("third", []string{"111111111111"}, []string{unknownAttribute})

				assert.Equal(t, invalidParameterCode(protocol), res.errorCode())
				assert.Equal(t, "Value SQS:Bogus for parameter ActionName is invalid. "+
					"Reason: Please refer to the appropriate WSDL for a list of valid actions. ", res.errorMessage())
			})

			t.Run("removing takes the statement out, and the last one the policy", func(t *testing.T) {
				remove := func(label string) result {
					return c.call("RemovePermission", map[string]any{paramQueueURL: url, "Label": label})
				}

				res := remove("nothing")
				assert.Equal(t, invalidParameterCode(protocol), res.errorCode())
				assert.Equal(t, "Value nothing for parameter Label is invalid. Reason: can't find label.",
					res.errorMessage())

				require.True(t, remove(labelFirst).ok())

				doc, _ := policy()
				statements, _ := doc["Statement"].([]any)
				assert.Len(t, statements, 1)

				require.True(t, remove("second").ok())

				_, ok := policy()
				assert.False(t, ok)
			})
		})
	}
}
