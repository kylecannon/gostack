package main

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodbstreams"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ddbServer is gostack's DynamoDB and DynamoDB Streams behind its gateway, in
// this process: the workloads are checked against the services they load, not a
// fake of them.
func ddbServer(t *testing.T) client {
	t.Helper()

	ddb := dynamodb.New(func() dynamodb.Settings { return dynamodb.Settings{} }, nil)
	g, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{listenAll}} },
		[]service.Registration{ddb.Registration(), dynamodbstreams.New(ddb).Registration()}, nil, nil,
	)
	require.NoError(t, err)

	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)

	return client{endpoint: srv.URL}
}

// Every cell, the persistence-on ones included (persistence itself is the
// container's), runs against the real services with no failed call.
func Test_dynamodb_workloads_run_against_DynamoDB(t *testing.T) {
	c := ddbServer(t)

	for _, cell := range cellsFor(profileFull, target{persistable: true}, serviceDynamoDB) {
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

func Test_dynamodb_workloads_refuse_cells_they_cannot_run(t *testing.T) {
	c := ddbServer(t)

	bad := []cellSpec{
		ddbCell(workloadDDBItem, 5000, "", levelLow),
		ddbCell(workloadDDBQuery, ddbPayloadMedium, "", levelLow),
		ddbCell(workloadDDBUpdate, ddbPayloadSmall, "", 0),
	}

	for _, cell := range bad {
		wl, err := findWorkload(cell.Service, cell.Workload)
		require.NoError(t, err)

		_, err = wl.Run(t.Context(), c, cell, time.Millisecond)
		require.ErrorIs(t, err, errBadAxis, cell.id())
	}
}

func Test_dynamodb_cells(t *testing.T) {
	full := cellsFor(profileFull, target{persistable: true}, serviceDynamoDB)
	// item: 3 payloads x 3 concurrencies; query, update, transact and stream: 3
	// concurrencies each; every one with persistence off and on.
	assert.Len(t, full, 2*(9+4*3))
	assert.Len(t, cellsFor(profileFull, target{}, serviceDynamoDB), 9+4*3, "a target that does not persist")
	assert.Len(t, cellsFor(profileQuick, target{persistable: true}, serviceDynamoDB), 6)
	assert.Len(t, cellsFor(profileQuick, target{}, serviceDynamoDB), 5, "a target that does not persist")

	ids := map[string]bool{}

	for _, c := range full {
		assert.False(t, ids[c.id()], "%s is listed twice", c.id())

		ids[c.id()] = true

		assert.Equal(t, c.persists(), c.PersistEnv == "PERSIST_DYNAMODB", c.id())
	}

	for _, c := range cellsFor(profileQuick, target{persistable: true}, serviceDynamoDB) {
		assert.True(t, ids[c.id()], "%s is not a cell of the full profile", c.id())
	}

	assert.Equal(t, "dynamodb/item/persist=off/payload=399KiB/c=64",
		ddbCell(workloadDDBItem, ddbPayloadLarge, "", levelHigh).id())
	assert.Equal(t, "dynamodb/stream/persist=on/payload=100B/c=16",
		ddbCell(workloadDDBStream, ddbPayloadSmall, persistEnvDynamoDB, levelMid).id())
}
