package dynamodb_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/stretchr/testify/require"
)

const (
	tableARNPrefix = "arn:aws:dynamodb:us-east-1:000000000000:table/"
	disabled       = "DISABLED"
)

// tag is a tag as the JSON decodes it.
func tag(key, value string) map[string]any {
	return map[string]any{"Key": key, "Value": value}
}

// ttlDescription is a TimeToLiveDescription as the JSON decodes it.
func ttlDescription(status, attribute string) map[string]any {
	out := map[string]any{"TimeToLiveStatus": status}
	if attribute != "" {
		out["AttributeName"] = attribute
	}

	return out
}

// deleteExpired calls the developer endpoint at path and returns the response.
func (s *stack) deleteExpired(tb testing.TB, path string) reply {
	tb.Helper()

	req := httptest.NewRequestWithContext(tb.Context(), http.MethodDelete, path, http.NoBody)
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)

	return reply{status: rec.Code, body: rec.Body.String()}
}

// Test_tags: tags given at CreateTable and by TagResource come back in the
// order their keys were first set; UntagResource and DeleteTable drop them.
func Test_tags(t *testing.T) {
	s := newStack(t)
	arn := tableARNPrefix + "tagged"

	s.ok(t, "CreateTable", `{"TableName":"tagged","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],`+
		`"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],`+
		`"BillingMode":"PAY_PER_REQUEST","Tags":[{"Key":"Name","Value":"t"},{"Key":"TestKey","Value":"true"}]}`)

	got := s.ok(t, "ListTagsOfResource", `{"ResourceArn":"`+arn+`"}`)
	require.Equal(t, []any{tag("Name", "t"), tag("TestKey", "true")}, got["Tags"])

	s.ok(t, "TagResource", `{"ResourceArn":"`+arn+`","Tags":[{"Key":"New","Value":"1"},{"Key":"Name","Value":"u"}]}`)
	got = s.ok(t, "ListTagsOfResource", `{"ResourceArn":"`+arn+`"}`)
	require.Equal(t, []any{tag("Name", "u"), tag("TestKey", "true"), tag("New", "1")}, got["Tags"])

	s.ok(t, "UntagResource", `{"ResourceArn":"`+arn+`","TagKeys":["Name","New"]}`)
	got = s.ok(t, "ListTagsOfResource", `{"ResourceArn":"`+arn+`"}`)
	require.Equal(t, []any{tag("TestKey", "true")}, got["Tags"])

	s.ok(t, "DeleteTable", `{"TableName":"tagged"}`)
	got = s.ok(t, "ListTagsOfResource", `{"ResourceArn":"`+arn+`"}`)
	require.Equal(t, []any{}, got["Tags"])
}

// Test_globalTables2017: the 2017 global table records, as LocalStack keeps
// them.
func Test_globalTables2017(t *testing.T) {
	s := newStack(t)
	group := `[{"RegionName":"us-east-1"},{"RegionName":"us-west-1"},{"RegionName":"eu-central-1"}]`

	got := s.ok(t, "CreateGlobalTable", `{"GlobalTableName":"gt","ReplicationGroup":`+group+`}`)
	require.Len(t, path(t, got, "GlobalTableDescription", "ReplicationGroup"), 3)
	require.Equal(t, "Replica active",
		path(t, got, "GlobalTableDescription", "ReplicationGroup", 0, "ReplicaStatusDescription"))

	s.fails(t, "CreateGlobalTable", `{"GlobalTableName":"gt","ReplicationGroup":`+group+`}`,
		"GlobalTableAlreadyExistsException", "Global table with this name already exists")
	s.fails(t, "DescribeGlobalTable", `{"GlobalTableName":"none"}`,
		"GlobalTableNotFoundException", "Global table with this name does not exist")

	got = s.ok(t, "UpdateGlobalTable", `{"GlobalTableName":"gt","ReplicaUpdates":[`+
		`{"Create":{"RegionName":"us-east-2"}},{"Create":{"RegionName":"us-west-2"}},`+
		`{"Delete":{"RegionName":"us-west-1"}}]}`)
	require.Len(t, path(t, got, "GlobalTableDescription", "ReplicationGroup"), 4)

	// The records span regions: another region sees the same one.
	other := s.call(t, "eu-west-1", "DescribeGlobalTable", `{"GlobalTableName":"gt"}`)
	require.Equal(t, http.StatusOK, other.status, other.body)

	got = s.ok(t, "ListGlobalTables", `{"RegionName":"us-west-2"}`)
	require.Equal(t, "gt", path(t, got, "GlobalTables", 0, "GlobalTableName"))
	got = s.ok(t, "ListGlobalTables", `{"RegionName":"us-west-1"}`)
	require.Equal(t, []any{}, got["GlobalTables"])
}

