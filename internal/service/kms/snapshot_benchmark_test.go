package kms_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/kms"
)

// The persistence rows of the KMS matrix (docs/standards.md, "Benchmarks",
// axis "Persistence"): CreateKey under persist=on, and the snapshot and
// restore of a region holding 10 or 10,000 keys.

// persistFrequency is the flush delay of a persisted bench: short enough that
// a save is always running or due while the calls run.
const persistFrequency = 10 * time.Millisecond

var snapshotSizes = []int{10, 10_000}

// newPersistedBench is a bench whose KMS is persisted under a temp dir, as
// PERSIST_KMS=1 wires it. It checks at the end that a snapshot reached the
// disk, so a row cannot pass with persistence off.
func newPersistedBench(b *testing.B) *bench {
	b.Helper()

	dir := b.TempDir()
	eng := persist.New(persist.Config{
		PersistVarSet: true, BaseDir: dir, Frequency: persistFrequency,
		MountDetector: func(string) bool { return false },
	})

	p := kms.New(eng.MarkDirty)
	eng.Register(serviceKMS, p.Registration().Persist)

	err := eng.Load(b.Context())
	if err != nil {
		b.Fatal(err)
	}

	b.Cleanup(func() {
		closeErr := eng.Close(context.WithoutCancel(b.Context()))
		if closeErr != nil {
			b.Errorf("close the engine: %v", closeErr)
		}

		_, statErr := os.Stat(filepath.Join(dir, serviceKMS, "store.json"))
		if statErr != nil {
			b.Errorf("KMS saved no snapshot: %v", statErr)
		}
	})

	g, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{testListen}} },
		[]service.Registration{p.Registration()}, nil, eng,
	)
	if err != nil {
		b.Fatal(err)
	}

	c := newBench(b)
	c.handler = g.Handler()

	return c
}

// filledKMS is a provider holding count symmetric keys, each with an alias.
func filledKMS(b *testing.B, count int) *kms.Provider {
	b.Helper()

	p := kms.New(nil)
	g, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{testListen}} },
		[]service.Registration{p.Registration()}, nil, nil,
	)
	if err != nil {
		b.Fatal(err)
	}

	c := newBench(b)
	c.handler = g.Handler()

	for i := range count {
		meta, ok := c.setup("CreateKey", map[string]any{})["KeyMetadata"].(map[string]any)
		if !ok {
			b.Fatal("CreateKey answered no KeyMetadata")
		}

		c.setup(opCreateAlias, map[string]any{
			fieldAliasName: "alias/bench-" + strconv.Itoa(i), fieldTargetKeyID: meta[fieldKeyID],
		})
	}

	return p
}

func BenchmarkSnapshot(b *testing.B) {
	for _, count := range snapshotSizes {
		b.Run("keys="+strconv.Itoa(count), func(b *testing.B) {
			p := filledKMS(b, count)

			var size bytes.Buffer

			err := p.Snapshot(&size)
			if err != nil {
				b.Fatal(err)
			}

			b.SetBytes(int64(size.Len()))
			b.ReportAllocs()

			for b.Loop() {
				err = p.Snapshot(io.Discard)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkRestore(b *testing.B) {
	for _, count := range snapshotSizes {
		b.Run("keys="+strconv.Itoa(count), func(b *testing.B) {
			var saved bytes.Buffer

			err := filledKMS(b, count).Snapshot(&saved)
			if err != nil {
				b.Fatal(err)
			}

			p := kms.New(nil)

			b.SetBytes(int64(saved.Len()))
			b.ReportAllocs()

			for b.Loop() {
				err = p.Restore(bytes.NewReader(saved.Bytes()))
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
