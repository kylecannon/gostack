package main

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/kms"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// kmsServer is gostack's KMS behind its gateway, in this process: the workloads
// are checked against the service they load, not a fake of it.
func kmsServer(t *testing.T) client {
	t.Helper()

	g, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{listenAll}} },
		[]service.Registration{kms.New(nil).Registration()}, nil, nil,
	)
	require.NoError(t, err)

	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)

	return client{endpoint: srv.URL}
}

func Test_kms_workloads_run_against_KMS(t *testing.T) {
	c := kmsServer(t)

	for _, cell := range cellsFor(profileFull, target{}, serviceKMS) {
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

func Test_kms_workloads_refuse_cells_they_cannot_run(t *testing.T) {
	c := kmsServer(t)

	bad := []cellSpec{
		signCell("RSA_4096", "", levelLow),
		kmsCell(workloadCrypt, kmsPayloadLarge+1, nil, "", levelLow),
		kmsCell(workloadCrypt, kmsPayloadSmall, nil, "", 0),
	}

	for _, cell := range bad {
		wl, err := findWorkload(cell.Service, cell.Workload)
		require.NoError(t, err)

		_, err = wl.Run(t.Context(), c, cell, time.Millisecond)
		require.ErrorIs(t, err, errBadAxis, cell.id())
	}
}

func Test_kms_cells(t *testing.T) {
	full := cellsFor(profileFull, target{persistable: true}, serviceKMS)
	// crypt: 2 payloads x 3 concurrencies; datakey: 3 concurrencies; sign: 2
	// specs x 3 concurrencies; each with persistence off and on.
	assert.Len(t, full, 2*(6+3+6))
	assert.Len(t, cellsFor(profileFull, target{}, serviceKMS), 6+3+6, "a target that does not persist")
	assert.Len(t, cellsFor(profileQuick, target{persistable: true}, serviceKMS), 6)
	assert.Len(t, cellsFor(profileQuick, target{}, serviceKMS), 5, "a target that does not persist")

	ids := map[string]bool{}

	for _, c := range full {
		assert.False(t, ids[c.id()], "%s is listed twice", c.id())

		ids[c.id()] = true

		assert.Equal(t, c.persists(), c.PersistEnv == "PERSIST_KMS", c.id())
	}

	for _, c := range cellsFor(profileQuick, target{persistable: true}, serviceKMS) {
		assert.True(t, ids[c.id()], "%s is not a cell of the full profile", c.id())
	}

	assert.Equal(t, "kms/crypt/persist=off/payload=4KiB/c=64",
		kmsCell(workloadCrypt, kmsPayloadLarge, nil, "", levelHigh).id())
	assert.Equal(t, "kms/datakey/persist=on/c=16", kmsCell(workloadDataKey, 0, nil, persistEnvKMS, levelMid).id(),
		"a data key has no payload axis")
	assert.Equal(t, "kms/sign/persist=off/payload=32B/spec=ECC_NIST_P256/c=1", signCell(kmsSpecECC, "", levelLow).id())
}
