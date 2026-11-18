package sqs_test

import (
	"strconv"
	"testing"

	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/stretchr/testify/assert"
)

func Test_ReceiveMessage_zero_stays_invalid_when_the_limit_is_disabled(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			s := newStack(t)
			s.settings.update(func(settings *sqs.Settings) { settings.DisableMaxNumberOfMessageLimit = true })

			c := s.as(protocol)
			url := c.createQueue("q", nil)

			res := c.call(opReceiveMessage, map[string]any{paramQueueURL: url, paramMaxMessages: 0})
			assert.Equal(t, errorCodes(protocol, codeInvalidParameterException, codeInvalidParameter), res.errorCode())
		})
	}
}

func Test_ReceiveMessage_takes_more_than_ten_when_the_limit_is_disabled(t *testing.T) {
	const sent = 14

	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			s := newStack(t)
			s.settings.update(func(settings *sqs.Settings) { settings.DisableMaxNumberOfMessageLimit = true })

			c := s.as(protocol)
			url := c.createQueue("q", nil)

			for i := range sent {
				c.send(url, "m"+strconv.Itoa(i), nil)
			}

			assert.Len(t, c.receive(url, map[string]any{paramMaxMessages: sent - 1}), sent-1)

			s.settings.update(func(settings *sqs.Settings) { settings.DisableMaxNumberOfMessageLimit = false })

			res := c.call(opReceiveMessage, map[string]any{paramQueueURL: url, paramMaxMessages: 11})
			assert.Equal(t, errorCodes(protocol, codeInvalidParameterException, codeInvalidParameter), res.errorCode(),
				"turning the setting off restores the limit on the next call")
		})
	}
}
