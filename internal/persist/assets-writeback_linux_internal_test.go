//go:build linux

package persist

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writebackFaultEnv names a base dir whose svc/assets is a mount of a
// filesystem whose data writes fail, as scripts/writeback-fault.sh makes one:
// ext4 on a loop device over a tmpfs too small for what the test writes. The
// tests that need it skip without it.
const writebackFaultEnv = "GOSTACK_WRITEBACK_FAULT_BASE"

// faultBody is larger than the free space under the faulty filesystem's
// loop device, so its writeback fails.
const faultBody = 32 << 20

// newFaultFixture is a syncFixture on the faulty filesystem.
func newFaultFixture(t *testing.T) *syncFixture {
	t.Helper()

	base := os.Getenv(writebackFaultEnv)
	if base == "" {
		t.Skipf("%s is not set; scripts/writeback-fault.sh runs this test", writebackFaultEnv)
	}

	logs := &warnings{}
	e := New(Config{PersistVarSet: true, BaseDir: base, Frequency: time.Hour, Logger: slog.New(logs)})
	n := &namer{}
	e.Register(syncService, n)

	a, err := e.Assets(syncService)
	require.NoError(t, err)

	return &syncFixture{e: e, n: n, a: a, stateFile: filepath.Join(base, syncService, FormatJSON.fileName()), logs: logs}
}

// assertNeverNamed checks that the service is off and that no state file names
// id: a body whose writeback failed must never be saved as durable.
func (f *syncFixture) assertNeverNamed(t *testing.T, id string) {
	t.Helper()

	data, err := os.ReadFile(f.stateFile)
	if !os.IsNotExist(err) {
		require.NoError(t, err)
	}

	assert.NotContains(t, string(data), id, "the state file names an asset whose writeback failed")
	assert.False(t, f.e.Enabled(syncService), "health reports the service not persisted")
	require.Len(t, f.logs.errors(), 1)
	assert.Contains(t, f.logs.errors()[0], "input/output error")
}

// Test_Assets_SyncfsRetryAfterAFailedSave is SQ-164 F1 (a) on a real kernel:
// syncfs reports a writeback error once, so the save after the one that saw it
// must fail too, rather than rename a snapshot naming the lost body.
func Test_Assets_SyncfsRetryAfterAFailedSave(t *testing.T) {
	f := newFaultFixture(t)
	id := f.commit(t, strings.Repeat("\xab", faultBody))

	err := f.e.Flush(t.Context())
	t.Logf("save 1: %v", err)
	require.Error(t, err, "setup: save 1 did not see the writeback error")

	f.e.MarkDirty(syncService)
	err = f.e.Flush(t.Context())
	t.Logf("save 2: %v", err)
	require.ErrorIs(t, err, ErrUnsafe)
	f.assertNeverNamed(t, id)
}

// Test_Assets_SyncfsAbandonedThenNextSave is SQ-164 F1 (b) on a real kernel: a
// save gives up on its syncfs at its deadline, the syncfs then reports the
// writeback error, and the next save must not rename a snapshot naming the body.
func Test_Assets_SyncfsAbandonedThenNextSave(t *testing.T) {
	f := newFaultFixture(t)
	id := f.commit(t, strings.Repeat("\xcd", faultBody))

	gate, returned := make(chan struct{}), make(chan error, 1)
	f.a.fsync = func(file *os.File) error {
		<-gate

		err := defaultSync(file)
		returned <- err

		return err
	}

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	err := f.e.Flush(ctx)
	t.Logf("save 1: %v", err)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	close(gate)

	abandoned := <-returned
	t.Logf("the abandoned syncfs returned: %v", abandoned)
	require.Error(t, abandoned, "setup: the abandoned syncfs did not see the writeback error")

	// The abandoned sync records its result after it returns.
	deadline := time.Now().Add(5 * time.Second)
	for f.e.Enabled(syncService) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	f.a.fsync = defaultSync

	err = f.e.Flush(t.Context())
	t.Logf("save 2: %v", err)
	require.ErrorIs(t, err, ErrUnsafe)
	f.assertNeverNamed(t, id)
}
