package secretsmanager_test

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/kylecannon/gostack/internal/service/secretsmanager"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	primaryRegion = "us-east-1"
	replicaRegion = "eu-west-1"
	msgOnReplica  = "Operation not permitted on a replica secret. Call must be made in primary secret's region."
)

// mustCallIn is mustCall in region.
func mustCallIn(t *testing.T, h http.Handler, region, operation, body string) map[string]any {
	t.Helper()

	status, out := callIn(t, h, region, operation, body)
	require.Equal(t, http.StatusOK, status, "%s %s: %v", operation, body, out)

	return out
}

// requireReplicaError is requireError in the replica region.
func requireReplicaError(t *testing.T, h http.Handler, operation, body, code string) map[string]any {
	t.Helper()

	status, out := callIn(t, h, replicaRegion, operation, body)
	require.Equal(t, http.StatusBadRequest, status, "%s %s: %v", operation, body, out)
	require.Equal(t, code, out["__type"], "%s %s: %v", operation, body, out)

	return out
}

func Test_replica_reads_through_and_refuses_writes(t *testing.T) {
	h := newHandler(t)

	requireError(t, h, "CreateSecret", `{"Name":"r","SecretString":"v","ClientRequestToken":"`+token1+`",`+
		`"AddReplicaRegions":[{"Region":"`+primaryRegion+`"}]}`, "InvalidParameterException")
	requireError(t, h, describeSecret, `{"SecretId":"r"}`, "ResourceNotFoundException")

	created := mustCall(t, h, "CreateSecret", `{"Name":"r","SecretString":"v","ClientRequestToken":"`+token1+`",`+
		`"AddReplicaRegions":[{"Region":"`+replicaRegion+`"}]}`)
	assert.Equal(t, []any{map[string]any{
		"Region": replicaRegion, "KmsKeyId": "alias/aws/secretsmanager", "Status": "InSync",
		"StatusMessage": "Replication succeeded",
	}}, created["ReplicationStatus"])

	primaryARN, ok := created["ARN"].(string)
	require.True(t, ok)

	replicaARN := strings.ReplaceAll(primaryARN, primaryRegion, replicaRegion)

	described := mustCallIn(t, h, replicaRegion, describeSecret, `{"SecretId":"r"}`)
	assert.Equal(t, replicaARN, described["ARN"])
	assert.Equal(t, primaryRegion, described["PrimaryRegion"])
	assert.Len(t, described["ReplicationStatus"], 1)

	value := mustCallIn(t, h, replicaRegion, "GetSecretValue", `{"SecretId":"`+replicaARN+`"}`)
	assert.Equal(t, replicaARN, value["ARN"])
	assert.Equal(t, "v", value["SecretString"])

	// A write on the primary shows through the replica.
	mustCall(t, h, "PutSecretValue", `{"SecretId":"r","SecretString":"v2","ClientRequestToken":"`+token2+`"}`)
	value = mustCallIn(t, h, replicaRegion, "GetSecretValue", `{"SecretId":"r"}`)
	assert.Equal(t, "v2", value["SecretString"])

	out := requireReplicaError(t, h, "PutSecretValue",
		`{"SecretId":"r","SecretString":"x","ClientRequestToken":"`+token3+`"}`, "InvalidParameterException")
	assert.Equal(t, msgOnReplica, out["Message"])
	requireReplicaError(t, h, "UpdateSecret",
		`{"SecretId":"r","SecretString":"x","ClientRequestToken":"`+token3+`"}`, "InvalidRequestException")
	requireReplicaError(t, h, "TagResource", `{"SecretId":"r","Tags":[]}`, "InvalidParameterException")
	requireReplicaError(t, h, "DeleteSecret", `{"SecretId":"r"}`, "InvalidParameterException")
	requireReplicaError(t, h, "ReplicateSecretToRegions",
		`{"SecretId":"r","AddReplicaRegions":[{"Region":"eu-north-1"}]}`, "InvalidParameterException")

	policy := mustCallIn(t, h, replicaRegion, "GetResourcePolicy", `{"SecretId":"r"}`)
	assert.Equal(t, replicaARN, policy["ARN"])

	listed := mustCallIn(t, h, replicaRegion, "ListSecrets",
		`{"Filters":[{"Key":"primary-region","Values":["`+primaryRegion+`"]}]}`)
	entries := asList(t, listed["SecretList"])
	require.Len(t, entries, 1)
	assert.Equal(t, replicaARN, asMap(t, entries[0])["ARN"])
	assert.Equal(t, primaryRegion, asMap(t, entries[0])["PrimaryRegion"])

	listed = mustCall(t, h, "ListSecrets", `{"Filters":[{"Key":"primary-region","Values":["`+primaryRegion+`"]}]}`)
	assert.Len(t, asList(t, listed["SecretList"]), 1)

	out = requireError(t, h, "DeleteSecret", `{"SecretId":"r"}`, "InvalidParameterException")
	assert.Equal(t, "You can't delete secret r that still has replica regions ["+replicaRegion+"]", out["Message"])
}

