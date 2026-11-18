package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/kylecannon/gostack/internal/internalapi"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testService = "widgets"

// widgets is a Snapshotter with a Reset, like a service built on store.Bundle.
type widgets struct {
	data   string
	resets [][2]string
}

func (w *widgets) Snapshot(out io.Writer) error {
	_, err := io.WriteString(out, w.data)

	return err //nolint:wrapcheck // a test double
}

func (w *widgets) Restore(in io.Reader) error {
	data, err := io.ReadAll(in)
	w.data = string(data)

	return err //nolint:wrapcheck // a test double
}

func (w *widgets) Reset(account, region string) {
	w.resets = append(w.resets, [2]string{account, region})
	w.data = ""
}

// plain is a Snapshotter that cannot reset itself.
type plain struct{}

func (plain) Snapshot(io.Writer) error { return nil }

func (plain) Restore(io.Reader) error { return nil }

func registration(name string) service.Registration {
	return service.Registration{Metadata: service.Metadata{Name: name}}
}

func persistentEngine(t *testing.T) (*persist.Engine, string) {
	t.Helper()

	dir := t.TempDir()

	return persist.New(persist.Config{PersistVarSet: true, BaseDir: dir}), dir
}

func Test_serviceTable(t *testing.T) {
	eng, _ := persistentEngine(t)
	table := &serviceTable{regs: []service.Registration{registration("sqs"), registration("sts")}, eng: eng}

	assert.Equal(t, map[string]internalapi.ServiceState{
		"sqs": {State: "running", Persisted: true},
		"sts": {State: "running", Persisted: true},
	}, table.States())
	assert.Equal(t, []internalapi.ServiceCapability{{Name: "sqs"}, {Name: "sts"}}, table.Capabilities())

	off := &serviceTable{regs: []service.Registration{registration("sqs")}, eng: persist.New(persist.Config{})}
	assert.False(t, off.States()["sqs"].Persisted, "persistence is off")
	assert.NotPanics(t, off.Check)
}

func Test_stateController_Reset(t *testing.T) {
	w := &widgets{data: "x"}
	c := &stateController{
		regs:      []service.Registration{registration(testService), registration("plain")},
		snapshots: map[string]persist.Snapshotter{testService: w, "plain": plain{}},
		eng:       persist.New(persist.Config{}),
	}

	require.NoError(t, c.Reset(internalapi.StateScope{Service: testService, Account: "111111111111", Region: "eu-west-1"}))
	assert.Equal(t, [][2]string{{"111111111111", "eu-west-1"}}, w.resets)

	require.ErrorIs(t, c.Reset(internalapi.StateScope{Service: "nope"}), internalapi.ErrUnknownService)
	require.ErrorIs(t, c.Reset(internalapi.StateScope{Service: "plain"}), internalapi.ErrNotSupported)

	require.NoError(t, c.Reset(internalapi.StateScope{}), "every service that can reset does")
	assert.Len(t, w.resets, 2)

	empty := &stateController{eng: persist.New(persist.Config{})}
	require.NoError(t, empty.Reset(internalapi.StateScope{}))
	require.ErrorIs(t, empty.Reset(internalapi.StateScope{Service: testService}), internalapi.ErrUnknownService)
}

func Test_stateController_SaveLoad(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		c := &stateController{eng: persist.New(persist.Config{})}

		require.ErrorIs(t, c.Save(t.Context()), internalapi.ErrNotSupported)
		require.ErrorIs(t, c.Load(t.Context()), internalapi.ErrNotSupported)
	})

	t.Run("a save writes a service nothing marked dirty, and a load reads it back", func(t *testing.T) {
		eng, dir := persistentEngine(t)
		w := &widgets{data: `{"n":1}`}
		eng.Register(testService, w)
		c := &stateController{
			regs:      []service.Registration{registration(testService)},
			snapshots: map[string]persist.Snapshotter{testService: w},
			eng:       eng,
		}

		require.NoError(t, c.Save(t.Context()))

		saved, err := os.ReadFile(filepath.Join(dir, testService, "store.json"))
		require.NoError(t, err)
		assert.JSONEq(t, `{"n":1}`, string(saved))

		w.data = "changed in memory"
		require.NoError(t, c.Load(t.Context()))
		assert.Equal(t, `{"n":1}`, w.data)
	})
}

func Test_stateController_Reset_reaches_the_disk(t *testing.T) {
	eng, dir := persistentEngine(t)
	w := &widgets{data: `{"n":1}`}
	eng.Register(testService, w)
	c := &stateController{
		regs:      []service.Registration{registration(testService)},
		snapshots: map[string]persist.Snapshotter{testService: w},
		eng:       eng,
	}

	require.NoError(t, c.Save(t.Context()))
	require.NoError(t, c.Reset(internalapi.StateScope{Service: testService}))
	require.NoError(t, eng.Flush(t.Context()))

	saved, err := os.ReadFile(filepath.Join(dir, testService, "store.json"))
	require.NoError(t, err)
	assert.Empty(t, string(saved), "the wipe was saved, so a restart does not bring the service back")
}
