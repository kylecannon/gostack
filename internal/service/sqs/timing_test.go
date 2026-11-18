package sqs_test

import (
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const minute = time.Minute

func Test_Purge_retry_delay(t *testing.T) {
	tests := []struct {
		protocol   string
		wantStatus int
	}{
		// The json protocol answers a client error with 400, 403 included, as
		// LocalStack's awsQueryCompatible serializer does.
		{protocol: protoJSON, wantStatus: http.StatusBadRequest},
		{protocol: protoQuery, wantStatus: http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newStack(t)
				s.settings.update(func(settings *sqs.Settings) { settings.DelayPurgeRetry = true })

				c := s.as(tt.protocol)
				url := c.createQueue("q", nil)
				purge := func() result { return c.call("PurgeQueue", map[string]any{paramQueueURL: url}) }

				require.True(t, purge().ok())

				res := purge()
				assert.Equal(t, tt.wantStatus, res.status)
				assert.Equal(t, purgeCode(tt.protocol), res.errorCode())
				assert.Equal(t, "Only one PurgeQueue operation on q is allowed every 60 seconds.", res.errorMessage())

				time.Sleep(minute - time.Second)
				assert.False(t, purge().ok(), "a second early")

				time.Sleep(time.Second)
				assert.True(t, purge().ok(), "after a minute")
				assert.False(t, purge().ok(), "the minute starts over")
			})
		})
	}
}

func purgeCode(protocol string) string {
	if protocol == protoQuery {
		return "AWS.SimpleQueueService.PurgeQueueInProgress"
	}

	return "PurgeQueueInProgress"
}

func Test_Purge_without_the_delay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newStack(t).as(protoJSON)
		url := c.createQueue("q", nil)

		for range 3 {
			assert.True(t, c.call("PurgeQueue", map[string]any{paramQueueURL: url}).ok())
		}

		res := c.call("PurgeQueue", map[string]any{paramQueueURL: standardURLPrefix + "ghost"})
		assert.Equal(t, "QueueDoesNotExist", res.errorCode())
	})
}

func Test_Delete_delays_creating_the_name(t *testing.T) {
	tests := []struct {
		protocol string
		wantCode string
	}{
		{protocol: protoJSON, wantCode: "QueueDeletedRecently"},
		{protocol: protoQuery, wantCode: "AWS.SimpleQueueService.QueueDeletedRecently"},
	}

	for _, tt := range tests {
		t.Run(tt.protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newStack(t)
				s.settings.update(func(settings *sqs.Settings) { settings.DelayRecentlyDeleted = true })

				c := s.as(tt.protocol)
				create := func(name string) result { return c.call("CreateQueue", map[string]any{paramQueueName: name}) }

				url := c.createQueue("q", nil)
				require.True(t, c.call("DeleteQueue", map[string]any{paramQueueURL: url}).ok())

				res := create("q")
				assert.Equal(t, http.StatusBadRequest, res.status)
				assert.Equal(t, tt.wantCode, res.errorCode())
				assert.Equal(t,
					"You must wait 60 seconds after deleting a queue before you can create another with the same name.",
					res.errorMessage())

				assert.True(t, create("another").ok(), "other names are free")

				time.Sleep(minute - time.Second)
				assert.False(t, create("q").ok(), "a second early")

				time.Sleep(time.Second)
				assert.True(t, create("q").ok(), "after a minute")
			})
		})
	}
}

func Test_Delete_without_the_delay(t *testing.T) {
	c := newStack(t).as(protoJSON)
	url := c.createQueue("q", nil)

	require.True(t, c.call("DeleteQueue", map[string]any{paramQueueURL: url}).ok())
	assert.True(t, c.call("CreateQueue", map[string]any{paramQueueName: "q"}).ok())
}

func Test_Delete_of_a_missing_queue(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			res := newStack(t).as(protocol).call("DeleteQueue", map[string]any{paramQueueURL: standardURLPrefix + "ghost"})

			assert.Equal(t, http.StatusBadRequest, res.status)
			assert.Equal(t, nonExistentCode(protocol), res.errorCode())
		})
	}
}
