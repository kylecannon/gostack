package persist

import (
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
	"runtime/debug"
	"slices"
	"strings"
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

// ErrUnsafe is wrapped by the error Flush and Load return for a service whose
// state file is unsafe to touch: a link that leaves the base dir, or a file that
// failed to load and could not be moved aside. The service is neither loaded
// nor saved until gostack restarts.
var ErrUnsafe = errors.New("saving and loading are off for the service until restart")

// Engine tracks which services changed and writes them to disk. It is safe for
// concurrent use. Every file it reads or writes is opened through an os.Root on
// the base dir, so no path, link or temp file takes it outside.
type Engine struct {
	dir     string
	freq    time.Duration
	formats []Format
	// loadFormats is the order Load tries the formats in.
	loadFormats []Format
	cfg         Config
	log         *slog.Logger

	// sem is held by whatever reads or writes the state files: one flush or one
	// load. A flush during a load would snapshot a service that Restore has
	// emptied but not yet refilled. It is a channel, so a Flush, Load or Close
	// stops waiting when its context ends.
	sem chan struct{}

	// openMu is held while a service's assets are opened, so they are opened
	// once. It is taken before mu.
	openMu sync.Mutex

	mu       sync.Mutex
	services map[string]*tracked
	// timer is the armed flush timer, from MarkDirty or the end of a flush.
	timer *time.Timer
	// gen identifies the armed timer, so a callback that lost a race with disarm
	// knows it is stale.
	gen uint64
	// running is true while the timer's callback runs. It is the one goroutine
	// the engine starts, and no timer is armed while it runs.
	running bool
	// flushing is true while a flush, by timer or by hand, holds the services it
	// took.
	flushing bool
	// dirtyAgain is true when a service is dirty that no flush has taken: a change
	// that came during a flush, or a service whose flush failed. The flush that
	// ends next schedules the one after.
	dirtyAgain bool
	// retryAt is when a failed flush may be tried again: the backoff after it
	// ended. It is zero after a flush that saved what it took.
	retryAt time.Time
	// failures counts the flushes that failed in a row, and lastFailure is the
	// error of the last one, so a failing disk is logged when something changes
	// rather than at every retry.
	failures    int
	lastFailure string
	closed      bool
}

// outcome is what one flush did to the run of failed flushes.
type outcome struct {
	// failures is the number of flushes failed in a row, this one included. It is
	// zero when the flush saved what it took.
	failures int
	// changed is true when this failure's error is not the last one's.
	changed bool
	// recovered is the number of failures in a row this flush ended.
	recovered int
}

type tracked struct {
	snap  Snapshotter
	dirty bool
	// unsafe is why the service's state file is unsafe to touch, or nil. The
	// service is then never loaded or written again, because a write could reach
	// outside the base dir or replace the only copy of what the file holds.
	unsafe error
	// assets are the service's assets once opened, or nil.
	assets *Assets
}

type pendingService struct {
	name   string
	snap   Snapshotter
	assets *Assets
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
		sem:         make(chan struct{}, 1),
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
// is false for a service whose state file is unsafe to touch, because that
// service is never loaded or written.
func (e *Engine) Enabled(service string) bool {
	if e.dir == "" || !e.cfg.serviceEnabled(service) {
		return false
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	t, registered := e.services[service]

	return !registered || t.unsafe == nil
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

// Assets returns the assets of the service named by service, and opens
// <base>/<service>/assets/.gostack on the first call; later calls return the
// same Assets, which stay open for the life of the process. It returns nil when
// the service's state is not persisted, as Enabled reports, so a nil Assets
// goes with health's persisted false. The service must be registered, and its
// Snapshotter must be a [Referencer]. When the directory cannot be opened, for
// a link that leaves the base dir as for a state file, or because it cannot be
// made, the service is neither loaded nor saved and the error wraps
// [ErrUnsafe].
func (e *Engine) Assets(service string) (*Assets, error) {
	if !e.Enabled(service) {
		return nil, nil //nolint:nilnil // a nil Assets is the answer for a service that is not persisted
	}

	e.mu.Lock()
	t, registered := e.services[service]
	e.mu.Unlock()

	if !registered {
		return nil, fmt.Errorf("assets of %s: %w", service, errNotRegistered)
	}

	if _, ok := t.snap.(Referencer); !ok {
		return nil, fmt.Errorf("assets of %s: %w", service, errNoReferencer)
	}

	return e.openAssets(service, t)
}

// ResetAssets runs wipe, which empties the state of the service named by
// service, and then removes every asset the service held before wipe ran, after
// the next commit of its snapshot, as Remove does. An asset committed after the
// listing stays, so the service has to make a write that began before wipe fail
// and Remove its asset itself. It holds the guard Load and Flush hold, waiting
// for it only until ctx ends, so a reset never falls between a load's restore
// and its sweep. A service whose assets the engine does not keep is wiped and
// nothing else.
func (e *Engine) ResetAssets(ctx context.Context, service string, wipe func()) error {
	a := e.resettable(service)
	if a == nil {
		wipe()

		return nil
	}

	err := e.acquire(ctx)
	if err != nil {
		return fmt.Errorf("reset %s: %w", service, err)
	}

	defer e.release()

	ids, err := a.list()
	if err != nil {
		e.log.Error("persist: list the assets to reset; the service removes the ones it names",
			"service", service, "dir", a.dir, "error", err)
	}

	wipe()
	a.requeue(ids)

	return nil
}

// MarkDirty records that a service changed. Nothing runs while no service is
// dirty: the first change arms the flush timer, and a change while a flush is
// pending or under way is left to that flush.
func (e *Engine) MarkDirty(service string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	t, ok := e.services[service]
	if !ok || t.unsafe != nil {
		return
	}

	t.dirty = true
	e.dirtyAgain = true
	e.scheduleLocked()
}

// Flush writes every dirty service. It clears a service's dirty bit before it
// takes the snapshot, and sets it again if the snapshot or the write fails, so
// a change made during the flush is kept for the next one. Flush attempts every
// dirty service and returns the errors joined, with an [ErrUnsafe] error for
// each service it does not save. It waits for a running flush or load only
// until ctx ends.
func (e *Engine) Flush(ctx context.Context) error {
	if e.dir == "" {
		return nil
	}

	err := e.acquire(ctx)
	if err != nil {
		return fmt.Errorf("flush: %w", err)
	}

	result, err := e.writeDirty(ctx)
	e.release()

	e.mu.Lock()
	e.scheduleLocked() //nolint:contextcheck // arms a timer, whose callback has no caller context
	e.mu.Unlock()

	// A failure is the caller's to report; the end of a run of them is logged here,
	// since the timer that logged the run will not see it.
	if result.recovered > 0 {
		e.logRecovered(result.recovered)
	}

	return errors.Join(err, e.unsafeErr())
}

// Load restores every registered service that has a state file. It tries the
// service's formats in the order they are configured, then any other format,
// and the first file that restores wins. A file that exists but cannot be read
// or restored is moved aside to <name>.unloadable-<UTC time> and logged, and the
// next format is tried; a service with no good file starts empty. If the file
// cannot be moved aside, or any of the service's state files is a link that
// leaves the base dir, the service is neither loaded nor saved, and Load
// returns an [ErrUnsafe] error naming it. Otherwise it returns an error only
// when ctx ends.
//
// Load runs while no flush does, so a flush never sees a service half restored.
func (e *Engine) Load(ctx context.Context) error {
	if e.dir == "" {
		return nil
	}

	err := e.acquire(ctx)
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}

	defer e.release()

	root, err := os.OpenRoot(e.dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			e.log.Error("persist: open base dir", "dir", e.dir, "error", err)
		}

		return e.unsafeErr()
	}

	defer func() { _ = root.Close() }()

	e.mu.Lock()
	safe := make(map[string]*tracked, len(e.services))

	for name, t := range e.services {
		if t.unsafe == nil {
			safe[name] = t
		}
	}

	e.mu.Unlock()

	for _, name := range slices.Sorted(maps.Keys(safe)) {
		err = ctx.Err()
		if err != nil {
			return fmt.Errorf("load: %w", err)
		}

		e.loadService(root, name, safe[name])
	}

	return e.unsafeErr()
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

// acquire waits for sem until ctx ends.
func (e *Engine) acquire(ctx context.Context) error {
	select {
	case e.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for the running flush or load: %w", ctx.Err())
	}
}

func (e *Engine) release() {
	<-e.sem
}

// scheduleLocked arms the flush timer when a service is dirty that no flush has
// taken, and no flush is armed, running or under way. The timer waits
// PERSIST_FREQUENCY, and at least until retryAt after a failed flush, so a
// failing disk is retried with a backoff even when PERSIST_FREQUENCY asks for no
// wait. A change while the timer is armed leaves it as it is.
func (e *Engine) scheduleLocked() {
	if !e.dirtyAgain || e.timer != nil || e.running || e.flushing || e.closed {
		return
	}

	e.gen++
	gen := e.gen
	e.timer = time.AfterFunc(max(e.freq, time.Until(e.retryAt)), func() { e.flushOnTimer(gen) })
}

// disarmLocked cancels the armed timer, if any.
func (e *Engine) disarmLocked() {
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}

	e.gen++
}

// flushOnTimer is the timer's callback. While it runs, from before it waits for
// sem to after it schedules the next flush, it is the only flush goroutine: a
// change in the meantime sets dirtyAgain and arms nothing.
func (e *Engine) flushOnTimer(gen uint64) {
	e.mu.Lock()
	if gen != e.gen {
		e.mu.Unlock()

		return
	}

	e.timer = nil
	e.running = true
	e.mu.Unlock()

	var (
		result outcome
		err    error
	)

	e.sem <- struct{}{} // a Load or a Flush by hand may hold it
	if e.due() {
		// The timer has no caller to take a context from, and a flush must run to
		// completion once the state is marked clean.
		result, err = e.writeDirty(context.Background())
	}
	e.release()

	e.mu.Lock()
	e.running = false
	e.scheduleLocked()
	e.mu.Unlock()

	e.logOutcome(result, err)
}

// logOutcome logs a flush by the timer. Of a run of failures it logs the first
// in full, then each failure whose error changed, then every tenth with the
// count, and then the save that ends the run. A refused service has been logged
// by writeAll.
func (e *Engine) logOutcome(result outcome, err error) {
	switch {
	case result.recovered > 0:
		e.logRecovered(result.recovered)
	case result.failures == 0:
	case result.failures == 1 || result.changed:
		e.log.Error("persist: flush failed; retrying with a backoff",
			"failures", result.failures, "error", err)
	case result.failures%10 == 0:
		e.log.Error("persist: flush still failing", "failures", result.failures, "error", err)
	default:
		// The same error again, logged at the next tenth failure.
	}
}

func (e *Engine) logRecovered(failures int) {
	e.log.Info(fmt.Sprintf("persist: save recovered after %d failures", failures))
}

// due reports whether the timer's flush may run: not after Close, which flushes
// by itself, and not before retryAt, which a Flush by hand that failed while
// the callback waited for sem may have moved.
func (e *Engine) due() bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	return !e.closed && !time.Now().Before(e.retryAt)
}

// writeDirty writes the dirty services. The caller holds sem.
func (e *Engine) writeDirty(ctx context.Context) (outcome, error) {
	pending := e.takeDirty()
	if len(pending) == 0 {
		return outcome{}, nil
	}

	failed, err := e.writeAll(ctx, pending)

	return e.finish(failed, err), err
}

// takeDirty clears the dirty bit of every dirty service and returns the safe
// ones in name order. It also cancels the armed timer, because the services it
// was armed for are now in the caller's hands.
func (e *Engine) takeDirty() []pendingService {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.disarmLocked()
	e.dirtyAgain = false

	var pending []pendingService

	for _, name := range slices.Sorted(maps.Keys(e.services)) {
		t := e.services[name]
		if !t.dirty {
			continue
		}

		t.dirty = false

		if t.unsafe == nil {
			pending = append(pending, pendingService{name: name, snap: t.snap, assets: t.assets})
		}
	}

	e.flushing = len(pending) > 0

	return pending
}

// finish ends a flush and returns what it did to the run of failures. The
// services that were not saved are dirty again, and the next flush waits for
// the backoff.
func (e *Engine) finish(failed []pendingService, err error) outcome {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.flushing = false

	if len(failed) == 0 {
		result := outcome{recovered: e.failures}
		e.failures, e.lastFailure, e.retryAt = 0, "", time.Time{}

		return result
	}

	e.failures++
	text := fmt.Sprint(err)
	result := outcome{failures: e.failures, changed: text != e.lastFailure}
	e.lastFailure = text
	e.retryAt = time.Now().Add(e.backoffLocked())

	for _, p := range failed {
		t := e.services[p.name]
		if t.unsafe == nil {
			t.dirty = true
			e.dirtyAgain = true
		}
	}

	return result
}

// backoffLocked is the wait after e.failures failed flushes in a row:
// max(retryDelay, PERSIST_FREQUENCY) after the first, doubled after each one
// that follows, up to max(maxRetryDelay, PERSIST_FREQUENCY).
func (e *Engine) backoffLocked() time.Duration {
	delay, limit := max(retryDelay, e.freq), max(maxRetryDelay, e.freq)

	for range e.failures - 1 {
		if delay >= limit {
			break
		}

		delay *= 2
	}

	return min(delay, limit)
}

// writeAll saves each pending service. It returns the services to try again,
// and the errors joined. A service whose file turns out to be a link that
// leaves the base dir is not tried again: saving is turned off for it.
//
// The assets a service removed before its snapshot is taken are deleted once
// that snapshot is on disk, and kept for the next save when it is not.
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
	)

	for i, p := range pending {
		err = ctx.Err()
		if err != nil {
			return append(failed, pending[i:]...), errors.Join(append(errs, err)...)
		}

		err = e.saveAssets(root, p)
		if err == nil {
			continue
		}

		errs = append(errs, fmt.Errorf("persist %s: %w", p.name, err))

		if leaves(root, err) {
			e.markUnsafe(p.name, err)
			e.log.Error("persist: a state file is a link that leaves the base dir or is absolute, "+
				"so the service is no longer saved; use a relative link inside the base dir",
				"service", p.name, "error", err)

			continue
		}

		failed = append(failed, p)
	}

	return failed, errors.Join(errs...)
}

