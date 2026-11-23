//go:build darwin

package persist

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
)

// syncBenchCounts is the axis of the sync benchmarks: pending assets in one
// save. 3,000 is where the per-file F_FULLFSYNC of SQ-424 ran a 4.5 s shutdown
// save out of time. These benchmarks build on darwin only, so the gate (linux)
// never runs them and docs/standards.md, "Benchmarks", does not bind them: on a
// darwin/arm64 laptop a sync of 100 took 12.9 ms, of 1,000 52 ms and of 3,000
// 125 ms.
var syncBenchCounts = []int{100, 1000, 3000}

// syncBenchAssets returns assets with n committed 1 KiB files, their ids and
// their paths. It is rewritten before each timed sync by dirty, so the sync has
// data to write back, as a save does.
func syncBenchAssets(b *testing.B, n int) (a *Assets, ids map[string]struct{}, paths []string) {
	b.Helper()

	a, err := newAssets(b.TempDir(), "bench", slog.New(slog.DiscardHandler))
	if err != nil {
		b.Fatal(err)
	}

	b.Cleanup(a.close)

	body := bytes.Repeat([]byte{'x'}, 1<<10)
	ids = make(map[string]struct{}, n)

	for range n {
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

		path, _ := assetPath(id)
		ids[id] = struct{}{}
		paths = append(paths, path)
	}

	return a, ids, paths
}

// dirty overwrites every file of paths, so the next sync has pages to write.
func dirty(b *testing.B, a *Assets, paths []string) {
	b.Helper()

	body := bytes.Repeat([]byte{'y'}, 1<<10)

	for _, path := range paths {
		f, err := a.root.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			b.Fatal(err)
		}

		_, err = io.Copy(f, bytes.NewReader(body))
		if err != nil {
			b.Fatal(err)
		}

		err = f.Close()
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSyncPaths measures the fsync of that many freshly written assets,
// 8 at a time: plain fsync(2) on macOS, with no drive flush, which a save adds
// once after them (BenchmarkSyncPending). The writes are not timed.
func BenchmarkSyncPaths(b *testing.B) {
	for _, n := range syncBenchCounts {
		b.Run(fmt.Sprintf("files=%d", n), func(b *testing.B) {
			a, _, paths := syncBenchAssets(b, n)

			for b.Loop() {
				b.StopTimer()
				dirty(b, a, paths)
				b.StartTimer()

				_, err := a.syncPaths(b.Context(), paths)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSyncPending measures the whole sync of a save with that many pending
// assets: an fsync of each, of each of their directories, and the one drive
// flush. The writes are not timed.
func BenchmarkSyncPending(b *testing.B) {
	for _, n := range syncBenchCounts {
		b.Run(fmt.Sprintf("files=%d", n), func(b *testing.B) {
			a, ids, paths := syncBenchAssets(b, n)

			for b.Loop() {
				b.StopTimer()
				dirty(b, a, paths)
				b.StartTimer()

				err := a.syncPending(b.Context(), ids)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
