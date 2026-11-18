package lifecycle

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync/atomic"
)

// ReadyMarker is the line testcontainers waits for in the container's output.
const ReadyMarker = "Ready."

// Process is the lifecycle of the running gostack process. The zero value runs
// no hooks and prints to stdout.
type Process struct {
	// Hooks holds the init scripts. It may be nil.
	Hooks *Hooks
	// Out receives the Ready. marker. It defaults to os.Stdout.
	Out io.Writer

	ready atomic.Bool
}

// Boot runs the boot hooks. Call it before the gateway starts.
func (p *Process) Boot(ctx context.Context) {
	p.run(ctx, StageBoot)
}

// Start runs the start hooks.
func (p *Process) Start(ctx context.Context) {
	p.run(ctx, StageStart)
}

// Ready runs the ready hooks, prints "Ready." and from then on reports
// ready. Call it once the gateway serves. The marker is printed even when a hook
// failed, as in LocalStack.
func (p *Process) Ready(ctx context.Context) {
	p.run(ctx, StageReady)
	out := p.Out
	if out == nil {
		out = os.Stdout
	}
	_, _ = fmt.Fprintln(out, ReadyMarker)
	p.ready.Store(true)
}

// Shutdown runs the shutdown hooks and reports not ready.
func (p *Process) Shutdown(ctx context.Context) {
	p.ready.Store(false)
	p.run(ctx, StageShutdown)
}

// IsReady reports whether the ready hooks have finished and the process has not
// started to shut down.
func (p *Process) IsReady() bool {
	return p.ready.Load()
}

func (p *Process) run(ctx context.Context, stage Stage) {
	if p.Hooks != nil {
		p.Hooks.Run(ctx, stage)
	}
}