// saveAssets saves one service between begin and finish of its assets: the
// removals queued before the snapshot is taken go once it is on disk, and the
// assets committed while it is written are not deleted at their Remove, because
// the snapshot may name them.
func (e *Engine) saveAssets(root *os.Root, p pendingService) error {
	removed := p.assets.begin()
	err := e.save(root, p)

	ref, _ := p.snap.(Referencer)
	p.assets.finish(removed, err, ref)

	return err
}

// save writes the state files of one service, removes the ones of formats that
// are no longer listed, and fsyncs the directories it changed.
func (e *Engine) save(root *os.Root, p pendingService) error {
	err := root.MkdirAll(p.name, dirMode)
	if err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	dirs, err := e.writeFormats(root, p)
	if err != nil {
		return err
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

	for _, dir := range dirs {
		err = syncDir(root, dir)
		if err != nil {
			return err
		}
	}

	return nil
}

// writeFormats snapshots one service straight into a temp file for each listed
// format, then renames them over the state files. It returns the directories it
// renamed in, the service's own first. A failed Snapshot or write removes the
// temp files and leaves the state files as they were.
func (e *Engine) writeFormats(root *os.Root, p pendingService) (dirs []string, err error) {
	writers := make([]*stateWriter, 0, len(e.formats))

	defer func() {
		if err != nil {
			for _, w := range writers {
				w.discard()
			}
		}
	}()

	outs := make([]io.Writer, 0, len(e.formats))

	for _, f := range e.formats {
		var w *stateWriter

		w, err = createState(root, p.name+"/"+f.fileName())
		if err != nil {
			return nil, err
		}

		writers = append(writers, w)
		outs = append(outs, w.buf)
	}

	err = snapshot(p, io.MultiWriter(outs...))
	if err != nil {
		return nil, err
	}

	for _, w := range writers {
		err = w.finish()
		if err != nil {
			return nil, err
		}
	}

	dirs = []string{p.name}

	for i, w := range writers {
		err = w.commit()
		if err != nil {
			writers = writers[i:] // the ones before are renamed, so not discarded

			return nil, err
		}

		err = removeTemps(root, dirOf(w.target), baseOf(w.target))
		if err != nil {
			return nil, err
		}

		if !slices.Contains(dirs, dirOf(w.target)) {
			dirs = append(dirs, dirOf(w.target))
		}
	}

	return dirs, nil
}

// snapshot writes the snapshot of one service to w, then fsyncs the assets
// committed since the last save. Every asset the snapshot names is committed
// by then, so it is on disk before the snapshot is.
func snapshot(p pendingService, w io.Writer) error {
	err := p.snap.Snapshot(w)
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}

	return p.assets.sync()
}

