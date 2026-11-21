package main

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/secretsmanager"
	"github.com/kylecannon/gostack/internal/service/ssm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// secretsServer is gostack's Secrets Manager and SSM behind its gateway, in this
// process: the workloads are checked against the services they load, not a fake
// of them.
func secretsServer(t *testing.T) client {
	t.Helper()

	g, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{listenAll}} },
		[]service.Registration{secretsmanager.New(nil).Registration(), ssm.New().Registration()}, nil, nil,
	)
	require.NoError(t, err)

	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)

	return client{endpoint: srv.URL}
}

func Test_secrets_and_ssm_workloads_run_against_the_services(t *testing.T) {
	c := secretsServer(t)

	for _, cell := range cellsFor(profileFull, target{}, serviceSecrets, serviceSSM) {
		t.Run(cell.id(), func(t *testing.T) {
			wl, err := findWorkload(cell.Service, cell.Workload)
			require.NoError(t, err)

			run, err := wl.Run(t.Context(), c, cell, 50*time.Millisecond)
			require.NoError(t, err)
			assert.Positive(t, run.Cycles)

			for _, op := range wl.Ops() {
				stats := run.Ops[op]
				assert.Positive(t, stats.Count, "%s timed no %s", cell.id(), op)
				assert.Zero(t, stats.Errors, "%s: %s failed", cell.id(), op)
			}
		})
	}
}

func Test_secrets_and_ssm_workloads_refuse_cells_they_cannot_run(t *testing.T) {
	c := secretsServer(t)

	bad := []cellSpec{
		secretsCell(secretsPayloadLimit+1, "", levelLow),
		secretsCell(0, "", levelLow),
		secretsCell(secretsPayloadSmall, "", 0),
		ssmCell(workloadParameter, ssmPayloadLimit+1, "", levelLow),
		ssmCell(workloadParameter, ssmPayloadSmall, "", 0),
		ssmCell(workloadPath, ssmPayloadLimit+1, "", levelLow),
		ssmCell(workloadPath, ssmPayloadSmall, "", 0),
	}

	for _, cell := range bad {
		wl, err := findWorkload(cell.Service, cell.Workload)
		require.NoError(t, err)

		_, err = wl.Run(t.Context(), c, cell, time.Millisecond)
		require.ErrorIs(t, err, errBadAxis, cell.id())
	}
}

func Test_secrets_and_ssm_cells(t *testing.T) {
	// value: 2 payloads x 3 concurrencies; parameter and path likewise; each
	// with persistence off and on.
	assert.Len(t, cellsFor(profileFull, target{persistable: true}, serviceSecrets), 2*6)
	assert.Len(t, cellsFor(profileFull, target{}, serviceSecrets), 6, "a target that does not persist")
	assert.Len(t, cellsFor(profileFull, target{persistable: true}, serviceSSM), 2*(6+6))
	assert.Len(t, cellsFor(profileFull, target{}, serviceSSM), 6+6, "a target that does not persist")
	assert.Len(t, cellsFor(profileQuick, target{persistable: true}, serviceSecrets), 3)
	assert.Len(t, cellsFor(profileQuick, target{}, serviceSecrets), 2, "a target that does not persist")
	assert.Len(t, cellsFor(profileQuick, target{persistable: true}, serviceSSM), 4)
	assert.Len(t, cellsFor(profileQuick, target{}, serviceSSM), 3, "a target that does not persist")

	ids := map[string]bool{}

	full := cellsFor(profileFull, target{persistable: true}, serviceSecrets, serviceSSM)
	for _, c := range full {
		assert.False(t, ids[c.id()], "%s is listed twice", c.id())

		ids[c.id()] = true

		assert.Equal(t, c.persists(), c.PersistEnv == persistEnvSecrets || c.PersistEnv == persistEnvSSM, c.id())
	}

	for _, c := range cellsFor(profileQuick, target{persistable: true}, serviceSecrets, serviceSSM) {
		assert.True(t, ids[c.id()], "%s is not a cell of the full profile", c.id())
	}

	assert.Equal(t, "secretsmanager/value/persist=off/payload=64KiB/c=64",
		secretsCell(secretsPayloadLimit, "", levelHigh).id())
	assert.Equal(t, "ssm/parameter/persist=on/payload=256B/c=16",
		ssmCell(workloadParameter, ssmPayloadSmall, persistEnvSSM, levelMid).id())
	assert.Equal(t, "ssm/path/persist=off/payload=4KiB/c=1", ssmCell(workloadPath, ssmPayloadLimit, "", levelLow).id())
}

func Test_secrets_tokens_are_32_characters_and_never_repeat(t *testing.T) {
	tok := newTokens()
	seen := map[string]bool{}

	for range 100 {
		next := tok.new()
		assert.Len(t, next, 32)
		assert.False(t, seen[next], "%s was made twice", next)

		seen[next] = true
	}
}
