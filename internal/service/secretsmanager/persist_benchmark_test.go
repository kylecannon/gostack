package secretsmanager_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/secretsmanager"
	"github.com/stretchr/testify/require"
)

// The persistence rows of the Secrets Manager matrix (docs/standards.md,
// "Benchmarks", axis "Persistence"): PutSecretValue under persist=on, and the
// snapshot and restore of a region holding 10 or 10,000 secrets.

const (
	persistOn = "persist=on"
	// persistFrequency is the flush delay of the engine: short enough that a
	// save is always running or due while the workers write.
	persistFrequency = 10 * time.Millisecond
)

// persistedHandler is a gateway in front of a Secrets Manager persisted under a
// temp dir, as PERSIST_SECRETSMANAGER=1 wires it. It checks at the end that a
// snapshot reached the disk, so a row cannot pass with persistence off.
func persistedHandler(b *testing.B) http.Handler {
	b.Helper()

	dir := b.TempDir()
	eng := persist.New(persist.Config{
		PersistVarSet: true, BaseDir: dir, Frequency: persistFrequency,
		MountDetector: func(string) bool { return false },
	})

	p := secretsmanager.New(eng.MarkDirty)
	eng.Register(serviceName, p.Registration().Persist)
	require.NoError(b, eng.Load(b.Context()))

	b.Cleanup(func() {
		err := eng.Close(context.WithoutCancel(b.Context()))
		if err != nil {
			b.Errorf("close the engine: %v", err)
		}

		_, err = os.Stat(filepath.Join(dir, serviceName, "store.json"))
		if err != nil {
			b.Errorf("Secrets Manager saved no snapshot: %v", err)
		}
	})

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{listenAddr}} },
		[]service.Registration{p.Registration()}, nil, eng,
	)
	require.NoError(b, err)

	return gw.Handler()
}

// filled is a provider holding count secrets, each with one 64-byte version.
func filled(b *testing.B, count int) *secretsmanager.Provider {
	b.Helper()

	p := secretsmanager.New(nil)
	h := handlerFor(b, p)
	value := string(bytes.Repeat([]byte("x"), 64))

	for i := range count {
		status := send(b.Context(), h, "CreateSecret", `{"Name":"bench-`+strconv.Itoa(i)+`","SecretString":"`+
			value+`","ClientRequestToken":"`+tokenOf(i, 0)+`"}`)
		require.Equal(b, http.StatusOK, status)
	}

	return p
}

// snapshotSizes are the secrets a snapshot and a restore run at. 10,000 is left
// out under the 10 ms rule (docs/standards.md, "Benchmarks"): a restore took
// 19.0 ms and a snapshot 14.8 ms, the slowest of three runs at -benchtime=500ms
// on linux/arm64 in a golang:1.27.4 container. The cost grows with the count,
// so 1,000 stands for the ladder.
var snapshotSizes = []int{10, 1_000}

func BenchmarkSnapshot(b *testing.B) {
	for _, count := range snapshotSizes {
		b.Run("secrets="+strconv.Itoa(count), func(b *testing.B) {
			p := filled(b, count)

			var size bytes.Buffer
			require.NoError(b, p.Snapshot(&size))
			b.SetBytes(int64(size.Len()))
			b.ReportAllocs()

			for b.Loop() {
				require.NoError(b, p.Snapshot(io.Discard))
			}
		})
	}
}

func BenchmarkRestore(b *testing.B) {
	for _, count := range snapshotSizes {
		b.Run("secrets="+strconv.Itoa(count), func(b *testing.B) {
			var saved bytes.Buffer
			require.NoError(b, filled(b, count).Snapshot(&saved))

			p := secretsmanager.New(nil)

			b.SetBytes(int64(saved.Len()))
			b.ReportAllocs()

			for b.Loop() {
				require.NoError(b, p.Restore(bytes.NewReader(saved.Bytes())))
			}
		})
	}
}