// loaded is what loading one service's state files did.
type loaded struct {
	// restored is true when a state file restored.
	restored bool
	// setAside is true when a state file failed and was moved aside.
	setAside bool
}

// loadService restores the service from the first state file that loads, then
// sweeps its assets when it keeps any. A missing file is skipped; a file that
// exists and fails is moved aside before the next format is tried. Before any
// file is read, every format's path and the assets directory are checked, so a
// link that leaves the base dir refuses the service whichever path it is.
func (e *Engine) loadService(root *os.Root, name string, t *tracked) {
	for _, f := range e.loadFormats {
		file := name + "/" + f.fileName()

		_, err := root.Stat(file)
		if err != nil && leaves(root, err) {
			e.refuse(root, name, file, err)

			return
		}
	}

	ref, keepsAssets := t.snap.(Referencer)
	if !keepsAssets {
		e.restore(root, name, t.snap)

		return
	}

	assets, err := e.openAssets(name, t)
	if err != nil {
		e.log.Error("persist: the assets directory could not be opened, so the service is neither loaded nor saved",
			"service", name, "error", err)

		return
	}

	result := e.restore(root, name, t.snap)

	loaded := make(map[string]struct{})
	for id := range ref.Restored() {
		loaded[id] = struct{}{}
	}

	e.sweepAssets(root, name, ref, assets, result, loaded)
}

