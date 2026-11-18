package persist_test

import (
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/persist"
)

func BenchmarkMarkDirty(b *testing.B) {
	cfg := baseCfg(b.TempDir())
	cfg.Frequency = time.Hour

	e := persist.New(cfg)
	e.Register(sqs, &fake{})
	e.MarkDirty(sqs)

	b.Cleanup(func() { _ = e.Close(b.Context()) })

	for b.Loop() {
		e.MarkDirty(sqs)
	}
}
