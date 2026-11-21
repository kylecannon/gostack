package persist_test

import (
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/persist"
)

// benchEngine builds an engine from baseCfg and hands back only the pointer.
//
// A benchmark that held persist.Config itself, as a literal or as the argument
// of persist.New, would carry its size in its own stack frame. The frame of a
// benchmark closure is where the benchmark's hot loop sits in the stack, and on
// some CPUs (AMD Zen 3, Zen 4) the speed of a byte-at-a-time loop such as
// jsonwire.AppendQuote depends on that position: adding two fields to Config
// slowed BenchmarkSQSFlush by 11% with no change to any code it runs
// (SQ-455, SQ-460). Building the engine here, with no inlining, keeps Config out
// of every benchmark frame, so a field added to Config cannot move one.
//
//go:noinline
func benchEngine(dir string, frequency time.Duration) *persist.Engine {
	cfg := baseCfg(dir)
	cfg.Frequency = frequency

	return persist.New(cfg)
}

func BenchmarkMarkDirty(b *testing.B) {
	e := benchEngine(b.TempDir(), time.Hour)
	e.Register(sqs, &fake{})
	e.MarkDirty(sqs)

	b.Cleanup(func() { _ = e.Close(b.Context()) })

	for b.Loop() {
		e.MarkDirty(sqs)
	}
}