// Test_tableSettings: continuous backups, contributor insights and Kinesis
// destinations are stored and answered back.
func Test_tableSettings(t *testing.T) {
	s := newStack(t)
	s.ok(t, "CreateTable", hashTable("settings"))

	got := s.ok(t, "DescribeContinuousBackups", `{"TableName":"settings"}`)
	require.Equal(t, disabled, path(t, got, "ContinuousBackupsDescription", "PointInTimeRecoveryDescription",
		"PointInTimeRecoveryStatus"))

	got = s.ok(t, "UpdateContinuousBackups",
		`{"TableName":"settings","PointInTimeRecoverySpecification":{"PointInTimeRecoveryEnabled":true}}`)
	require.Equal(t, "ENABLED", path(t, got, "ContinuousBackupsDescription", "ContinuousBackupsStatus"))
	require.InDelta(t, 35, path(t, got, "ContinuousBackupsDescription", "PointInTimeRecoveryDescription",
		"RecoveryPeriodInDays"), 0)

	got = s.ok(t, "DescribeContinuousBackups", `{"TableName":"settings"}`)
	require.Equal(t, "ENABLED", path(t, got, "ContinuousBackupsDescription", "PointInTimeRecoveryDescription",
		"PointInTimeRecoveryStatus"))
	s.fails(t, "DescribeContinuousBackups", `{"TableName":"missing"}`, "TableNotFoundException",
		"Table not found: missing")

	got = s.ok(t, "DescribeContributorInsights", `{"TableName":"any-table","IndexName":"gsi"}`)
	require.Equal(t, "any-table", path(t, got, "TableName"))
	require.Equal(t, "gsi", path(t, got, "IndexName"))
	require.Equal(t, disabled, path(t, got, "ContributorInsightsStatus"))

	stream := "arn:aws:kinesis:us-east-1:000000000000:stream/s"
	s.fails(t, "DescribeKinesisStreamingDestination", `{"TableName":"missing"}`,
		"ResourceNotFoundException", "Cannot do operations on a non-existent table")
	s.fails(t, "EnableKinesisStreamingDestination", `{"TableName":"missing","StreamArn":"`+stream+`"}`,
		"ResourceNotFoundException", "Requested resource not found: Table: missing not found")
	s.fails(t, "UpdateKinesisStreamingDestination", `{"TableName":"missing","StreamArn":"x",`+
		`"UpdateKinesisStreamingConfiguration":{"ApproximateCreationDateTimePrecision":"BAD"}}`,
		"ResourceNotFoundException", "Cannot do operations on a non-existent table")

	got = s.ok(t, "EnableKinesisStreamingDestination", `{"TableName":"settings","StreamArn":"`+stream+`"}`)
	require.Equal(t, "ENABLING", got["DestinationStatus"])
	s.fails(t, "EnableKinesisStreamingDestination", `{"TableName":"settings","StreamArn":"`+stream+`"}`,
		"ValidationException", "Table is not in a valid state to enable Kinesis Streaming Destination:"+
			"EnableKinesisStreamingDestination must be DISABLED or ENABLE_FAILED to perform ENABLE operation.")

	got = s.ok(t, "UpdateKinesisStreamingDestination", `{"TableName":"settings","StreamArn":"`+stream+`",`+
		`"UpdateKinesisStreamingConfiguration":{"ApproximateCreationDateTimePrecision":"MICROSECOND"}}`)
	require.Equal(t, "UPDATING", got["DestinationStatus"])

	got = s.ok(t, "DisableKinesisStreamingDestination", `{"TableName":"settings","StreamArn":"`+stream+`"}`)
	require.Equal(t, "DISABLING", got["DestinationStatus"])

	got = s.ok(t, "DescribeKinesisStreamingDestination", `{"TableName":"settings"}`)
	require.Equal(t, []any{map[string]any{"StreamArn": stream, "DestinationStatus": disabled}},
		got["KinesisDataStreamDestinations"])
}

