package secretsmanager_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"testing"

	"github.com/kylecannon/gostack/internal/persist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The cases below are the ones the property test in restart_test.go found, each
// written out: what the API answers before a restart, and that it answers the
// same after one.

var allFormats = []persist.Format{persist.FormatJSON, persist.FormatBinary}

// mustSendAs is sendAs for a call that must succeed. It returns the body.
func mustSendAs(t *testing.T, h http.Handler, at restartPlace, operation string, body any) map[string]any {
	t.Helper()

	code, answer := sendAs(t, h, at, operation, body)
	require.Equal(t, http.StatusOK, code, "%s %v: %s", operation, body, answer)

	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(answer), &out))

	return out
}

// valueOf is the SecretString GetSecretValue answers for id.
func valueOf(t *testing.T, h http.Handler, at restartPlace, id string) any {
	t.Helper()

	return mustSendAs(t, h, at, "GetSecretValue", map[string]any{keySecretID: id})[keyString]
}

// requireNotFound is a call that answers ResourceNotFoundException.
func requireNotFound(t *testing.T, h http.Handler, at restartPlace, operation string, body any) {
	t.Helper()

	code, answer := sendAs(t, h, at, operation, body)
	require.Equal(t, http.StatusBadRequest, code, "%s %v: %s", operation, body, answer)
	require.Contains(t, answer, "ResourceNotFoundException", "%s %v", operation, body)
}

// restartOn flushes eng and starts a new provider on the file it wrote.
func restartOn(t *testing.T, eng *persist.Engine, dir string, format persist.Format) http.Handler {
	t.Helper()

	eng.MarkDirty(serviceName)
	require.NoError(t, eng.Flush(t.Context()))

	_, _, h := startProvider(t, dir, format)

	return h
}

func create(name string, extra map[string]any) map[string]any {
	body := map[string]any{"Name": name, keyToken: token1, keyString: name}
	maps.Copy(body, extra)

	return body
}

// withBypass adds the tag that lets CreateSecret skip its name check to extra.
func withBypass(extra map[string]any) map[string]any {
	extra["Tags"] = tagList(bypass, "1")

	return extra
}

// An empty name is stored, as LocalStack stores it under the bypass tag, and a
// snapshot with one in it loads: it takes every other secret with it otherwise.
func Test_a_secret_with_an_empty_name_survives_a_restart(t *testing.T) {
	for _, format := range allFormats {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			eng, _, h := startProvider(t, dir, format)
			us := restartPlace{firstAccount, primaryRegion}
			eu := restartPlace{firstAccount, replicaRegion}

			mustSendAs(t, h, us, "CreateSecret", create("keep", nil))
			mustSendAs(t, h, eu, "CreateSecret", create("other-region", nil))
			empty := mustSendAs(t, h, us, "CreateSecret", create("", withBypass(map[string]any{keyString: "empty"})))
			emptyARN, ok := empty["ARN"].(string)
			require.True(t, ok)

			restarted := restartOn(t, eng, dir, format)

			assert.Equal(t, "keep", valueOf(t, restarted, us, "keep"))
			assert.Equal(t, "other-region", valueOf(t, restarted, eu, "other-region"))

			got := mustSendAs(t, restarted, us, "GetSecretValue", map[string]any{keySecretID: emptyARN})
			assert.Empty(t, got["Name"])
			assert.Equal(t, "empty", got[keyString])
			assert.Equal(t, emptyARN, got["ARN"])
		})
	}
}

