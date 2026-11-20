package dynamodbstreams_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Test_ttl_removal: an item DELETE /_aws/dynamodb/expired removes leaves a
// REMOVE record with DynamoDB's service identity, as AWS documents for a
// time-to-live deletion; a DeleteItem of the same kind of item carries none.
func Test_ttl_removal(t *testing.T) {
	s := newStack(t)
	arn, shard := s.createStream(t, "ttl", "OLD_IMAGE")
	s.ddbOK(t, "UpdateTimeToLive",
		`{"TableName":"ttl","TimeToLiveSpecification":{"Enabled":true,"AttributeName":"exp"}}`)

	past := strconv.FormatInt(time.Now().Unix()-10, 10)
	s.ddbOK(t, "PutItem", `{"TableName":"ttl","Item":{"id":{"S":"old"},"exp":{"N":"`+past+`"}}}`)
	s.ddbOK(t, "PutItem", `{"TableName":"ttl","Item":{"id":{"S":"gone"},"exp":{"N":"`+past+`"}}}`)
	s.ddbOK(t, "DeleteItem", `{"TableName":"ttl","Key":{"id":{"S":"gone"}}}`)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/_aws/dynamodb/expired", http.NoBody)
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"ExpiredItems": 1}`, rec.Body.String())

	got := s.records(t, s.iterator(t, arn, shard, "TRIM_HORIZON"), 0).Records
	require.Len(t, got, 4)

	deleted, expired := got[2], got[3]
	require.Equal(t, "REMOVE", deleted.EventName)
	require.Equal(t, "gone", deleted.Dynamodb.key(t))
	require.Nil(t, deleted.UserIdentity)

	require.Equal(t, "REMOVE", expired.EventName)
	require.Equal(t, "old", expired.Dynamodb.key(t))
	require.NotNil(t, expired.Dynamodb.OldImage)
	require.Equal(t, map[string]any{"Type": "Service", "PrincipalId": "dynamodb.amazonaws.com"}, expired.UserIdentity)
}
