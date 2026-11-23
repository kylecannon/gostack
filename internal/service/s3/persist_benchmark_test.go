package s3_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/blob"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service/s3"
)

// The persisted rows of the object and versioning matrices (docs/standards.md,
// "Benchmarks", axis "Persistence"). They are sub-benchmarks of the same
// functions as the persistence-off rows, under a persist=on key:
//
//	BenchmarkPutObject/persist=on/payload=1MiB/concurrency=1
//
// benchstat lines them up with the rows that have no persist key by splitting
// the columns on it: benchstat -col /persist bench.txt.
//
// The server is the one gostack wires under PERSIST_S3=1: bodies are assets of
// the persistence engine, written to a temp file and renamed while the request
// runs, with no fsync, and the engine saves the S3 snapshot every
// persistFrequency, syncing the bodies committed since the last save first.
// That is short enough that a save is always running or due while the workers
// write, so the rows show the cost of the two together: the request-time write,
// the save's sync of the new bodies and its copy of the state.

// persistFrequency is the flush delay of the engine.
const persistFrequency = 10 * time.Millisecond

// persistedPayloads are the payloads the persisted rows cover: one that stays in
// memory when unpersisted and one that spills to a file.
var persistedPayloads = []benchPayload{benchPayloads[0], benchPayloads[2]}

// persistRows are the persistence settings a row can have: "" is off, and has no
// key in its name.
var persistRows = []string{persistOff, persistOn}

const (
	persistOff = ""
	persistOn  = "persist=on"
)

// serverFor is a server with the persistence setting.
func serverFor(b *testing.B, persistence string) *objectBenchServer {
	b.Helper()

	if persistence == persistOn {
		return newPersistedBenchServer(b)
	}

	return newObjectBenchServer(b)
}

// persistName is the sub-benchmark name for a row with the persistence setting.
func persistName(persistence, name string) string {
	if persistence == persistOn {
		return persistOn + "/" + name
	}

	return name
}

// newPersistedBenchServer is newObjectBenchServer with S3 persisted under a
// temp dir, wired as cmd/gostack wires it: the assets open after Register and
// before Load, and S3 takes them after Load. It checks at the end that a
// snapshot reached the disk, so a row cannot pass with persistence off.
func newPersistedBenchServer(b *testing.B) *objectBenchServer {
	b.Helper()

	dir := b.TempDir()
	eng := persist.New(persist.Config{
		PersistVarSet: true, BaseDir: dir, Frequency: persistFrequency,
		MountDetector: func(string) bool { return false },
	})

	provider := s3.New(func() s3.Settings { return s3.Settings{LocalstackHost: localstackHost} },
		blob.NewEphemeral(b.TempDir()))
	b.Cleanup(func() { provider.Close(context.WithoutCancel(b.Context())) })

	reg := provider.Registration()
	eng.Register(s3Name, reg.Persist)

	assets, err := eng.Assets(s3Name)
	if err != nil || assets == nil {
		b.Fatalf("open the assets of s3: %v (assets %v)", err, assets)
	}

	err = eng.Load(b.Context())
	if err != nil {
		b.Fatal(err)
	}

	provider.UseBlobs(persist.NewBlobStore(assets))

	b.Cleanup(func() {
		err := eng.Close(context.WithoutCancel(b.Context()))
		if err != nil {
			b.Errorf("close the engine: %v", err)
		}

		_, err = os.Stat(filepath.Join(dir, s3Name, "store.json"))
		if err != nil {
			b.Errorf("S3 saved no snapshot: %v", err)
		}
	})

	return serveObjectBench(b, reg, eng)
}
