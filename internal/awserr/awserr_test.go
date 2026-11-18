package awserr_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_Error(t *testing.T) {
	tests := []struct {
		name        string
		giveCode    string
		giveMessage string
		want        string
	}{
		{
			name:        "code and message",
			giveCode:    "QueueDoesNotExist",
			giveMessage: "no such queue",
			want:        "QueueDoesNotExist: no such queue",
		},
		{name: "empty message", giveCode: "InternalError", giveMessage: "", want: "InternalError: "},
		{name: "empty code", giveCode: "", giveMessage: "boom", want: ": boom"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &awserr.Error{Code: tt.giveCode, Message: tt.giveMessage}
			assert.Equal(t, tt.want, err.Error())
		})
	}
}

func Test_Error_As(t *testing.T) {
	wrapped := fmt.Errorf("receive: %w", &awserr.Error{Code: "QueueDoesNotExist", Message: "no such queue"})

	var got *awserr.Error

	require.ErrorAs(t, wrapped, &got)
	assert.Equal(t, "QueueDoesNotExist", got.Code)
}

func Test_NotImplemented(t *testing.T) {
	err := awserr.NotImplemented("sqs", "SendMessage")

	assert.Equal(t, "InternalFailure", err.Code)
	assert.Equal(t, http.StatusNotImplemented, err.StatusCode)
	assert.Equal(t, awserr.FaultReceiver, err.Fault)
	assert.Empty(t, err.QueryCode)
	assert.Equal(t, "API action 'SendMessage' for service 'sqs' not yet implemented", err.Message)
	assert.Equal(t, "InternalFailure: "+err.Message, err.Error())
}

func Test_Internal(t *testing.T) {
	err := awserr.Internal("disk full")

	assert.Equal(t, "InternalError", err.Code)
	assert.Equal(t, "disk full", err.Message)
	assert.Equal(t, http.StatusInternalServerError, err.StatusCode)
	assert.Equal(t, awserr.FaultReceiver, err.Fault)
	assert.Empty(t, err.QueryCode)
	assert.Equal(t, "InternalError: disk full", err.Error())
}