// putTTL puts an item keyed id whose ttl attribute is the given value.
func (s *stack) putTTL(tb testing.TB, table, id, ttl string) {
	tb.Helper()
	s.ok(tb, "PutItem", `{"TableName":"`+table+`","Item":{"id":{"S":"`+id+`"},"ttl":`+ttl+`}}`)
}

// present reports whether the item keyed id is stored, read in us-east-1.
func (s *stack) present(tb testing.TB, table, id string) bool {
	tb.Helper()

	got := s.call(tb, usEast1, "GetItem", `{"TableName":"`+table+`","Key":{"id":{"S":"`+id+`"}}}`)
	require.Equal(tb, http.StatusOK, got.status, got.body)

	return got.body != "{}"
}

// Test_timeToLive: the setting round-trips, and DELETE /_aws/dynamodb/expired
// removes only items whose attribute is a number at or before now.
func Test_timeToLive(t *testing.T) {
	s := newStack(t)
	s.ok(t, "CreateTable", hashTable("ttl-table"))

	s.fails(t, "DescribeTimeToLive", `{"TableName":"missing"}`,
		"ResourceNotFoundException", "Requested resource not found: Table: missing not found")

	got := s.ok(t, "DescribeTimeToLive", `{"TableName":"ttl-table"}`)
	require.Equal(t, ttlDescription(disabled, ""), got["TimeToLiveDescription"])

	got = s.ok(t, "UpdateTimeToLive",
		`{"TableName":"ttl-table","TimeToLiveSpecification":{"Enabled":true,"AttributeName":"ttl"}}`)
	require.Equal(t, true, path(t, got, "TimeToLiveSpecification", "Enabled"))

	got = s.ok(t, "DescribeTimeToLive", `{"TableName":"ttl-table"}`)
	require.Equal(t, ttlDescription("ENABLED", "ttl"), got["TimeToLiveDescription"])

	nowSec := time.Now().Unix()
	past, future := strconv.FormatInt(nowSec-10, 10), strconv.FormatInt(nowSec+1000, 10)
	s.putTTL(t, "ttl-table", "past", `{"N":"`+past+`"}`)
	s.putTTL(t, "ttl-table", "fraction", `{"N":"`+past+`.5"}`)
	s.putTTL(t, "ttl-table", "future", `{"N":"`+future+`"}`)
	s.putTTL(t, "ttl-table", "string", `{"S":"`+past+`"}`)

	got2 := s.deleteExpired(t, "/_aws/dynamodb/expired")
	require.Equal(t, http.StatusOK, got2.status)
	require.JSONEq(t, `{"ExpiredItems": 2}`, got2.body)

	require.False(t, s.present(t, "ttl-table", "past"))
	require.False(t, s.present(t, "ttl-table", "fraction"))
	require.True(t, s.present(t, "ttl-table", "future"))
	require.True(t, s.present(t, "ttl-table", "string"))

	got2 = s.deleteExpired(t, "/_gostack/services/dynamodb/expired")
	require.JSONEq(t, `{"ExpiredItems": 0}`, got2.body)

	s.ok(t, "UpdateTimeToLive",
		`{"TableName":"ttl-table","TimeToLiveSpecification":{"Enabled":false,"AttributeName":"ttl"}}`)
	got = s.ok(t, "DescribeTimeToLive", `{"TableName":"ttl-table"}`)
	require.Equal(t, ttlDescription(disabled, "ttl"), got["TimeToLiveDescription"])
}

