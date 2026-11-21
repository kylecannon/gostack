package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// committedPanics is a persisted service whose Committed panics, as a bug in
// one would at shutdown.
type committedPanics struct{ assetful }

func (*committedPanics) Committed() { panic("committed blew up") }

// Test_saver_save_survivesAPanicOfTheEngine: the first service's Committed
// panics, which passes through the engine's Stop to the saver and leaves the
// second service, taken and not yet written, dirty. The panic is logged with
// its stack, once, and the saver still runs the Flush that writes the second
// service and the Close that releases the assets.
func Test_saver_save_survivesAPanicOfTheEngine(t *testing.T) {
	const (
		panicky = "a-panicky"
		behind  = "z-behind"
	)

	logs := captureLog(t)
	eng, dir := persistentEngine(t)
	state, after := &committedPanics{}, &stamped{}

	eng.Register(panicky, state)
	eng.Register(behind, after)

	assets, err := eng.Assets(panicky)
	require.NoError(t, err)
	require.NotNil(t, assets)

	reg, regAfter := registration(panicky), registration(behind)
	reg.Persist, regAfter.Persist = state, after
	s := &saver{eng: eng, services: &providers{regs: []service.Registration{reg, regAfter}}}

	eng.MarkDirty(panicky)
	eng.MarkDirty(behind)

	require.NotPanics(t, func() { s.save(t.Context(), t.Context()) })

	text := logs.String()
	assert.Equal(t, 1, strings.Count(text, "persisting state panicked"), "the panic is logged once:\n%s", text)
	assert.Contains(t, text, "step=stop")
	assert.Contains(t, text, "committed blew up")
	assert.Contains(t, text, "stack=", "with its stack")

	assert.FileExists(t, filepath.Join(dir, behind, "store.json"), "the service behind the panic is saved after it")

	writer, err := assets.Create()
	if err == nil {
		writer.Abort()
	}

	require.Error(t, err, "Close ran, and released the assets")
}
