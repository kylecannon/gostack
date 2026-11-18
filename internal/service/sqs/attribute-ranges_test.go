package sqs_test

import (
	"maps"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	attrKmsReuse   = "KmsDataKeyReusePeriodSeconds"
	attrThroughput = "FifoThroughputLimit"
	codeInvalidVal = "InvalidAttributeValue"
	valueMsgGroup  = "messageGroup"
	valuePerGroup  = "perMessageGroupId"
	valuePerQueue  = "perQueue"
	nameRefused    = "refused"

	scopeQueueValue = "queue"
)

func rangeMessage(name, reason string) string {
	return "Invalid value for the parameter " + name + ". Reason: " + reason + "."
}

// The ranges are botocore's CreateQueue.Attributes documentation. The retention
// period has no lower bound: the parity suite sets it to 2.
func Test_Attributes_ranges(t *testing.T) {
	tests := []struct {
		name       string
		giveValid  []string
		giveBad    []string
		wantReason string
	}{
		{
			name: attrDelaySeconds, giveValid: []string{"0", "900"}, giveBad: []string{"-1", "901", "x", ""},
			wantReason: "Must be between 0 and 900",
		},
		{
			name: attrMaximumMessageSize, giveValid: []string{"1024", "1048576"},
			giveBad:    []string{"1023", "1048577", "0", "1.5"},
			wantReason: "Must be between 1024 and 1048576",
		},
		{
			name: attrMessageRetention, giveValid: []string{"2", "60", "1209600"}, giveBad: []string{"1209601", "x"},
			wantReason: "Must be at most 1209600",
		},
		{
			name: attrReceiveWait, giveValid: []string{"0", "20"}, giveBad: []string{"-1", "21"},
			wantReason: "Must be between 0 and 20",
		},
		{
			name: attrVisibilityTimeout, giveValid: []string{"0", "43200"}, giveBad: []string{"-1", "43201"},
			wantReason: "Must be between 0 and 43200",
		},
		{
			name: attrKmsReuse, giveValid: []string{"60", "86400"}, giveBad: []string{"59", "86401", "0"},
			wantReason: "Must be between 60 and 86400",
		},
	}

	for _, protocol := range protocols {
		for _, tt := range tests {
			t.Run(protocol+"/"+tt.name, func(t *testing.T) {
				c := newStack(t).as(protocol)
				url := c.createQueue("existing", nil)

				for _, value := range tt.giveValid {
					created := c.call("CreateQueue", map[string]any{
						paramQueueName: "create-" + value, paramAttributes: map[string]string{tt.name: value},
					})
					assert.True(t, created.ok(), "CreateQueue takes %s=%s", tt.name, value)

					set := c.call(opSetAttributes, map[string]any{
						paramQueueURL: url, paramAttributes: map[string]string{tt.name: value},
					})
					assert.True(t, set.ok(), "SetQueueAttributes takes %s=%s", tt.name, value)
				}

				for _, value := range tt.giveBad {
					created := c.call("CreateQueue", map[string]any{
						paramQueueName: nameRefused, paramAttributes: map[string]string{tt.name: value},
					})
					assert.Equal(t, http.StatusBadRequest, created.status, "CreateQueue refuses %s=%q", tt.name, value)
					assert.Equal(t, codeInvalidVal, created.errorCode())
					assert.Equal(t, rangeMessage(tt.name, tt.wantReason), created.errorMessage())

					set := c.call(opSetAttributes, map[string]any{
						paramQueueURL: url, paramAttributes: map[string]string{tt.name: value},
					})
					assert.Equal(t, http.StatusBadRequest, set.status, "SetQueueAttributes refuses %s=%q", tt.name, value)
					assert.Equal(t, codeInvalidVal, set.errorCode())
					assert.Equal(t, rangeMessage(tt.name, tt.wantReason), set.errorMessage())
				}

				// A refused call changes nothing: the last valid value is still there.
				got := c.call(opGetQueueAttributes, map[string]any{
					paramQueueURL: url, paramAttributeNames: []string{tt.name},
				})
				require.True(t, got.ok())
				assert.Equal(t, tt.giveValid[len(tt.giveValid)-1], got.attributes()[tt.name])
			})
		}
	}
}

// botocore: perMessageGroupId "is allowed only when the value for
// DeduplicationScope is messageGroup".
func Test_Attributes_throughput_limit_pairing(t *testing.T) {
	const wantMsg = "Invalid value for the parameter FifoThroughputLimit. " +
		"Reason: perMessageGroupId is allowed only when DeduplicationScope is messageGroup."

	tests := []struct {
		name       string
		giveCreate map[string]string
		giveSet    map[string]string
		wantErr    string
	}{
		{
			name:       "both set together",
			giveCreate: map[string]string{attrDedupScope: valueMsgGroup, attrThroughput: valuePerGroup},
		},
		{
			name:       "the limit with the default scope",
			giveCreate: map[string]string{attrThroughput: valuePerGroup}, wantErr: wantMsg,
		},
		{
			name:       "the limit with the queue scope",
			giveCreate: map[string]string{attrDedupScope: scopeQueueValue, attrThroughput: valuePerGroup},
			wantErr:    wantMsg,
		},
		{
			name:       "per queue with a message group scope",
			giveCreate: map[string]string{attrDedupScope: valueMsgGroup, attrThroughput: valuePerQueue},
		},
		{
			name:       "the limit set after the scope",
			giveCreate: map[string]string{attrDedupScope: valueMsgGroup},
			giveSet:    map[string]string{attrThroughput: valuePerGroup},
		},
		{
			name:    "the scope and the limit set together after creation",
			giveSet: map[string]string{attrDedupScope: valueMsgGroup, attrThroughput: valuePerGroup},
		},
		{
			name:    "the limit set on the default scope",
			giveSet: map[string]string{attrThroughput: valuePerGroup}, wantErr: wantMsg,
		},
		{
			name:       "the scope leaves a message group limit",
			giveCreate: map[string]string{attrDedupScope: valueMsgGroup, attrThroughput: valuePerGroup},
			giveSet:    map[string]string{attrDedupScope: scopeQueueValue}, wantErr: wantMsg,
		},
		{
			name:       "the scope and the limit leave together",
			giveCreate: map[string]string{attrDedupScope: valueMsgGroup, attrThroughput: valuePerGroup},
			giveSet:    map[string]string{attrDedupScope: scopeQueueValue, attrThroughput: valuePerQueue},
		},
	}

	for _, protocol := range protocols {
		for _, tt := range tests {
			t.Run(protocol+"/"+tt.name, func(t *testing.T) {
				c := newStack(t).as(protocol)

				attrs := map[string]string{attrFifoQueue: valueTrue}
				maps.Copy(attrs, tt.giveCreate)

				if tt.giveSet == nil {
					created := c.call("CreateQueue", map[string]any{paramQueueName: "q.fifo", paramAttributes: attrs})
					assertPairing(t, created, tt.wantErr)

					return
				}

				url := c.createQueue("q.fifo", attrs)
				set := c.call(opSetAttributes, map[string]any{paramQueueURL: url, paramAttributes: tt.giveSet})
				assertPairing(t, set, tt.wantErr)
			})
		}
	}
}

// assertPairing expects the call to succeed when wantMsg is empty.
func assertPairing(t *testing.T, res result, wantMsg string) {
	t.Helper()

	if wantMsg == "" {
		assert.True(t, res.ok())

		return
	}

	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Equal(t, codeInvalidVal, res.errorCode())
	assert.Equal(t, wantMsg, res.errorMessage())
}