// Test_timeToLive_sweepTimer: no timer exists until DYNAMODB_REMOVE_EXPIRED_ITEMS
// is set and a table turns time to live on; it then sweeps every hour, and it
// is gone once no table has time to live on.
func Test_timeToLive_sweepTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newStack(t)
		p := s.provider
		enable := `{"TableName":"swept","TimeToLiveSpecification":{"Enabled":true,"AttributeName":"ttl"}}`

		s.ok(t, "CreateTable", hashTable("swept"))
		require.False(t, dynamodb.ExpiryArmed(p), "no table has time to live")

		// The setting is off: turning time to live on arms nothing.
		s.ok(t, "UpdateTimeToLive", enable)
		require.False(t, dynamodb.ExpiryArmed(p), "DYNAMODB_REMOVE_EXPIRED_ITEMS is off")

		s.settings.Store(&dynamodb.Settings{Host: testHost, RemoveExpiredItems: true})
		s.ok(t, "UpdateTimeToLive", enable)
		require.True(t, dynamodb.ExpiryArmed(p))

		s.putTTL(t, "swept", "old", `{"N":"`+strconv.FormatInt(time.Now().Unix()-1, 10)+`"}`)
		s.putTTL(t, "swept", "young", `{"N":"`+strconv.FormatInt(time.Now().Add(90*time.Minute).Unix(), 10)+`"}`)

		time.Sleep(time.Hour + time.Second)
		synctest.Wait()

		require.False(t, s.present(t, "swept", "old"), "the first sweep removes the expired item")
		require.True(t, s.present(t, "swept", "young"))
		require.True(t, dynamodb.ExpiryArmed(p), "the sweep re-arms while time to live is on")

		time.Sleep(time.Hour)
		synctest.Wait()
		require.False(t, s.present(t, "swept", "young"), "the second sweep removes it")

		s.ok(t, "UpdateTimeToLive",
			`{"TableName":"swept","TimeToLiveSpecification":{"Enabled":false,"AttributeName":"ttl"}}`)
		require.False(t, dynamodb.ExpiryArmed(p), "turning the last table off stops the timer")

		s.ok(t, "UpdateTimeToLive", enable)
		require.True(t, dynamodb.ExpiryArmed(p))
		s.ok(t, "DeleteTable", `{"TableName":"swept"}`)
		require.False(t, dynamodb.ExpiryArmed(p), "deleting the last table stops the timer")

		s.ok(t, "CreateTable", hashTable("swept"))
		s.ok(t, "UpdateTimeToLive", enable)
		require.True(t, dynamodb.ExpiryArmed(p))

		// The setting turned off: the next run sweeps nothing and disarms.
		s.settings.Store(&dynamodb.Settings{Host: testHost})
		s.putTTL(t, "swept", "kept", `{"N":"1"}`)
		time.Sleep(time.Hour + time.Second)
		synctest.Wait()
		require.False(t, dynamodb.ExpiryArmed(p))
		require.True(t, s.present(t, "swept", "kept"))

		s.settings.Store(&dynamodb.Settings{Host: testHost, RemoveExpiredItems: true})
		s.ok(t, "UpdateTimeToLive", enable)
		p.Registration().Close(t.Context())
		require.False(t, dynamodb.ExpiryArmed(p), "Close stops the timer")
	})
}

