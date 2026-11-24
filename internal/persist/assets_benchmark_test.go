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
// the next save's (BenchmarkSavePendingDisk). The file is unlinked
// after each one, so the disk does not fill.
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

// savePendingDisk is the pending counts of BenchmarkSavePendingDisk
// (docs/standards.md, "Benchmarks"), which run on the disk, where the save pays
// the syncfs of the whole filesystem, a floor of the disk and not of this code,
// so they are the two smallest; the larger ones of the benchmark before the
// rule are left out. The slowest of ten runs on a runner of the benchmark
// before SQ-547, which timed an engine's first save: 2.2 ms for 1 pending asset
// and 2.6 ms for 10.
//
// The counts that run on a memory filesystem through memoryDir are one
// top-level benchmark each (BenchmarkSavePending100, 1000 and 10000), where
// the syncfs that ends a save costs nothing, so they measure the engine's work
// on each pending asset and its allocations, which the allocation gate holds
// (it caught the regression of PR #56). Their counts are those of the
// benchmark before the rule. The slowest of ten runs on a runner of the
// benchmark before SQ-547, which timed an engine's first save: 0.2 ms for 100,
// 0.6 ms for 1,000 and 5.8 ms for 10,000.
//
// Every engine of a cell serves all its iterations. One run of each memory
// cell at -benchtime 500ms takes 1.3 to 1.9 s on a 4 CPU Linux container, in
// two runs 2.3 to 2.4, 2.5 and 2.2 to 2.4 times what it times at 100, 1,000
// and 10,000 pending, and the disk benchmark 1.7 to 1.9 s, 1.1 to 1.4 times.
// A memory cell that falls back to the disk takes 1.0 to 3.0 s, at most 4.8
// times, since it recommits its assets each round. Until SQ-547 each iteration
// built an engine, committed its assets and removed the last one's directory,
// untimed, and a memory cell took 18 to 34 s for 0.6 s timed
// (BenchmarkSavePendingAssets100, 1000 and 10000): `bench select` estimates a
// shard from one run, and a shard job ran out of its hour on them. They were
// renamed then, because the bench job runs a name the base also holds on the
// base's own cells, on the shard and outside the estimate of `bench select`.
var savePendingDisk = []int{1, 10}

// BenchmarkSavePendingDisk measures one save of a service whose state names
// that many 1 KiB assets committed since the last save, all of which the save
// makes durable before its snapshot is renamed into place: one syncfs on
// Linux, an fsync of each asset and of each of their directories elsewhere.
// Between saves, untimed, the last save's assets are unlinked and as many new
// ones committed, so each save has new bytes to make durable. It holds the disk
// counts; the others are the benchmarks below.
func BenchmarkSavePendingDisk(b *testing.B) {
	for _, pending := range savePendingDisk {
		b.Run(fmt.Sprintf("pending=%d", pending), func(b *testing.B) {
			p := newPendingShelf(b, b.TempDir(), pending)

			benchSavePending(b, []*pendingShelf{p}, func(p *pendingShelf) { recommit(b, p) })
		})
	}
}

// BenchmarkSavePending100 is BenchmarkSavePendingDisk at 100 pending assets on
// a memory filesystem.
func BenchmarkSavePending100(b *testing.B) { benchSavePendingMemory(b, 100, 8) }

// BenchmarkSavePending1000 is BenchmarkSavePendingDisk at 1,000 pending assets
// on a memory filesystem.
func BenchmarkSavePending1000(b *testing.B) { benchSavePendingMemory(b, 1000, 4) }

// BenchmarkSavePending10000 is BenchmarkSavePendingDisk at 10,000 pending
// assets on a memory filesystem.
func BenchmarkSavePending10000(b *testing.B) { benchSavePendingMemory(b, 10000, 1) }