// A region that was reset stays reset after a restart: a replica record it held
// is gone, and its name is free, though the primary still lists the region.
func Test_a_reset_of_a_replica_region_survives_a_restart(t *testing.T) {
	for _, format := range allFormats {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			eng, p, h := startProvider(t, dir, format)
			eu := restartPlace{firstAccount, replicaRegion}
			ap := restartPlace{firstAccount, regionAP}
			byName := map[string]any{keySecretID: "db"}

			mustSendAs(t, h, eu, "CreateSecret", create("db", map[string]any{
				keyReplicas: regionsOf(ap.region),
			}))
			assert.Equal(t, eu.region, mustSendAs(t, h, ap, "DescribeSecret", byName)["PrimaryRegion"])

			p.Registration().Reset(ap.account, ap.region)
			requireNotFound(t, h, ap, "DescribeSecret", byName)

			restarted := restartOn(t, eng, dir, format)

			requireNotFound(t, restarted, ap, "DescribeSecret", byName)
			requireNotFound(t, restarted, ap, "GetSecretValue", byName)
			assert.Empty(t, mustSendAs(t, restarted, ap, "ListSecrets", map[string]any{})["SecretList"])

			primary := mustSendAs(t, restarted, eu, "DescribeSecret", byName)
			assert.Len(t, primary["ReplicationStatus"], 1, "the primary still lists the region")

			// The name is free in the reset region, as it was before the restart.
			mustSendAs(t, restarted, ap, "CreateSecret", create("db", nil))
		})
	}
}

// A replica whose name starts like a partial ARN is placed by its name, not
// read as another secret's: s1 in the replica's region, created after it, is not
// the secret that name stands for.
func Test_a_replica_named_like_an_arn_survives_a_restart(t *testing.T) {
	for _, format := range allFormats {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			eng, _, h := startProvider(t, dir, format)
			ap := restartPlace{firstAccount, regionAP}
			us := restartPlace{firstAccount, primaryRegion}
			odd := partialARN

			mustSendAs(t, h, ap, "CreateSecret", create(odd, map[string]any{
				keyString:   "primary",
				keyReplicas: regionsOf(us.region),
			}))
			mustSendAs(t, h, us, "CreateSecret", create("s1", map[string]any{keyString: "unrelated"}))

			byOdd := map[string]any{keySecretID: odd}
			assert.Equal(t, "primary", valueOf(t, h, us, odd))

			restarted := restartOn(t, eng, dir, format)

			assert.Equal(t, "primary", valueOf(t, restarted, us, odd))
			assert.Equal(t, ap.region, mustSendAs(t, restarted, us, "DescribeSecret", byOdd)["PrimaryRegion"])
			assert.Equal(t, "unrelated", valueOf(t, restarted, us, "s1"))
		})
	}
}

// A replica record its primary has left behind is not a secret. Its name,
// arn:aws:secretsmanager:s1, is also the partial ARN of s1, and a lookup that
// stopped at the record answered "not found" until a restart dropped it and the
// same call reached s1.
func Test_a_record_left_behind_does_not_shadow_a_secret(t *testing.T) {
	for _, format := range allFormats {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			eng, p, h := startProvider(t, dir, format)
			ap := restartPlace{firstAccount, regionAP}
			us := restartPlace{firstAccount, primaryRegion}
			odd := partialARN

			mustSendAs(t, h, ap, "CreateSecret", create(odd, map[string]any{keyReplicas: regionsOf(us.region)}))
			mustSendAs(t, h, us, "CreateSecret", create("s1", map[string]any{keyString: "s1"}))
			assert.Equal(t, odd, mustSendAs(t, h, us, "DescribeSecret", map[string]any{keySecretID: odd})["Name"])

			// The primary's region is reset: its record in us-east-1 is left behind.
			p.Registration().Reset(ap.account, ap.region)

			assert.Equal(t, "s1", valueOf(t, h, us, odd), "the partial ARN reaches s1")

			restarted := restartOn(t, eng, dir, format)

			assert.Equal(t, "s1", valueOf(t, restarted, us, odd))
		})
	}
}

// CreateSecret stores a secret in the caller's account and region whatever its
// name looks like, as LocalStack does: an ARN of another service, or of another
// place, is only a name.
func Test_a_secret_named_like_an_arn_is_stored_where_the_caller_is(t *testing.T) {
	h := newHandler(t)
	us := restartPlace{firstAccount, primaryRegion}

	for _, name := range []string{"arn:aws:sqs:x:y:p-odd", "arn:aws:secretsmanager:eu-west-1:111111111111:secret:p-far"} {
		made := mustSendAs(t, h, us, "CreateSecret", create(name, nil))
		assert.Contains(t, made["ARN"], "arn:aws:secretsmanager:us-east-1:"+firstAccount+":secret:"+name+"-")
	}

	listed := mustSendAs(t, h, us, "ListSecrets", map[string]any{})
	assert.Len(t, listed["SecretList"], 2)
}
