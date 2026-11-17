package persist

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"slices"
	"sync"
	"time"
)

// Snapshotter saves and restores the state of one service.
type Snapshotter interface {
	// Snapshot writes the state of the service to w.
	Snapshot(w io.Writer) error
	// Restore replaces the state of the service with the bytes in r. On error it
	// leaves the service empty.
	Restore(r io.Reader) error
}

// Engine tracks which services changed and writes them to disk. It is safe for
// concurrent use.
type Engine struct {
	dir     string
	freq    time.Duration
	formats []Format
	cfg     Config
	log     *slog.Logger

	// flushMu keeps two flushes from writing the same temp file at once.
	flushMu sync.Mutex

	mu       sync.Mutex
	services map[string]*tracked
	timer    *time.Timer
	// gen identifies the pending timer, so a callback that lost a race with
	// disarm or re-arm knows it is stale.
	gen    uint64
	closed bool
}

type tracked struct {
	snap  Snapshotter
	dirty bool
}

type pendingService struct {
	name string
	snap Snapshotter
}

// New returns an Engine for cfg. When cfg turns persistence off, the Engine is
// still valid and all of its methods do nothing.
func New(cfg Config) *Engine {
	e := &Engine{
		dir:     cfg.baseDir(),
		freq:    cmp.Or(max(cfg.Frequency, 0), DefaultFrequency),
		formats: cfg.listedFormats(),
		cfg:     cfg,
		log:     cmp.Or(cfg.Logger, slog.Default()),
	}
	e.cfg.Services = maps.Clone(cfg.Services)

	return e
}

// BaseDir returns the directory state is written to, or "" when persistence is
// off.
func (e *Engine) BaseDir() string {
	return e.dir
}

// Enabled reports whether state of the service named by name is persisted.
func (e *Engine) Enabled(service string) bool {
	return e.dir != "" && e.cfg.serviceEnabled(service)
}

// Register adds a service to the Engine. A service whose persistence is off is
// not tracked.
func (e *Engine) Register(service string, s Snapshotter) {
	if !e.Enabled(service) {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.services == nil {
		e.services = make(map[string]*tracked)
	}

	e.services[service] = &tracked{snap: s}
}

// MarkDirty records that a service changed. It arms the flush timer unless one
// is already pending. Nothing runs while no service is dirty.
func (e *Engine) MarkDirty(service string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.markDirtyLocked(service)
}

// Flush writes every dirty service. It clears a service's dirty bit before it
// takes the snapshot, and sets it again if the snapshot or the write fails, so
// a change made during the flush is kept for the next one. Flush attempts every
// dirty service and returns the errors joined.
func (e *Engine) Flush(ctx context.Context) error {
	if e.dir == "" {
		return nil
	}

	e.flushMu.Lock()
	defer e.flushMu.Unlock()

	pending := e.takeDirty()
	if len(pending) == 0 {
		return nil
	}

	failed, err := e.writeAll(ctx, pending)
	e.redirty(failed) //nolint:contextcheck // reached from the timer callback, which has no caller context

	return err
}

// Load restores every registered service that has a state file, preferring
// json. A service whose file cannot be read or restored is logged and left
// empty; Load does not fail for it. Load returns an error only when ctx ends.
func (e *Engine) Load(ctx context.Context) error {
	if e.dir == "" {
		return nil
	}

	root, err := os.OpenRoot(e.dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			e.log.Error("persist: open base dir", "dir", e.dir, "error", err)
		}

		return nil
	}

	defer func() { _ = root.Close() }()

	e.mu.Lock()
	snaps := make(map[string]Snapshotter, len(e.services))

	for name, t := range e.services {
		snaps[name] = t.snap
	}

	e.mu.Unlock()

	for _, name := range slices.Sorted(maps.Keys(snaps)) {
		err = ctx.Err()
		if err != nil {
			return fmt.Errorf("load: %w", err)
		}

		e.loadService(root, name, snaps[name])
	}

	return nil
}

