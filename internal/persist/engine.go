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
	"path/filepath"
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
	// loadFormats is the order Load tries the formats in.
	loadFormats []Format
	cfg         Config
	log         *slog.Logger

	// flushMu keeps a flush from overlapping another flush or a load. Two
	// flushes would write the same temp file, and a flush during a load would
	// snapshot a service that Restore has emptied but not yet refilled.
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
	// unsafe is set when the service's state file failed to load and could not
	// be moved aside. The service is never written again, because a write would
	// replace the only copy of what the file holds.
	unsafe bool
}

type pendingService struct {
	name string
	snap Snapshotter
}

// New returns an Engine for cfg. When cfg turns persistence off, the Engine is
// still valid and all of its methods do nothing.
func New(cfg Config) *Engine {
	e := &Engine{
		dir:         cfg.baseDir(),
		freq:        cfg.flushDelay(),
		formats:     cfg.listedFormats(),
		loadFormats: cfg.loadFormats(),
		cfg:         cfg,
		log:         cmp.Or(cfg.Logger, slog.Default()),
	}
	e.cfg.Services = maps.Clone(cfg.Services)

	return e
}

// BaseDir returns the directory state is written to, or "" when persistence is
// off.
func (e *Engine) BaseDir() string {
	return e.dir
}

// Enabled reports whether state of the service named by name is persisted. It
// is false for a service whose state file failed to load and could not be moved
// aside, because that service is never written.
func (e *Engine) Enabled(service string) bool {
	if e.dir == "" || !e.cfg.serviceEnabled(service) {
		return false
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	t, registered := e.services[service]

	return !registered || !t.unsafe
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

	e.markDirtyLocked(service, e.freq)
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

// Load restores every registered service that has a state file. It tries the
// service's formats in the order they are configured, then any other format,
// and the first file that restores wins. A file that exists but cannot be read
// or restored is moved aside to <name>.unloadable-<UTC time> and logged, and the
// next format is tried; a service with no good file starts empty. If the file
// cannot be moved aside, the service is never written, so that the next save
// does not replace it. Load does not fail for a bad file. It returns an error
// only when ctx ends.
//
// Load runs while no flush does, so a flush never sees a service half restored.
func (e *Engine) Load(ctx context.Context) error {
	if e.dir == "" {
		return nil
	}

	e.flushMu.Lock()
	defer e.flushMu.Unlock()

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
		if !t.unsafe {
			snaps[name] = t.snap
		}
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

// markDirtyLocked marks the service dirty and, unless a timer is pending, arms
// one that fires after delay.
func (e *Engine) markDirtyLocked(service string, delay time.Duration) {
	t, ok := e.services[service]
	if !ok || t.unsafe {
		return
	}

	t.dirty = true

	if e.timer == nil && !e.closed {
		e.gen++
		gen := e.gen
		e.timer = time.AfterFunc(delay, func() { e.flushOnTimer(gen) })
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
		t := e.services[name]
		if !t.dirty {
			continue
		}

		t.dirty = false

		if !t.unsafe {
			pending = append(pending, pendingService{name: name, snap: t.snap})
		}
	}

	return pending
}

// redirty marks the services dirty again, which also re-arms the timer. The
// timer waits at least retryDelay, so a flush that keeps failing is retried at
// that pace even when PERSIST_FREQUENCY asks for no wait.
func (e *Engine) redirty(pending []pendingService) {
	if len(pending) == 0 {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	for _, p := range pending {
		e.markDirtyLocked(p.name, max(e.freq, retryDelay))
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
		err = writeState(root, e.dir, p.name+"/"+f.fileName(), buf.Bytes())
		if err != nil {
			return err
		}
	}

	// Formats that are no longer listed go only after the listed ones are safe.
	for _, f := range knownFormats {
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

// loadService restores s from the first state file that loads. A missing file is
// skipped; a file that exists and fails is moved aside before the next format is
// tried.
func (e *Engine) loadService(root *os.Root, name string, s Snapshotter) {
	for _, f := range e.loadFormats {
		file := name + "/" + f.fileName()

		found, err := e.restoreFile(root, file, s)
		if !found {
			continue
		}

		if err == nil {
			return
		}

		e.setAside(root, name, file, err)
	}
}

// restoreFile restores s from file. It reports false when the file does not
// exist, which is no failure.
func (e *Engine) restoreFile(root *os.Root, file string, s Snapshotter) (bool, error) {
	loc, err := locate(root, e.dir, file)
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist), err
	}

	defer loc.close()

	r, err := loc.root.Open(loc.name)
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist), fmt.Errorf("open %s: %w", loc.name, err)
	}

	defer func() { _ = r.Close() }()

	err = s.Restore(r)
	if err != nil {
		return true, fmt.Errorf("restore %s: %w", loc.name, err)
	}

	return true, nil
}

// setAside moves a state file that failed to load out of the way, so a later
// save cannot replace it. When the file cannot be moved, it turns saving off for
// the service instead.
func (e *Engine) setAside(root *os.Root, service, file string, cause error) {
	path := filepath.Join(e.dir, file)

	moved, err := moveAside(root, file, time.Now())
	if err == nil {
		e.log.Error("persist: state file failed to load and was moved aside; the service starts without it",
			"service", service, "file", path, "moved_to", filepath.Join(e.dir, moved), "error", cause)

		return
	}

	e.mu.Lock()

	t, registered := e.services[service]
	if registered {
		t.unsafe = true
	}

	e.mu.Unlock()

	e.log.Error("persist: state file failed to load and could not be moved aside, so saving is off for the service",
		"service", service, "file", path, "move_error", err, "error", cause)
}