func Test_replicate_and_remove_regions(t *testing.T) {
	h := newHandler(t)

	mustCall(t, h, "CreateSecret", `{"Name":"r","SecretString":"v","ClientRequestToken":"`+token1+`"}`)
	mustCallIn(t, h, replicaRegion, "CreateSecret",
		`{"Name":"r","SecretString":"other","ClientRequestToken":"`+token1+`"}`)

	// The name is taken in the replica region: Failed, and that secret stays.
	out := mustCall(t, h, "ReplicateSecretToRegions",
		`{"SecretId":"r","AddReplicaRegions":[{"Region":"`+replicaRegion+`"}]}`)
	assert.Equal(t, "r", out["ARN"])

	status := asMap(t, asList(t, out["ReplicationStatus"])[0])
	assert.Equal(t, "Failed", status["Status"])
	assert.Equal(t, "Replication failed: Secret name r already exists in region "+replicaRegion+".",
		status["StatusMessage"])

	out = mustCall(t, h, "RemoveRegionsFromReplication", `{"SecretId":"r","RemoveReplicaRegions":["`+replicaRegion+`"]}`)
	assert.Equal(t, []any{}, out["ReplicationStatus"])

	value := mustCallIn(t, h, replicaRegion, "GetSecretValue", `{"SecretId":"r"}`)
	assert.Equal(t, "other", value["SecretString"])

	// With force the secret there is replaced by the replica.
	out = mustCall(t, h, "ReplicateSecretToRegions", `{"SecretId":"r","ForceOverwriteReplicaSecret":true,`+
		`"AddReplicaRegions":[{"Region":"`+replicaRegion+`"}]}`)
	assert.Equal(t, "InSync", asMap(t, asList(t, out["ReplicationStatus"])[0])["Status"])

	value = mustCallIn(t, h, replicaRegion, "GetSecretValue", `{"SecretId":"r"}`)
	assert.Equal(t, "v", value["SecretString"])

	mustCall(t, h, "RemoveRegionsFromReplication",
		`{"SecretId":"r","RemoveReplicaRegions":["`+replicaRegion+`","nowhere-1"]}`)
	requireReplicaError(t, h, describeSecret, `{"SecretId":"r"}`, "ResourceNotFoundException")

	described := mustCall(t, h, describeSecret, `{"SecretId":"r"}`)
	assert.NotContains(t, described, "ReplicationStatus")

	mustCall(t, h, "DeleteSecret", `{"SecretId":"r","ForceDeleteWithoutRecovery":true}`)
}

func Test_replicas_survive_a_snapshot(t *testing.T) {
	p := secretsmanager.New(nil)
	h := handlerFor(t, p)

	mustCall(t, h, "CreateSecret", `{"Name":"r","SecretString":"v","ClientRequestToken":"`+token1+`",`+
		`"AddReplicaRegions":[{"Region":"`+replicaRegion+`"}]}`)
	mustCallIn(t, h, "eu-north-1", "CreateSecret", `{"Name":"r","SecretString":"x","ClientRequestToken":"`+token1+`"}`)
	mustCall(t, h, "ReplicateSecretToRegions", `{"SecretId":"r","AddReplicaRegions":[{"Region":"eu-north-1"}]}`)
	before := mustCall(t, h, describeSecret, `{"SecretId":"r"}`)

	var saved bytes.Buffer
	require.NoError(t, p.Snapshot(&saved))

	restored := secretsmanager.New(nil)
	require.NoError(t, restored.Restore(bytes.NewReader(saved.Bytes())))

	// Saved again before any read stamps LastAccessedDate, it is the same.
	var again bytes.Buffer
	require.NoError(t, restored.Snapshot(&again))
	assert.Equal(t, saved.String(), again.String())

	rh := handlerFor(t, restored)
	assert.Equal(t, before["ReplicationStatus"], mustCall(t, rh, describeSecret, `{"SecretId":"r"}`)["ReplicationStatus"])

	replica := mustCallIn(t, rh, replicaRegion, describeSecret, `{"SecretId":"r"}`)
	assert.Equal(t, primaryRegion, replica["PrimaryRegion"])

	// The Failed entry has no record: the other secret is restored as itself.
	other := mustCallIn(t, rh, "eu-north-1", "GetSecretValue", `{"SecretId":"r"}`)
	assert.Equal(t, "x", other["SecretString"])
}

// Test_replication_runs_with_everything_else replicates one name both ways
// between two regions while secrets are deleted, listed and reset. Run it
// with -race: it fails on a deadlock or a data race, not on an answer.
func Test_replication_runs_with_everything_else(t *testing.T) {
	p := secretsmanager.New(nil)
	h := handlerFor(t, p)

	const rounds = 30

	var wg sync.WaitGroup

	both := func(from, to string) {
		defer wg.Done()

		for range rounds {
			callIn(t, h, from, "CreateSecret", `{"Name":"x","SecretString":"v","ClientRequestToken":"`+token1+`",`+
				`"AddReplicaRegions":[{"Region":"`+to+`"}]}`)
			callIn(t, h, from, "ReplicateSecretToRegions", `{"SecretId":"x","ForceOverwriteReplicaSecret":true,`+
				`"AddReplicaRegions":[{"Region":"`+to+`"}]}`)
			callIn(t, h, from, "RemoveRegionsFromReplication", `{"SecretId":"x","RemoveReplicaRegions":["`+to+`"]}`)
			callIn(t, h, from, "DeleteSecret", `{"SecretId":"x","ForceDeleteWithoutRecovery":true}`)
		}
	}

	others := func(region string) {
		defer wg.Done()

		for range rounds {
			callIn(t, h, region, "ListSecrets", `{"Filters":[{"Key":"primary-region","Values":["`+region+`"]}]}`)
			callIn(t, h, region, describeSecret, `{"SecretId":"x"}`)
			callIn(t, h, region, "BatchGetSecretValue", `{"SecretIdList":["x"]}`)

			var buf bytes.Buffer
			assert.NoError(t, p.Snapshot(&buf))
		}
	}

	wg.Add(4)

	go both(primaryRegion, replicaRegion)
	go both(replicaRegion, primaryRegion)
	go others(primaryRegion)
	go others(replicaRegion)

	for range rounds {
		p.Registration().Reset("", "")
	}

	wg.Wait()
}
