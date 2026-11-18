package main

import (
	"context"
	"io"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/internalapi"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testService = "widgets"

// widgets is a service whose state persists and resets, like SQS.
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

func (w *widgets) registration() service.Registration {
	reg := registration(testService)
	reg.Persist = w
	reg.Reset = w.Reset

	return reg
}

// bodies is a service whose state names assets, like S3.
type bodies struct {
	widgets

	ids []string
}

func (b *bodies) References() iter.Seq[string] {
	return slices.Values(b.ids)
}

func (b *bodies) registration() service.Registration {
	reg := b.widgets.registration()
	reg.Persist = b
	reg.Reset = func(account, region string) {
		b.Reset(account, region)
		b.ids = nil
	}

	return reg
}

// sessions is a service whose state resets and is not saved, like STS.
type sessions struct {
	resets int
}

func (s *sessions) registration() service.Registration {
	reg := registration("sessions")
	reg.Reset = func(string, string) { s.resets++ }

	return reg
}

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
	saved := registration("sqs")
	saved.Persist = &widgets{}
	table := &serviceTable{regs: []service.Registration{saved, registration("sts")}, eng: eng}

	assert.Equal(t, map[string]internalapi.ServiceState{
		"sqs": {State: "running", Persisted: true},
		"sts": {State: "running", Persisted: false},
	}, table.States())
	assert.Equal(t, []internalapi.ServiceCapability{{Name: "sqs"}, {Name: "sts"}}, table.Capabilities())

	off := &serviceTable{regs: []service.Registration{registration("sqs")}, eng: persist.New(persist.Config{})}
	assert.False(t, off.States()["sqs"].Persisted, "persistence is off")
	assert.NotPanics(t, off.Check)
}

func Test_stateController_Reset(t *testing.T) {
	w := &widgets{data: "x"}
	s := &sessions{}
	c := &stateController{
		regs: []service.Registration{w.registration(), s.registration(), registration("stateless")},
		eng:  persist.New(persist.Config{}),
	}

	ctx := t.Context()

	scoped := internalapi.StateScope{Service: testService, Account: "111111111111", Region: "eu-west-1"}
	require.NoError(t, c.Reset(ctx, scoped))
	assert.Equal(t, [][2]string{{"111111111111", "eu-west-1"}}, w.resets)

	require.NoError(t, c.Reset(ctx, internalapi.StateScope{Service: "sessions"}), "a service that is not saved resets too")
	assert.Equal(t, 1, s.resets)

	require.ErrorIs(t, c.Reset(ctx, internalapi.StateScope{Service: "nope"}), internalapi.ErrUnknownService)
	require.ErrorIs(t, c.Reset(ctx, internalapi.StateScope{Service: "stateless"}), internalapi.ErrNotSupported)

	require.NoError(t, c.Reset(ctx, internalapi.StateScope{}), "every service that keeps state resets, saved or not")
	assert.Len(t, w.resets, 2)
	assert.Equal(t, 2, s.resets)

	empty := &stateController{eng: persist.New(persist.Config{})}
	require.NoError(t, empty.Reset(ctx, internalapi.StateScope{}))
	require.ErrorIs(t, empty.Reset(ctx, internalapi.StateScope{Service: testService}), internalapi.ErrUnknownService)
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
		c := &stateController{regs: []service.Registration{w.registration()}, eng: eng}

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
	c := &stateController{regs: []service.Registration{w.registration()}, eng: eng}

	require.NoError(t, c.Save(t.Context()))
	require.NoError(t, c.Reset(t.Context(), internalapi.StateScope{Service: testService}))
	require.NoError(t, eng.Flush(t.Context()))

	saved, err := os.ReadFile(filepath.Join(dir, testService, "store.json"))
	require.NoError(t, err)
	assert.Empty(t, string(saved), "the wipe was saved, so a restart does not bring the service back")
}

// blocker is a service whose snapshot waits for release, which keeps a save
// running and the engine's guard held.
type blocker struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blocker) Snapshot(out io.Writer) error {
	b.once.Do(func() { close(b.started) })
	<-b.release

	_, err := io.WriteString(out, "x")

	return err //nolint:wrapcheck // a test double
}

func (*blocker) Restore(io.Reader) error { return nil }

// Test_stateController_Reset_tries_every_service: a reset of every service that
// fails for one of them still resets the ones after it, and the error names each
// service it could not reset.
func Test_stateController_Reset_tries_every_service(t *testing.T) {
	eng, _ := persistentEngine(t)

	first, last := &bodies{data: "a"}, &bodies{data: "c"}
	plain := &widgets{data: "b"}

	firstReg, plainReg, lastReg := first.registration(), plain.registration(), last.registration()
	firstReg.Metadata.Name, plainReg.Metadata.Name, lastReg.Metadata.Name = "first", "plain", "last"

	eng.Register("first", first)
	eng.Register("plain", plain)
	eng.Register("last", last)

	// A save is under way, so a reset of a service with assets waits for it.
	hold := &blocker{started: make(chan struct{}), release: make(chan struct{})}
	eng.Register("hold", hold)
	eng.MarkDirty("hold")

	flushed := make(chan error, 1)

	go func() { flushed <- eng.Flush(context.Background()) }()

	select {
	case <-hold.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the save did not start")
	}

	released := false
	release := func() {
		if !released {
			released = true

			close(hold.release)
		}
	}

	t.Cleanup(release)

	c := &stateController{regs: []service.Registration{firstReg, plainReg, lastReg}, eng: eng}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	err := c.Reset(ctx, internalapi.StateScope{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reset first:", "the error names the first service that failed")
	assert.Contains(t, err.Error(), "reset last:", "and the last one: the first failure did not stop the loop")
	assert.NotContains(t, err.Error(), "plain")
	assert.Empty(t, plain.data, "the service between them was reset")
	assert.Equal(t, "a", first.data, "a service that could not be reset keeps its state")

	release()
	require.NoError(t, <-flushed)

	require.NoError(t, c.Reset(t.Context(), internalapi.StateScope{}), "nothing holds the guard now")
	assert.Empty(t, first.data)
	assert.Empty(t, last.data)
}

func Test_stateController_Reset_removes_assets(t *testing.T) {
	eng, dir := persistentEngine(t)
	b := &bodies{data: "x"}
	eng.Register(testService, b)
	c := &stateController{regs: []service.Registration{b.registration()}, eng: eng}

	assets, err := eng.Assets(testService)
	require.NoError(t, err)

	w, err := assets.Create()
	require.NoError(t, err)

	id, _, err := w.Commit()
	require.NoError(t, err)

	b.ids = []string{id}
	blob := filepath.Join(dir, testService, "assets", "blobs", id[:2], id)

	require.NoError(t, c.Save(t.Context()))

	require.NoError(t, c.Reset(t.Context(), internalapi.StateScope{Service: testService, Account: "111111111111"}))
	require.NoError(t, eng.Flush(t.Context()))
	assert.FileExists(t, blob, "a reset of one account leaves the removals to the service")

	require.NoError(t, c.Reset(t.Context(), internalapi.StateScope{Service: testService}))
	assert.FileExists(t, blob, "nothing goes before the wipe is saved")

	require.NoError(t, eng.Flush(t.Context()))
	assert.NoFileExists(t, blob, "a reset of the whole service removes every asset it kept")
}