// restore restores s from the first of the service's state files that loads.
func (e *Engine) restore(root *os.Root, name string, s Snapshotter) loaded {
	var result loaded

	for _, f := range e.loadFormats {
		file := name + "/" + f.fileName()

		found, err := restoreFile(root, file, s)
		if !found {
			continue
		}

		if err == nil {
			result.restored = true

			return result
		}

		e.setAside(root, name, file, err)
		result.setAside = true
	}

	return result
}

// restoreFile restores s from file. It reports false when the file does not
// exist, which is no failure. A panic in Restore is a failure like any other,
// with the stack in the error, so the file is moved aside and gostack still
// starts.
func restoreFile(root *os.Root, file string, s Snapshotter) (found bool, err error) {
	r, err := root.Open(file)
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist), fmt.Errorf("open %s: %w", file, err)
	}

	defer func() { _ = r.Close() }()

	defer func() {
		p := recover()
		if p != nil {
			found, err = true, fmt.Errorf("restore %s: panic: %v\n%s", file, p, debug.Stack())
		}
	}()

	err = s.Restore(r)
	if err != nil {
		return true, fmt.Errorf("restore %s: %w", file, err)
	}

	return true, nil
}

// sweepAssets removes what a crash left in the assets of a service whose state
// has just loaded: temp files no writer holds, and committed assets that
// neither its live state nor loadedIDs, the file it loaded, names, except the
// ones this process committed, which a request may be about to name. The load
// holds the guard a save holds, so the file loaded is the one on disk. Queued
// removals of assets the restored state names again are dropped. Every asset is
// kept while a state file of the service sits moved aside, because that file
// may name them.
func (e *Engine) sweepAssets(
	root *os.Root, name string, ref Referencer, a *Assets, result loaded, loadedIDs map[string]struct{},
) {
	err := a.removeTemps()
	if err != nil {
		e.log.Error("persist: remove asset temp files", "service", name, "dir", a.dir, "error", err)
	}

	refs := make(map[string]struct{})
	for id := range ref.References() {
		refs[id] = struct{}{}
	}

	if result.restored {
		a.keep(refs)
	}

	if result.setAside || movedAside(root, name) {
		e.log.Warn("persist: assets are kept because a state file of the service was moved aside and may name them; "+
			"orphaned assets build up until the file is removed",
			"service", name, "dir", a.dir)

		return
	}

	removed, err := a.sweep(refs, loadedIDs)
	if err != nil {
		e.log.Error("persist: remove orphaned assets", "service", name, "dir", a.dir, "error", err)
	}

	if removed > 0 {
		e.log.Info("persist: removed assets that no snapshot names", "service", name, "count", removed)
	}
}