// Test_replicas: a 2019 replica forwards the item and table operations to the
// owning region's one copy.
func Test_replicas(t *testing.T) {
	const owner, replica, other = "ap-south-1", "us-east-1", "eu-west-1"

	s := newStack(t)
	create := `{"TableName":"songs","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],` +
		`"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"BillingMode":"PAY_PER_REQUEST"}`
	require.Equal(t, http.StatusOK, s.call(t, owner, "CreateTable", create).status)

	bad := s.call(t, owner, "UpdateTable", `{"TableName":"songs","ReplicaUpdates":[{"Create":{"RegionName":"xx-1"}}]}`)
	require.Contains(t, bad.body, "Region xx-1 is not supported")

	for _, region := range []string{replica, other} {
		got := s.call(t, owner, "UpdateTable",
			`{"TableName":"songs","ReplicaUpdates":[{"Create":{"RegionName":"`+region+`","KMSMasterKeyId":"k"}}]}`)
		require.Equal(t, http.StatusOK, got.status, got.body)
	}

	again := s.call(t, owner, "UpdateTable", `{"TableName":"songs","ReplicaUpdates":[{"Create":{"RegionName":"`+
		replica+`"}}]}`)
	require.Contains(t, again.body, "because one or more replicas already existed as tables.")

	put := s.call(t, owner, "PutItem", `{"TableName":"songs","Item":{"id":{"S":"1"}}}`)
	require.Equal(t, http.StatusOK, put.status, put.body)
	require.True(t, s.present(t, "songs", "1"), "a replica-region read returns the owner's item")

	described := s.call(t, replica, "DescribeTable", `{"TableName":"songs"}`)
	require.Contains(t, described.body, `"TableArn":"arn:aws:dynamodb:us-east-1:000000000000:table/songs"`)
	require.Contains(t, described.body,
		`"Replicas":[{"RegionName":"ap-south-1","ReplicaStatus":"ACTIVE"},{"RegionName":"eu-west-1"`)

	listed := s.call(t, replica, "ListTables", `{}`)
	require.Contains(t, listed.body, `"songs"`)

	batch := s.call(t, replica, "BatchGetItem", `{"RequestItems":{"songs":{"Keys":[{"id":{"S":"1"}}]}}}`)
	require.Equal(t, http.StatusBadRequest, batch.status, "batch calls do not forward, as in LocalStack")

	gone := s.call(t, owner, "UpdateTable", `{"TableName":"songs","ReplicaUpdates":[{"Delete":{"RegionName":"`+
		other+`"}}]}`)
	require.Equal(t, http.StatusOK, gone.status, gone.body)
	missing := s.call(t, other, "GetItem", `{"TableName":"songs","Key":{"id":{"S":"1"}}}`)
	require.Contains(t, missing.body, "ResourceNotFoundException")

	twice := s.call(t, owner, "UpdateTable", `{"TableName":"songs","ReplicaUpdates":[{"Delete":{"RegionName":"`+
		other+`"}}]}`)
	require.Contains(t, twice.body, "were not part of the global table.")

	last := s.call(t, replica, "UpdateTable", `{"TableName":"songs","ReplicaUpdates":[{"Delete":{"RegionName":"`+
		replica+`"}}]}`)
	require.Equal(t, http.StatusOK, last.status, last.body)
	require.NotContains(t, s.call(t, owner, "DescribeTable", `{"TableName":"songs"}`).body, "Replicas")
}

// Test_removeExpired_counts: a sweep counts the items it removed, and a
// second finds none.
func Test_removeExpired_counts(t *testing.T) {
	s := newStack(t)
	s.ok(t, "CreateTable", hashTable("counted"))
	s.ok(t, "UpdateTimeToLive",
		`{"TableName":"counted","TimeToLiveSpecification":{"Enabled":true,"AttributeName":"ttl"}}`)

	for i := range 5 {
		s.putTTL(t, "counted", strconv.Itoa(i), `{"N":"`+strconv.Itoa(i)+`"}`)
	}

	require.Equal(t, 5, dynamodb.RemoveExpired(s.provider))
	require.Equal(t, 0, dynamodb.RemoveExpired(s.provider))
}
