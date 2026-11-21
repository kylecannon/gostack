package main

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sns"
	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listenAll is the listen address the in-process gateways of these tests are
// given; httptest picks the port they serve on.
const listenAll = "0.0.0.0:4566"

// snsServer is gostack's SNS and SQS behind one gateway, in this process, with
// SNS delivering through it as it does in the container: the workload is
// checked against the services it loads, not a fake of them.
func snsServer(t *testing.T) client {
	t.Helper()

	provider := sns.New(nil, nil)
	queues := sqs.New(func() sqs.Settings { return sqs.Settings{} }, func(string) {})

	t.Cleanup(queues.Close)

	g, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{listenAll}} },
		[]service.Registration{provider.Registration(), queues.Registration()}, provider.MountProtocol, nil,
	)
	require.NoError(t, err)

	provider.Bind(g.Handler())

	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(func() { provider.Close(context.WithoutCancel(t.Context())) })

	return client{endpoint: srv.URL}
}

// Every completed cycle took one delivery off every subscribed queue, so the
// receives are the publishes times the subscribers: a publish whose delivery
// did not reach a queue, or that a filter=on policy dropped, would fail its
// cycle instead.
func Test_sns_workload_takes_every_delivery_it_publishes(t *testing.T) {
	c := snsServer(t)

	for _, cell := range cellsFor(profileFull, target{}, serviceSNS) {
		t.Run(cell.id(), func(t *testing.T) {
			wl, err := findWorkload(cell.Service, cell.Workload)
			require.NoError(t, err)

			run, err := wl.Run(t.Context(), c, cell, 50*time.Millisecond)
			require.NoError(t, err)
			require.Positive(t, run.Cycles)

			spec, err := snsCellOf(cell)
			require.NoError(t, err)

			for _, op := range wl.Ops() {
				assert.Zero(t, run.Ops[op].Errors, "%s: %s failed", cell.id(), op)
			}

			assert.Equal(t, run.Cycles, run.Ops[opPublish].Count)
			assert.Equal(t, run.Cycles*spec.Subs, run.Ops[opReceive].Count)
			assert.Equal(t, run.Cycles*spec.Subs, run.Ops[opDelete].Count)
			assert.Contains(t, run.Counters, counterEmptyReceives)
		})
	}
}

func Test_sns_workload_refuses_cells_it_cannot_run(t *testing.T) {
	c := snsServer(t)

	bad := []cellSpec{
		snsCellSpec(snsPayloadSmall, 0, topicStandard, deliveryEnvelope, filterOff, "", levelLow),
		snsCellSpec(snsPayloadSmall, snsSubsOne, "ordered", deliveryEnvelope, filterOff, "", levelLow),
		snsCellSpec(snsPayloadSmall, snsSubsOne, topicStandard, "json", filterOff, "", levelLow),
		snsCellSpec(snsPayloadSmall, snsSubsOne, topicStandard, deliveryEnvelope, "maybe", "", levelLow),
		snsCellSpec(snsPublishMax, snsSubsOne, topicStandard, deliveryEnvelope, filterOff, "", levelLow),
		snsCellSpec(0, snsSubsOne, topicStandard, deliveryEnvelope, filterOff, "", levelLow),
		snsBase("", 0),
	}

	for _, cell := range bad {
		_, err := publishWorkload{}.Run(t.Context(), c, cell, time.Millisecond)
		require.ErrorIs(t, err, errBadAxis, cell.id())
	}
}

func Test_sns_cells(t *testing.T) {
	full := cellsFor(profileFull, target{persistable: true}, serviceSNS)
	// 2 topics x 2 deliveries x 2 filters, plus 10 and 100 subscribers and
	// 256 KB, each at 3 concurrencies, with persistence off and on.
	assert.Len(t, full, 2*(8+3)*3)
	assert.Len(t, cellsFor(profileFull, target{}, serviceSNS), (8+3)*3, "a target that does not persist")
	assert.Len(t, cellsFor(profileQuick, target{persistable: true}, serviceSNS), 7)
	assert.Len(t, cellsFor(profileQuick, target{}, serviceSNS), 6, "a target that does not persist")

	ids := map[string]bool{}

	for _, c := range full {
		assert.False(t, ids[c.id()], "%s is listed twice", c.id())

		ids[c.id()] = true

		assert.Equal(t, c.persists(), c.PersistEnv == "PERSIST_SNS", c.id())
	}

	for _, c := range cellsFor(profileQuick, target{persistable: true}, serviceSNS) {
		assert.True(t, ids[c.id()], "%s is not a cell of the full profile", c.id())
	}

	assert.Equal(t, "sns/publish/persist=off/payload=256B/subs=1/topic=standard/delivery=envelope/filter=off/c=16",
		snsBase("", levelMid).id())
	assert.Equal(t, "sns/publish/persist=on/payload=250KiB/subs=1/topic=standard/delivery=envelope/filter=off/c=64",
		snsCellSpec(snsPayloadLarge, snsSubsOne, topicStandard, deliveryEnvelope, filterOff, persistEnvSNS,
			levelHigh).id())
}
