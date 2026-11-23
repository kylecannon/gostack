package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The workload runs against gostack's DynamoDB in this process: every operation
// of the mix is timed and none fails, which also shows the service holds the
// index, the filtered scan and the batch write the mix uses.
func Test_large_table_workload_runs_against_DynamoDB(t *testing.T) {
	c := ddbServer(t)

	for _, concurrency := range []int{levelLow, levelMid} {
		cell := ddbCell(workloadDDBLarge, 0, "", concurrency)
		cell.Options = []option{{Key: optItems, Value: "300"}}
		cell.Memory = ddbLargeMemory

		wl, err := findWorkload(cell.Service, cell.Workload)
		require.NoError(t, err)

		run, err := wl.Run(t.Context(), c, cell, 300*time.Millisecond)
		require.NoError(t, err)
		assert.Positive(t, run.Cycles)

		for _, op := range wl.Ops() {
			stats := run.Ops[op]
			assert.Positive(t, stats.Count, "%s timed no %s", cell.id(), op)
			assert.Zero(t, stats.Errors, "%s: %s failed", cell.id(), op)
		}
	}
}

func Test_large_table_workload_refuses_a_cell_it_cannot_run(t *testing.T) {
	c := ddbServer(t)
	wl := largeTableWorkload{}

	for _, items := range []string{"", "0", "-3k", "many"} {
		cell := ddbCell(workloadDDBLarge, 0, "", levelLow)
		cell.Options = []option{{Key: optItems, Value: items}}

		_, err := wl.Run(t.Context(), c, cell, time.Millisecond)
		require.ErrorIs(t, err, errBadAxis, "items=%q", items)
	}

	_, err := wl.Run(t.Context(), c, ddbCell(workloadDDBLarge, 0, "", 0), time.Millisecond)
	require.ErrorIs(t, err, errBadAxis, "no workers")
}

// Only the large profile lists the workload, and it lists nothing else, so the
// full and quick profiles are what they were.
func Test_large_table_cells_belong_to_the_large_profile(t *testing.T) {
	large := cellsFor(profileLarge, target{persistable: true})
	assert.Len(t, large, 2*len(ddbLargeSteps)*2, "persistence off and on, every size, two concurrencies")
	assert.Len(t, cellsFor(profileLarge, target{}), len(ddbLargeSteps)*2, "a target that does not persist")

	ids := map[string]bool{}

	for _, c := range large {
		assert.False(t, ids[c.id()], "%s is listed twice", c.id())

		ids[c.id()] = true

		assert.Equal(t, serviceDynamoDB, c.Service)
		assert.Equal(t, workloadDDBLarge, c.Workload)
		assert.Equal(t, ddbLargeMemory, c.Memory)
	}

	assert.Equal(t, "dynamodb/large/persist=on/items=1m/mem=1g/c=64",
		ddbLargeCell(persistEnvDynamoDB, 1_000_000, levelHigh).id())
	assert.True(t, ids["dynamodb/large/persist=off/items=100k/mem=1g/c=16"])

	for _, profile := range []string{profileFull, profileQuick} {
		for _, c := range cellsFor(profile, target{persistable: true}) {
			assert.NotEqual(t, workloadDDBLarge, c.Workload, "%s lists %s", profile, c.id())
		}
	}
}

func Test_large_table_sizes_round_trip(t *testing.T) {
	for _, items := range []int{1, 999, 1000, 100_000, 300_000, 1_000_000, 2_500_000, 1_500} {
		cell := ddbLargeCell("", items, levelMid)

		got, err := cell.items()
		require.NoError(t, err, itemsName(items))
		assert.Equal(t, items, got, itemsName(items))
	}

	assert.Equal(t, "100k", itemsName(100_000))
	assert.Equal(t, "1m", itemsName(1_000_000))
	assert.Equal(t, "1500", itemsName(1_500))
}

func Test_large_table_seed_time_grows_with_the_table(t *testing.T) {
	wl := largeTableWorkload{}

	small := wl.SeedTime(ddbLargeCell("", 100_000, levelMid))
	big := wl.SeedTime(ddbLargeCell("", 1_000_000, levelMid))

	assert.Greater(t, big, small)
	assert.Greater(t, small, ddbSeedTimeoutExtra)
}

// An item is valid DynamoDB JSON with the attributes and sizes the workload
// documents, and the same index and version always make the same bytes.
func Test_large_table_items(t *testing.T) {
	sizes := map[int]bool{}

	for i := range 400 {
		raw := appendItem(nil, i, 0)

		var item map[string]map[string]json.RawMessage

		require.NoError(t, json.Unmarshal(raw, &item), "item %d: %s", i, raw)
		assert.GreaterOrEqual(t, len(item), ddbMinAttributes, "item %d", i)
		assert.LessOrEqual(t, len(item), ddbMaxAttributes, "item %d", i)

		for _, name := range []string{"pk", "sk", "gsi1pk", "gsi1sk", "kind", "n", "body"} {
			assert.Contains(t, item, name, "item %d", i)
		}

		var body string

		require.NoError(t, json.Unmarshal(item["body"]["S"], &body))
		assert.GreaterOrEqual(t, len(body), ddbPayloadSmall)
		assert.LessOrEqual(t, len(body), ddbPayloadMedium)

		sizes[len(body)/512] = true

		assert.Equal(t, raw, appendItem(nil, i, 0), "item %d is not repeatable", i)
	}

	assert.Greater(t, len(sizes), 3, "bodies are spread over the sizes")
	assert.NotEqual(t, appendItem(nil, 7, 0), appendItem(nil, 7, 1), "a version changes the item")

	var key map[string]map[string]string

	require.NoError(t, json.Unmarshal(keyJSON(41), &key))
	assert.Equal(t, "g2", key["pk"]["S"])
	assert.Equal(t, "i00000041", key["sk"]["S"])
}

func Test_large_table_requests_are_valid_JSON(t *testing.T) {
	m := &mixer{table: "t", items: 1000, rng: workerRand(0)}

	// The source picks the variant of each request, so enough of them reach every one.
	filters, indexes := 0, 0

	for key := range 200 {
		query, scan := m.query(key), m.scan(key)

		assert.True(t, json.Valid(query), "%s", query)
		assert.True(t, json.Valid(scan), "%s", scan)

		if bytes.Contains(query, []byte("FilterExpression")) && bytes.Contains(scan, []byte("FilterExpression")) {
			filters++
		}

		if bytes.Contains(query, []byte("IndexName")) {
			indexes++
		}
	}

	assert.Positive(t, filters, "a request with a filter")
	assert.Positive(t, indexes, "a query of the index")

	assert.True(t, json.Valid(appendPut(nil, "t", 3, 9)))
	assert.True(t, json.Valid(appendBatch(nil, "t", 0, ddbBatchSize, 1)))
}