// Close flushes every dirty service and stops the timer. A MarkDirty after
// Close records the change but arms no timer.
func (e *Engine) Close(ctx context.Context) error {
	if e.dir == "" {
		return nil
	}

	e.mu.Lock()
	e.closed = true
	e.disarmLocked()
	e.mu.Unlock()

	return e.Flush(ctx)
}

func (e *Engine) markDirtyLocked(service string) {
	t, ok := e.services[service]
	if !ok {
		return
	}

	t.dirty = true

	if e.timer == nil && !e.closed {
		e.gen++
		gen := e.gen
		e.timer = time.AfterFunc(e.freq, func() { e.flushOnTimer(gen) })
	}
}

// disarmLocked cancels the pending timer, if any.
func (e *Engine) disarmLocked() {
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}

	e.gen++
}

func (e *Engine) flushOnTimer(gen uint64) {
	e.mu.Lock()
	stale := gen != e.gen
	e.timer = nil
	e.mu.Unlock()

	if stale {
		return
	}

	// The timer has no caller to take a context from, and a flush must run to
	// completion once the state is marked clean.
	err := e.Flush(context.Background())
	if err != nil {
		e.log.Error("persist: flush failed", "error", err)
	}
}

// takeDirty clears the dirty bit of every dirty service and returns them in
// name order. It also cancels the timer, because the services it was armed for
// are now in the caller's hands.
func (e *Engine) takeDirty() []pendingService {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.disarmLocked()

	var pending []pendingService

	for _, name := range slices.Sorted(maps.Keys(e.services)) {
		if t := e.services[name]; t.dirty {
			t.dirty = false
			pending = append(pending, pendingService{name: name, snap: t.snap})
		}
	}

	return pending
}

// redirty marks the services dirty again, which also re-arms the timer.
func (e *Engine) redirty(pending []pendingService) {
	if len(pending) == 0 {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	for _, p := range pending {
		e.markDirtyLocked(p.name)
	}
}

// writeAll saves each pending service. It returns the services that were not
// saved, to be marked dirty again, and the errors joined.
func (e *Engine) writeAll(ctx context.Context, pending []pendingService) ([]pendingService, error) {
	err := os.MkdirAll(e.dir, dirMode)
	if err != nil {
		return pending, fmt.Errorf("create base dir: %w", err)
	}

	root, err := os.OpenRoot(e.dir)
	if err != nil {
		return pending, fmt.Errorf("open base dir: %w", err)
	}

	defer func() { _ = root.Close() }()

	var (
		failed []pendingService
		errs   []error
		buf    bytes.Buffer
	)

	for i, p := range pending {
		err = ctx.Err()
		if err != nil {
			return append(failed, pending[i:]...), errors.Join(append(errs, err)...)
		}

		buf.Reset()

		err = e.save(root, p, &buf)
		if err != nil {
			failed = append(failed, p)
			errs = append(errs, fmt.Errorf("persist %s: %w", p.name, err))
		}
	}

	return failed, errors.Join(errs...)
}

// save snapshots one service into buf and replaces its files.
func (e *Engine) save(root *os.Root, p pendingService, buf *bytes.Buffer) error {
	err := p.snap.Snapshot(buf)
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}

	err = root.MkdirAll(p.name, dirMode)
	if err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	for _, f := range e.formats {
		err = writeAtomic(root, p.name+"/"+f.fileName(), buf.Bytes())
		if err != nil {
			return err
		}
	}

	// Formats that are no longer listed go only after the listed ones are safe.
	for _, f := range loadOrder {
		if slices.Contains(e.formats, f) {
			continue
		}

		err = removeFile(root, p.name+"/"+f.fileName())
		if err != nil {
			return err
		}
	}

	return syncDir(root, p.name)
}

func (e *Engine) loadService(root *os.Root, name string, s Snapshotter) {
	for _, f := range loadOrder {
		file := name + "/" + f.fileName()

		r, err := root.Open(file)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}

		if err == nil {
			err = s.Restore(r)
			_ = r.Close()
		}

		if err != nil {
			e.log.Error("persist: restore failed, service starts empty",
				"service", name, "file", file, "error", err)
		}

		return
	}
}
