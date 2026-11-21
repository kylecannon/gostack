package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_saver_save_logsARefusedStartOnce: the startup load could not open the
// base dir, a file where a directory should be, so saving is off for every
// service. Each of the shutdown's three saves, Stop, Flush and Close, returns
// that refusal; it is logged once.
func Test_saver_save_logsARefusedStartOnce(t *testing.T) {
	const name = "stamped"

	logs := captureLog(t)

	base := filepath.Join(t.TempDir(), "state")
	require.NoError(t, os.WriteFile(base, []byte("not a directory"), 0o600))

	eng := persist.New(persist.Config{PersistVarSet: true, BaseDir: base})
	state := &stamped{}
	eng.Register(name, state)
	require.ErrorIs(t, eng.Load(t.Context()), persist.ErrUnsafe, "the startup load refuses the base dir")

	reg := registration(name)
	reg.Persist = state
	s := &saver{eng: eng, services: &providers{regs: []service.Registration{reg}}}

	eng.MarkDirty(name)
	s.save(t.Context(), t.Context())

	text := logs.String()
	assert.Equal(t, 1, strings.Count(text, `msg="save persisted state"`), "the refusal is logged once:\n%s", text)
	assert.Contains(t, text, "could not be opened at startup")
}