// benchSavePendingMemory runs the one cell of a top-level benchmark that holds
// a count of pending assets on a memory filesystem, named for the filesystem it
// got: the disk when there is no room in memory. On a memory filesystem its
// engines' assets are put back as pending with persist.PendAgain, so no
// iteration writes or removes a file: the save's syncfs has no bytes to write
// back, so whether they are new does not change it. On the disk they are
// recommitted as BenchmarkSavePendingDisk's are, since there a save of assets
// that are already durable writes nothing back and would under-read one.
//
// It saves engines of their own in turn, so that the timer stops once for that
// many saves: a stop costs about 0.3 ms on a 4 CPU Linux container, twice a
// save of 100 pending assets.
func benchSavePendingMemory(b *testing.B, pending, engines int) {
	b.Helper()

	dir, fs := memoryDir(b, int64(engines*pending)*(2<<10)+8<<20)

	b.Run(fmt.Sprintf("pending=%d/fs=%s", pending, fs), func(b *testing.B) {
		shelves := make([]*pendingShelf, engines)
		for i := range shelves {
			shelves[i] = newPendingShelf(b, filepath.Join(dir, strconv.Itoa(i)), pending)
		}

		pend := func(p *pendingShelf) { persist.PendAgain(p.a, p.ids) }
		if fs != "tmpfs" {
			pend = func(p *pendingShelf) { recommit(b, p) }
		}

		benchSavePending(b, shelves, pend)
	})
}

// pendingShelf is an engine with its state under dir, whose shelf names ids,
// assets it committed.
type pendingShelf struct {
	e   *persist.Engine
	s   *shelf
	a   *persist.Assets
	dir string
	ids []string
}

// recommit unlinks the assets p's last save made durable and commits as many
// new ones, so the next save has new bytes to make durable.
func recommit(b *testing.B, p *pendingShelf) {
	b.Helper()

	for _, id := range p.ids {
		p.s.drop(id)

		err := os.Remove(blobFile(p.dir, id))
		if err != nil {
			b.Fatal(err)
		}
	}

	p.ids = commitPending(b, p.s, p.a, len(p.ids))
}

// newPendingShelf starts an engine with its state under dir, commits pending
// assets, and saves them once. The first save of an engine allocates the timer
// of its slow flushes and finds no state file to replace, so every save the
// benchmark times is a later one, as a service's saves are.
func newPendingShelf(b *testing.B, dir string, pending int) *pendingShelf {
	b.Helper()

	e := benchEngine(dir, flushEvery)
	b.Cleanup(func() { _ = e.Close(b.Context()) })

	s := newShelf()
	e.Register(shelfService, s)

	a, err := e.Assets(shelfService)
	if err != nil {
		b.Fatal(err)
	}

	p := &pendingShelf{e: e, s: s, a: a, dir: dir, ids: commitPending(b, s, a, pending)}
	e.MarkDirty(shelfService)

	err = e.Flush(b.Context())
	if err != nil {
		b.Fatal(err)
	}

	return p
}

// benchSavePending times one save of one of shelves an iteration, in turn.
// Before each round, untimed, pend leaves each shelf's assets pending again.
func benchSavePending(b *testing.B, shelves []*pendingShelf, pend func(p *pendingShelf)) {
	b.Helper()

	next := len(shelves)

	for b.Loop() {
		if next == len(shelves) {
			b.StopTimer()

			for _, p := range shelves {
				pend(p)
				p.e.MarkDirty(shelfService)
			}

			next = 0

			b.StartTimer()
		}

		err := shelves[next].e.Flush(b.Context())
		if err != nil {
			b.Fatal(err)
		}

		next++
	}
}

// commitPending commits count 1 KiB assets and adds each to s.
func commitPending(b *testing.B, s *shelf, a *persist.Assets, count int) []string {
	b.Helper()

	body := bytes.Repeat([]byte{'x'}, 1<<10)
	ids := make([]string, count)

	for i := range ids {
		ids[i] = commitBody(b, a, body)
		s.add(ids[i])
	}

	return ids
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