// openAssets returns the open Assets of a registered service, and opens them when
// they are not. Assets that cannot be opened turn the service off, whatever the
// cause, and the error wraps [ErrUnsafe]: a link that leaves the base dir, a
// link or a file where a directory belongs, or a directory that cannot be made,
// such as on a read-only base dir. A service saved without its assets would name
// bodies a restart loses.
func (e *Engine) openAssets(service string, t *tracked) (*Assets, error) {
	e.openMu.Lock()
	defer e.openMu.Unlock()

	e.mu.Lock()
	a := t.assets
	e.mu.Unlock()

	if a != nil {
		return a, nil
	}

	a, err := newAssets(e.dir, service, e.log)
	if err != nil {
		e.markUnsafe(service, err)

		return nil, fmt.Errorf("persist %s: %w: %w", service, ErrUnsafe, err)
	}

	e.mu.Lock()
	t.assets = a
	e.mu.Unlock()

	return a, nil
}

// resettable returns the assets ResetAssets removes for the service, or nil
// when the engine keeps none for it.
func (e *Engine) resettable(service string) *Assets {
	if !e.Enabled(service) {
		return nil
	}

	e.mu.Lock()
	t, registered := e.services[service]
	e.mu.Unlock()

	if !registered {
		return nil
	}

	if _, ok := t.snap.(Referencer); !ok {
		return nil
	}

	a, err := e.openAssets(service, t)
	if err != nil {
		e.log.Error("persist: open the assets to reset; the service removes the ones it names",
			"service", service, "error", err)

		return nil
	}

	return a
}

