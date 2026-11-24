package persist_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/kylecannon/gostack/internal/persist"
)

// assetPayloads is the payload axis: small, medium, and a body that crosses the
// 512 KiB spill limit of the ephemeral store.
var assetPayloads = []struct {
	name string
	size int
}{
	{"payload=1KiB", 1 << 10},
	{"payload=64KiB", 64 << 10},
	{"payload=1MiB", 1 << 20},
}

func benchAssets(b *testing.B) (*persist.Assets, string) {
	b.Helper()

	dir := b.TempDir()
	e := benchEngine(dir, flushEvery)
	e.Register(shelfService, newShelf())

	a, err := e.Assets(shelfService)
	if err != nil {
		b.Fatal(err)
	}

	return a, dir
}

// BenchmarkAssetsCommit writes and renames one asset per iteration, which is
// what a persisted PutObject adds to the ephemeral store's cost; the fsync is
// the next save's (BenchmarkSavePendingAssets). The file is unlinked after each
// one, so the disk does not fill.
func BenchmarkAssetsCommit(b *testing.B) {
	for _, payload := range assetPayloads {
		b.Run(payload.name, func(b *testing.B) {
			a, dir := benchAssets(b)
			body := bytes.Repeat([]byte{'x'}, payload.size)

			b.SetBytes(int64(payload.size))

			for b.Loop() {
				w, err := a.Create()
				if err != nil {
					b.Fatal(err)
				}

				_, err = w.Write(body)
				if err != nil {
					b.Fatal(err)
				}

				id, _, err := w.Commit()
				if err != nil {
					b.Fatal(err)
				}

				err = os.Remove(blobFile(dir, id))
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// savePendingDisk is the pending counts of BenchmarkSavePendingAssets
// (docs/standards.md, "Benchmarks"), which run on the disk, where the save pays
// the syncfs of the whole filesystem, a floor of the disk and not of this code,
// so they are the two smallest; the larger ones of the benchmark before the
// rule are left out. The slowest of ten runs on a runner: 2.2 ms for 1 pending
// asset and 2.6 ms for 10.
//
// The counts that run on a memory filesystem through memoryDir are one
// top-level benchmark each (BenchmarkSavePendingAssets100, 1000 and 10000),
// where the syncfs that ends a save costs nothing, so they measure the engine's
// work on each pending asset and its allocations, which the allocation gate
// holds (it caught the regression of PR #56). Their counts are those of the
// benchmark before the rule. The slowest of ten runs on a runner: 0.2 ms for
// 100, 0.6 ms for 1,000 and 5.8 ms for 10,000. One run of each at -benchtime
// 500ms takes 18, 31 and 23 s on a 4 CPU Linux container, where the untimed
// commits of each iteration are most of it, and all three in one benchmark
// came to 1h10m14s of `bench select`'s estimate on a runner, over what a shard
// job holds. A top-level benchmark is the smallest unit `bench select` places
// in a shard, so each is its own.
var savePendingDisk = []int{1, 10}

// BenchmarkSavePendingAssets measures one save of a service whose state names
// that many 1 KiB assets committed since the last save, all of which the save
// makes durable before its snapshot is renamed into place: one syncfs on
// Linux, an fsync of each asset and of each of their directories elsewhere.
// The commits are not timed, and neither is the removal of the last save's
// directory, so that a memory filesystem does not fill as the iterations run.
// It holds the disk counts; the others are the benchmarks below.
func BenchmarkSavePendingAssets(b *testing.B) {
	for _, pending := range savePendingDisk {
		b.Run(fmt.Sprintf("pending=%d", pending), func(b *testing.B) { benchSavePending(b, pending, b.TempDir()) })
	}
}

// BenchmarkSavePendingAssets100 is BenchmarkSavePendingAssets at 100 pending
// assets on a memory filesystem.
func BenchmarkSavePendingAssets100(b *testing.B) { benchSavePendingMemory(b, 100) }

// BenchmarkSavePendingAssets1000 is BenchmarkSavePendingAssets at 1,000 pending
// assets on a memory filesystem.
func BenchmarkSavePendingAssets1000(b *testing.B) { benchSavePendingMemory(b, 1000) }

// BenchmarkSavePendingAssets10000 is BenchmarkSavePendingAssets at 10,000
// pending assets on a memory filesystem.
func BenchmarkSavePendingAssets10000(b *testing.B) { benchSavePendingMemory(b, 10000) }

// benchSavePendingMemory runs the one cell of a top-level benchmark that holds
// a count of pending assets on a memory filesystem, named for the filesystem it
// got: the disk when there is no room in memory.
func benchSavePendingMemory(b *testing.B, pending int) {
	b.Helper()

	dir, fs := memoryDir(b, int64(pending)*(2<<10)+8<<20)

	b.Run(fmt.Sprintf("pending=%d/fs=%s", pending, fs), func(b *testing.B) { benchSavePending(b, pending, dir) })
}

// benchSavePending runs one cell of BenchmarkSavePendingAssets with each save's
// state in its own directory under base.
func benchSavePending(b *testing.B, pending int, base string) {
	b.Helper()

	body := bytes.Repeat([]byte{'x'}, 1<<10)
	run, last := 0, ""

	for b.Loop() {
		b.StopTimer()

		if last != "" {
			err := os.RemoveAll(last)
			if err != nil {
				b.Fatal(err)
			}
		}

		run++
		last = filepath.Join(base, strconv.Itoa(run))
		e := benchEngine(last, flushEvery)
		s := newShelf()
		e.Register(shelfService, s)

		a, err := e.Assets(shelfService)
		if err != nil {
			b.Fatal(err)
		}

		for range pending {
			s.add(commitBody(b, a, body))
		}

		e.MarkDirty(shelfService)
		b.StartTimer()

		err = e.Flush(b.Context())
		if err != nil {
			b.Fatal(err)
		}
	}
}

func commitBody(b *testing.B, a *persist.Assets, body []byte) string {
	b.Helper()

	w, err := a.Create()
	if err != nil {
		b.Fatal(err)
	}

	_, err = w.Write(body)
	if err != nil {
		b.Fatal(err)
	}

	id, _, err := w.Commit()
	if err != nil {
		b.Fatal(err)
	}

	return id
}

// BenchmarkAssetsOpen opens one asset and reads it whole per iteration.
func BenchmarkAssetsOpen(b *testing.B) {
	for _, payload := range assetPayloads {
		b.Run(payload.name, func(b *testing.B) {
			a, _ := benchAssets(b)

			w, err := a.Create()
			if err != nil {
				b.Fatal(err)
			}

			_, err = w.Write(bytes.Repeat([]byte{'x'}, payload.size))
			if err != nil {
				b.Fatal(err)
			}

			id, _, err := w.Commit()
			if err != nil {
				b.Fatal(err)
			}

			b.SetBytes(int64(payload.size))

			for b.Loop() {
				f, err := a.Open(id)
				if err != nil {
					b.Fatal(err)
				}

				_, err = io.Copy(io.Discard, f)
				_ = f.Close()

				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
