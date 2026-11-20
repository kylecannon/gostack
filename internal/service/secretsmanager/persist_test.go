package secretsmanager_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/secretsmanager"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serviceName is the name the engine keys Secrets Manager's state by.
const (
	serviceName = "secretsmanager"
	// listenAddr is the address the test gateways listen on.
	listenAddr = "0.0.0.0:4566"
)

// startEngine loads Secrets Manager from dir through an engine that writes
// format, and returns the engine and the gateway in front of the loaded
// provider, which tells the engine about every change.
func startEngine(t *testing.T, dir string, format persist.Format) (*persist.Engine, http.Handler) {
	t.Helper()

	eng, _, h := startProvider(t, dir, format)

	return eng, h
}

// startProvider is startEngine that also returns the provider, so a test can
// reset one account and region as the admin endpoint does.
func startProvider(
	t *testing.T, dir string, format persist.Format,
) (*persist.Engine, *secretsmanager.Provider, http.Handler) {
	t.Helper()

	eng := persist.New(persist.Config{
		PersistVarSet: true, BaseDir: dir, Frequency: time.Hour, Formats: []persist.Format{format},
		MountDetector: func(string) bool { return false },
	})

	p := secretsmanager.New(eng.MarkDirty)
	eng.Register(serviceName, p.Registration().Persist)
	require.NoError(t, eng.Load(t.Context()))

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{listenAddr}} },
		[]service.Registration{p.Registration()}, nil, eng,
	)
	require.NoError(t, err)

	secretsmanager.SetFunctionLookup(p, oneFunction)

	return eng, p, gw.Handler()
}

// putSample writes three versions of a secret with a policy, tags and a
// rotation, and a second secret scheduled for deletion. It returns that
// secret's DeletedDate.
func putSample(t *testing.T, h http.Handler) any {
	t.Helper()

	mustCall(t, h, "CreateSecret", `{"Name":"kept","SecretString":"v1","ClientRequestToken":"`+token1+`",`+
		`"Tags":[{"Key":"team","Value":"core"}]}`)
	mustCall(t, h, "PutSecretValue", `{"SecretId":"kept","SecretString":"v2","ClientRequestToken":"`+token2+`"}`)
	mustCall(t, h, "PutSecretValue", `{"SecretId":"kept","SecretBinary":"AAH/","ClientRequestToken":"`+token3+`"}`)
	mustCall(t, h, "PutResourcePolicy", `{"SecretId":"kept","ResourcePolicy":"{}"}`)
	mustCall(t, h, "RotateSecret", `{"SecretId":"kept","RotationLambdaARN":"`+functionARN+`",`+
		`"RotationRules":{"AutomaticallyAfterDays":3},"RotateImmediately":false,"ClientRequestToken":"`+token4+`"}`)

	mustCall(t, h, "CreateSecret", `{"Name":"going","SecretString":"g","ClientRequestToken":"`+token1+`"}`)
	mustCall(t, h, "DeleteSecret", `{"SecretId":"going","RecoveryWindowInDays":7}`)

	return mustCall(t, h, describeSecret, `{"SecretId":"going"}`)["DeletedDate"]
}

// checkSample reads back what putSample wrote.
func checkSample(t *testing.T, h http.Handler, deleted any) {
	t.Helper()

	current := mustCall(t, h, "GetSecretValue", `{"SecretId":"kept"}`)
	assert.Equal(t, token3, current["VersionId"])
	assert.Equal(t, "AAH/", current["SecretBinary"])

	previous := mustCall(t, h, "GetSecretValue", `{"SecretId":"kept","VersionStage":"AWSPREVIOUS"}`)
	assert.Equal(t, "v2", previous["SecretString"])

	versions := asList(t, mustCall(t, h, "ListSecretVersionIds",
		`{"SecretId":"kept","IncludeDeprecated":true}`)["Versions"])
	assert.Len(t, versions, 3)

	described := mustCall(t, h, describeSecret, `{"SecretId":"kept"}`)
	assert.Equal(t, []any{map[string]any{"Key": "team", "Value": "core"}}, described["Tags"])
	assert.Equal(t, true, described["RotationEnabled"])
	assert.Equal(t, functionARN, described["RotationLambdaARN"])
	assert.Contains(t, described, "NextRotationDate")

	policy := mustCall(t, h, "GetResourcePolicy", `{"SecretId":"kept"}`)
	assert.Equal(t, "{}", policy["ResourcePolicy"])

	assert.Equal(t, deleted, mustCall(t, h, describeSecret, `{"SecretId":"going"}`)["DeletedDate"])
}

// Test_persisted_secrets_survive_a_restart writes secrets through the gateway,
// flushes in each PERSIST_FORMAT, and loads them into a new provider.
func Test_persisted_secrets_survive_a_restart(t *testing.T) {
	for format, file := range map[persist.Format]string{
		persist.FormatJSON: "store.json", persist.FormatBinary: "store.bin",
	} {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			eng, h := startEngine(t, dir, format)

			deleted := putSample(t, h)
			require.NotNil(t, deleted)
			require.NoError(t, eng.Flush(t.Context()))

			_, err := os.Stat(filepath.Join(dir, serviceName, file))
			require.NoError(t, err, "the state is written as %s", file)

			_, restarted := startEngine(t, dir, format)
			checkSample(t, restarted, deleted)
		})
	}
}
