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

// BenchmarkSavePendingAssets measures one save of a service whose state names
// that many 1 KiB assets committed since the last save, all of which the save
// makes durable before its snapshot is renamed into place: one syncfs on
// Linux, an fsync of each asset and of each of their directories elsewhere.
// The commits are not timed.
func BenchmarkSavePendingAssets(b *testing.B) {
	body := bytes.Repeat([]byte{'x'}, 1<<10)

	for _, pending := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("pending=%d", pending), func(b *testing.B) {
			base := b.TempDir()
			run := 0

			for b.Loop() {
				b.StopTimer()

				run++
				e := benchEngine(filepath.Join(base, strconv.Itoa(run)), flushEvery)
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
		})
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
