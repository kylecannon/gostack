package request_test

import (
	"context"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type otherKey struct{}

func Test_From(t *testing.T) {
	rc := &request.Context{
		Service:   "sqs",
		Operation: "SendMessage",
		Protocol:  protocol.KindJSON,
		AccountID: "000000000000",
		Region:    "us-east-1",
		RequestID: "00000000-0000-4000-8000-000000000000",
	}

	tests := []struct {
		name      string
		giveBuild func(context.Context) context.Context
		want      *request.Context
		wantOK    bool
	}{
		{
			name:      "stored",
			giveBuild: func(ctx context.Context) context.Context { return request.With(ctx, rc) },
			want:      rc,
			wantOK:    true,
		},
		{
			name: "nested under another value",
			giveBuild: func(ctx context.Context) context.Context {
				return context.WithValue(request.With(ctx, rc), otherKey{}, 1)
			},
			want:   rc,
			wantOK: true,
		},
		{
			name:      "absent",
			giveBuild: func(ctx context.Context) context.Context { return ctx },
			want:      nil,
			wantOK:    false,
		},
		{
			name: "stored under another key",
			giveBuild: func(ctx context.Context) context.Context {
				return context.WithValue(ctx, otherKey{}, rc)
			},
			want:   nil,
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := request.From(tt.giveBuild(t.Context()))

			assert.Equal(t, tt.wantOK, ok)
			assert.Same(t, tt.want, got)
		})
	}
}

func Test_With(t *testing.T) {
	first := &request.Context{Service: "sqs"}
	second := &request.Context{Service: "sts"}

	ctx := request.With(request.With(t.Context(), first), second)

	got, ok := request.From(ctx)
	require.True(t, ok)
	assert.Same(t, second, got, "the innermost With wins")

	parent, ok := request.From(request.With(t.Context(), first))
	require.True(t, ok)
	assert.Same(t, first, parent, "With does not modify its parent")
}
