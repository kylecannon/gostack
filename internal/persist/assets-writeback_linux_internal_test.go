//go:build linux

package persist

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
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

// forceSyncfsEnv set to 1 makes the fault fixture sync by syncfs whatever the
// filesystem, to test one for syncfsProven.
const forceSyncfsEnv = "GOSTACK_WRITEBACK_FAULT_SYNCFS"

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
	e := New(warnConfig(base, time.Hour, logs))
	n := &namer{}
	e.Register(syncService, n)

	a, err := e.Assets(syncService)
	require.NoError(t, err)

	if os.Getenv(forceSyncfsEnv) == "1" {
		forceSyncfs(t, a)
	}

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
	assert.Regexp(t, "input/output error|no space left on device", f.logs.errors()[0], "the error names the lost write")
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

	gate, returned := make(chan struct{}), make(chan error, 64)
	realSync := f.a.fsync
	f.a.fsync = func(file *os.File) error {
		<-gate

		err := realSync(file)
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

	f.a.fsync = realSync

	err = f.e.Flush(t.Context())
	t.Logf("save 2: %v", err)
	require.ErrorIs(t, err, ErrUnsafe)
	f.assertNeverNamed(t, id)
}

// foreignSyncFirst commits a body whose writeback fails, runs args twice as
// another process, which may see the error first, and then saves. The save must
// fail and the service turn off, however the other process's sync went.
func foreignSyncFirst(t *testing.T, args ...string) {
	t.Helper()

	f := newFaultFixture(t)
	id := f.commit(t, strings.Repeat("\x6b", faultBody))

	for i := range 2 {
		out, err := exec.CommandContext(t.Context(), args[0], args[1:]...).CombinedOutput()

		t.Logf("foreign %v #%d: err=%v out=%q", args, i, err, strings.TrimSpace(string(out)))
	}

	err := f.e.Flush(t.Context())
	t.Logf("save: %v", err)
	require.Error(t, err, "the save named a body whose writeback failed")
	f.assertNeverNamed(t, id)
}

// Test_Assets_ForeignSyncfsFirst is SQ-175 M1: another process runs syncfs on
// the assets filesystem (sync -f) after the writeback failed and before the
// save. On overlayfs the first syncfs on the mount takes the error, whichever
// process calls it, so a save by syncfs then succeeds.
func Test_Assets_ForeignSyncfsFirst(t *testing.T) {
	base := os.Getenv(writebackFaultEnv)
	foreignSyncFirst(t, "sync", "-f", filepath.Join(base, syncService, "assets"))
}

// Test_Assets_ForeignGlobalSyncFirst is Test_Assets_ForeignSyncfsFirst with a
// plain sync(2), which walks every filesystem in the kernel and reports nothing.
func Test_Assets_ForeignGlobalSyncFirst(t *testing.T) {
	foreignSyncFirst(t, "sync")
}

// Test_Assets_SaveAfterAFailedWriteback commits a body whose writeback fails
// and saves at once. Alone it checks the plain case; scripts/writeback-fault.sh
// also runs it beside another container that loops sync(2), the case where a
// process outside gostack's container takes the error first.
func Test_Assets_SaveAfterAFailedWriteback(t *testing.T) {
	f := newFaultFixture(t)
	id := f.commit(t, strings.Repeat("\x3e", faultBody))

	time.Sleep(500 * time.Millisecond) // the other container's syncs run

	err := f.e.Flush(t.Context())
	t.Logf("save: %v", err)
	require.Error(t, err, "the save named a body whose writeback failed")
	f.assertNeverNamed(t, id)
}
