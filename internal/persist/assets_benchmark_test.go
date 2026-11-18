package persist_test

import (
	"bytes"
	"io"
	"os"
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
	e := persist.New(baseCfg(dir))
	e.Register(shelfService, newShelf())

	a, err := e.Assets(shelfService)
	if err != nil {
		b.Fatal(err)
	}

	return a, dir
}

// BenchmarkAssetsCommit writes, fsyncs and renames one asset per iteration,
// which is what a persisted PutObject adds to the ephemeral store's cost. The
// file is unlinked after each one, so the disk does not fill.
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