// movedAside reports whether the service's directory holds a state file moved
// aside, or cannot be read to tell.
func movedAside(root *os.Root, service string) bool {
	entries, err := readDir(root, service)
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}

	return slices.ContainsFunc(entries, func(entry fs.DirEntry) bool {
		return strings.Contains(entry.Name(), unloadableInfix)
	})
}

// refuse turns a service off whose state file is, or is reached through, a link
// that leaves the base dir.
func (e *Engine) refuse(root *os.Root, service, file string, cause error) {
	path := filepath.Join(e.dir, file)
	link, _ := root.Readlink(file)

	e.markUnsafe(service, fmt.Errorf("%s: %w", path, errLinkLeaves))
	e.log.Error("persist: a state file is a link that leaves the base dir or is absolute, "+
		"so the service is neither loaded nor saved; use a relative link inside the base dir",
		"service", service, "file", path, "link", link, "error", cause)
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

	e.markUnsafe(service, fmt.Errorf("%s failed to load and could not be moved aside: %w", path, err))
	e.log.Error("persist: state file failed to load and could not be moved aside, so saving is off for the service",
		"service", service, "file", path, "move_error", err, "error", cause)
}

// markUnsafe turns loading and saving off for the service, for reason.
func (e *Engine) markUnsafe(service string, reason error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	t, registered := e.services[service]
	if registered && t.unsafe == nil {
		t.unsafe = reason
		t.dirty = false
	}
}

// unsafeErr returns an [ErrUnsafe] error for each service that is neither
// loaded nor saved, naming it and why, or nil when there is none.
func (e *Engine) unsafeErr() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	var errs []error

	for _, name := range slices.Sorted(maps.Keys(e.services)) {
		reason := e.services[name].unsafe
		if reason != nil {
			errs = append(errs, fmt.Errorf("persist %s: %w: %w", name, ErrUnsafe, reason))
		}
	}

	return errors.Join(errs...)
}
