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
// state file is unsafe to touch: a link that leaves the base dir, a file that
// failed to load and could not be moved aside, assets that could not be
// opened or whose sync failed, a Snapshot that panicked, or a base dir that
// exists and could not be opened at startup. The service is neither loaded nor
// saved until gostack restarts.
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
	// markBudget is the time the rounds of abandoned-save markers of one flush
	// share (see markAllowance). Tests replace it.
	markBudget time.Duration
	// markSync makes a marker and its directory durable, and markFlush, when not
	// nil, flushes the drive once after every marker of a round is synced (see
	// markerSync). Tests replace them.
	markSync, markFlush func(*os.File) error
	// slow watches the flushes whose ctx has no deadline but can end, and
	// watchAfter is how long one runs before it is watched (see slowWatch). Tests
	// replace watchAfter.
	slow       slowWatch
	watchAfter time.Duration

	// sem is held by whatever reads or writes the state files: one flush or one
	// load. A flush during a load would snapshot a service that Restore has
	// emptied but not yet refilled. It is a channel, so a Flush, Load or Close
	// stops waiting when its context ends. A flush by the timer holds it shared:
	// a Flush that finds it so joins it rather than wait, and saves beside it
	// the services whose save it has not begun (see Flush).
	sem chan struct{}
	// sharers counts the flushes that hold sem together, the timer's and the
	// Flush calls that joined it; it is zero while sem is free or held by one
	// caller alone. The last of them to leave releases sem, or keeps it when it
	// goes on to flush alone.
	sharers int
	// preempting counts the Load and ResetAssets calls waiting for sem. A Flush
	// that comes while one waits does not join the flushes sharing sem, so it
	// writes nothing ahead of it (see acquire).
	preempting int

	// openMu is held while a service's assets are opened, so they are opened
	// once. It is taken before mu.
	openMu sync.Mutex

	mu       sync.Mutex
	services map[string]*tracked
	// timer is the armed flush timer, from MarkDirty or the end of a flush, and
	// timerAt is when it fires, and backedOff is true when that is later than
	// PERSIST_FREQUENCY after it was armed, for a service's backoff.
	timer     *time.Timer
	timerAt   time.Time
	backedOff bool
	// gen identifies the armed timer, so a callback that lost a race with disarm
	// knows it is stale.
	gen uint64
	// running is true while the timer's callback runs. It is the one goroutine
	// the engine starts, and no timer is armed while it runs.
	running bool
	// timed cancels the flush the timer's callback runs, or waits to run. It is
	// nil while the callback does not run.
	timed *timedFlush
	// waiting counts the Flush, Load, Close and ResetAssets calls waiting for
	// sem. A timer callback that starts while one waits does not flush; see
	// acquire for what happens to a timed flush already running.
	waiting int
	// flushing counts the flushes, by timer or by hand, that hold services they
	// took. Two run at once only while sem is shared.
	flushing int
	// dirtyAgain is true when a service is dirty that no flush has taken: a change
	// that came during a flush, a service whose flush failed, or one a flush by
	// the timer left for its backoff. The flush that ends next schedules the one
	// after.
	dirtyAgain bool
	closed     bool
	// loaded is true once a Load has opened the base dir, or found it missing: a
	// later Load is not the startup one.
	loaded bool
}

// outcome is what one flush did to one service's run of failed saves.
type outcome struct {
	service string
	// err is the error the service's save failed with. It is nil when the
	// service saved.
	err error
	// failures is the number of saves of the service failed in a row, this one
	// included. It is zero when the service saved.
	failures int
	// changed is true when this failure's error is not the last one's.
	changed bool
	// recovered is the number of failures in a row this save ended.
	recovered int
}

type tracked struct {
	snap  Snapshotter
	dirty bool
	// owner is the flush that took the service, from when it takes it until its
	// save there has ended, renamed or not; nil while no flush holds it. started
	// is true once that save has begun (see flushRun.own). Until then a flush
	// beside the owner may move the service to itself; after, no other flush
	// takes it, nor its leader (see takeDirty).
	owner   *flushRun
	started bool
	// renamed is true once the owner's save has renamed the service's snapshot
	// into place, which is when the abandoned-save marker is no longer needed for
	// it (see unsavedLocked). It is meaningful while a flush holds the service.
	renamed bool
	// retryAt is when the service's failed save may be tried again by the timer:
	// the backoff after it ended. It is zero after a save. A service backs off
	// alone, so a failing one never holds back another's save.
	retryAt time.Time
	// failures counts the service's saves that failed in a row, and lastFailure
	// is the failureKey of the last one's error, so a failing disk is logged when
	// something changes rather than at every retry.
	failures    int
	lastFailure string
	// unsafe is why the service's state file is unsafe to touch, or nil. The
	// service is then never loaded or written again, because a write could reach
	// outside the base dir or replace the only copy of what the file holds.
	unsafe error
	// assets are the service's assets once opened, or nil.
	assets *Assets
	// took is how long the service's last Snapshot in this process took, or the
	// Restore of a later Load until its next Snapshot, or zero when it has
	// neither. A flush orders its saves by it; see saveOrder.
	took time.Duration
}

type pendingService struct {
	name   string
	snap   Snapshotter
	assets *Assets
	// took is the service's tracked took when the flush took it.
	took time.Duration
	// moved is true when the flush took the service from a flush by the timer
	// that held it (see takeDirty).
	moved bool
}

// unsaved is a service a flush did not save, and what it failed with.
type unsaved struct {
	pendingService

	err error
	// gaveWay is true when the save gave way to a cancellation, or to Close's
	// release of the assets: it failed at nothing a retry would fail at again.
	gaveWay bool
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
		markBudget:  markBudget,
		markSync:    markerSync,
		markFlush:   markerFlush,
		watchAfter:  markWatchAfter,
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
// not tracked. It panics, a wiring error, for a [Follower] that follows itself
// or would make a chain with a registered service: a follower whose leader
// follows a service, or one a registered service follows. A flush saves a
// follower only with a leader that follows no service.
func (e *Engine) Register(service string, s Snapshotter) {
	if follows(s) == service {
		panic(fmt.Sprintf("persist: %s follows itself", service))
	}

	if !e.Enabled(service) {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.services == nil {
		e.services = make(map[string]*tracked)
	}

	refuseChain(e.services, service, s)
	e.services[service] = &tracked{snap: s}
}

// refuseChain panics when registering s as service would make a chain of
// followers with one of the services registered.
func refuseChain(services map[string]*tracked, service string, s Snapshotter) {
	leader := follows(s)
	if leader == "" {
		return
	}

	if l, ok := services[leader]; ok && follows(l.snap) != "" {
		panic(fmt.Sprintf("persist: %s follows %s, which follows %s", service, leader, follows(l.snap)))
	}

	for name, t := range services {
		if name != service && follows(t.snap) == service {
			panic(fmt.Sprintf("persist: %s follows %s, which follows %s", name, service, leader))
		}
	}
}

// Assets returns the assets of the service named by service, and opens
// <base>/<service>/assets/.gostack on the first call; later calls return the
// same Assets, which stay open until Close. It returns nil when
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

	_, err := e.acquire(ctx, preempt)
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
// dirty service, a [Follower] only right after its leader's save (see
// takeDirty), and returns the errors joined, with an [ErrUnsafe] error for
// each service it does not save. It waits for a running flush or load only
// until ctx ends.
//
// A flush by the timer that is under way does not hold back the services whose
// save it has not begun: Flush saves them beside it at once, those it did not
// take and those it took and has not reached, and only then waits for it,
// lending it ctx's deadline (see acquire), to save what is still dirty. A
// timed save inside a long Snapshot or a stuck asset sync, neither of which
// can be cut short, so never costs a shutdown the services that save is not
// writing. A service whose save the timed flush has begun, and a [Leader]
// whose follower it holds, is saved after it, so no service is saved by two
// flushes at once and a follower is still saved only right after its leader.
//
// A Flush whose ctx ends at a deadline marks for the next start (see
// abandonedFile), when it comes and not when the Snapshot it is inside returns,
// the services it holds and has not saved, every service that is dirty, and the
// ones the flush by the timer holds, to which it lends the deadline then; it
// does not return before those markers are written. A ctx with no deadline that
// ends with a deadline's cause, as a [context.WithCancelCause] ctx can, is
// marked for the same way once the flush has run for [markWatchAfter], so
// within that long of its end, and at the flush's end before that: a Flush that
// is in time allocates nothing for it. It marks the same when it ends waiting for
// sem. A Flush that has a deadline marks as well, at it, the services another
// Flush holds and has not yet renamed, whatever that Flush's ctx: one with no
// deadline, or one that comes after this kill, would not mark them in time.
//
// A panic of a service's Snapshot is recovered: the save fails, and saving is
// off for the service until restart. A panic of its [Committer] or [Leader]
// passes through Flush to its caller, after the flush ended: the services it
// took and did not save are dirty again, each leader is told the flush is over,
// and the timer is armed.
func (e *Engine) Flush(ctx context.Context) error {
	if e.dir == "" {
		return nil
	}

	// The rounds of markers this call runs share one budget (see markAllowance),
	// which a ctx with no deadline does not make: it costs two allocations, and
	// such a flush has none to mark at (see hasDeadline and slowWatch).
	if hasDeadline(ctx) {
		ctx = withMarkAllowance(ctx)
	}

	shared, err := e.acquire(ctx, lend)
	if err != nil {
		// Not a single service was tried, and a deadline cost the changes of them all.
		e.markWaiting(ctx, err)

		return fmt.Errorf("flush: %w", err)
	}

	var results []outcome

	if shared {
		results, err = e.flushShared(ctx)
	} else {
		results, err = e.flushByHand(ctx)
	}

	// A failure is the caller's to report; the end of a run of them is logged here,
	// since the timer that logged the run will not see it.
	for _, result := range results {
		if result.recovered > 0 {
			e.logRecovered(result.service, result.recovered)
		}
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
// returns an [ErrUnsafe] error naming it; so is every service when the base
// dir exists and cannot be opened at the startup Load, the first one in this
// process to find it, while a missing one is a fresh start. A later Load that
// cannot open the base dir returns that error and changes nothing. Otherwise it
// returns an error only when ctx ends. Load also removes the temp
// files of a service's state files that a save a crash cut short left behind.
//
// Services load in name order, except that a [Follower] loads right after the
// service it follows: dynamodbstreams after dynamodb, whose restored tables
// its records go back into. A follower whose service failed to load still
// loads, and finds no table to take its records.
//
// Load runs while no flush does, so a flush never sees a service half restored.
func (e *Engine) Load(ctx context.Context) error {
	if e.dir == "" {
		return nil
	}

	_, err := e.acquire(ctx, preempt)
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}

	defer e.release()

	root, err := os.OpenRoot(e.dir)
	if err != nil {
		return e.openFailed(err)
	}

	defer func() { _ = root.Close() }()

	e.mu.Lock()
	startup := !e.loaded
	e.loaded = true
	safe := make(map[string]*tracked, len(e.services))

	for name, t := range e.services {
		if t.unsafe == nil {
			safe[name] = t
		}
	}

	e.mu.Unlock()

	names := afterLeaders(slices.Sorted(maps.Keys(safe)), func(name string) (string, string) {
		return name, follows(safe[name].snap)
	})

	for _, name := range names {
		err = ctx.Err()
		if err != nil {
			return fmt.Errorf("load: %w", err)
		}

		e.loadService(root, name, safe[name])
	}

	if startup {
		e.warnAbandoned(root, names)
	}

	e.drainRemovals(safe) //nolint:contextcheck // arms a timer, whose callback has no caller context

	return e.unsafeErr()
}

// Stop stops the timer and flushes every dirty service. A MarkDirty after Stop
// records the change but arms no timer. Flush still works after it, and so do
// the assets, for the last flush of a shutdown; Close releases them.
func (e *Engine) Stop(ctx context.Context) error {
	if e.dir == "" {
		return nil
	}

	e.mu.Lock()
	e.closed = true
	e.disarmLocked()
	e.mu.Unlock()

	return e.Flush(ctx)
}

// Close is Stop, and then releases what every service's assets hold open. A
// sync a save stopped waiting for keeps its descriptor until it returns. The
// assets fail every call after Close.
func (e *Engine) Close(ctx context.Context) error {
	// A panic through Stop, from a [Committer] or a [Leader], still releases them.
	defer e.closeAssets()

	return e.Stop(ctx)
}

// closeAssets releases what every service's assets hold open.
func (e *Engine) closeAssets() {
	e.mu.Lock()
	open := make([]*Assets, 0, len(e.services))

	for _, t := range e.services {
		if t.assets != nil {
			open = append(open, t.assets)
		}
	}
	e.mu.Unlock()

	for _, a := range open {
		a.close()
	}
}

// openFailed ends a Load that could not open the base dir. A missing base dir
// is a fresh start: the load has succeeded, with nothing to restore. One that
// exists and cannot be opened holds state this load did not read, and a save
// would replace it: at the startup Load, before any Load has succeeded in this
// process, that turns saving and loading off for every service. A later Load
// restored state this process holds in memory and saved, so a base dir that
// cannot be opened now is a fault that may pass: it returns its error and
// changes nothing.
func (e *Engine) openFailed(err error) error {
	e.mu.Lock()
	startup := !e.loaded
	e.loaded = e.loaded || errors.Is(err, fs.ErrNotExist)
	e.mu.Unlock()

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return e.unsafeErr()
	case startup:
		e.refuseAll(err)

		return e.unsafeErr()
	default:
		return fmt.Errorf("load: open the base dir, so nothing was loaded: %w", err)
	}
}

// flushByHand writes the dirty services of a flush by hand, which holds sem
// alone. The guard is released, and the timer armed for what is left dirty, by
// defer: a panic that passes through the flush, such as one in a [Committer],
// would otherwise hold the guard for ever, and no later flush or load,
// shutdown's included, could run. writeDirty has made the services the flush
// took dirty again by then.
func (e *Engine) flushByHand(ctx context.Context) ([]outcome, error) {
	defer e.armTimer() //nolint:contextcheck // arms a timer, whose callback has no caller context
	defer e.release()

	return e.writeDirty(ctx, byHand)
}

// flushShared writes the dirty services of a flush by hand that shares sem
// with a flush by the timer: it saves beside that flush first, then waits to
// hold sem alone (see unshare) and saves what is still dirty. The timer is
// armed by defer, as in flushByHand; flushBeside ends the share when a panic
// passes through it.
func (e *Engine) flushShared(ctx context.Context) ([]outcome, error) {
	defer e.armTimer() //nolint:contextcheck // arms a timer, whose callback has no caller context

	results, err := e.flushBeside(ctx)

	waitErr := e.unshare(ctx)
	if waitErr != nil {
		e.markWaiting(ctx, waitErr)

		return results, errors.Join(err, fmt.Errorf("flush: %w", waitErr))
	}

	// Out of time, a second pass would take every dirty service only to abandon it
	// at once, and count it failed for a deadline it never had a chance at.
	if ctx.Err() != nil {
		e.release()

		if err == nil && e.anyDirty() {
			err = fmt.Errorf("flush: %w", ctx.Err())
			e.markWaiting(ctx, err)
		}

		return results, err
	}

	more, moreErr := e.flushByHand(ctx)

	return append(results, more...), errors.Join(err, moreErr)
}

// anyDirty reports whether a service is dirty that a flush may save.
func (e *Engine) anyDirty() bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, t := range e.services {
		if t.dirty && t.unsafe == nil {
			return true
		}
	}

	return false
}

// armTimer arms the flush timer for what is dirty.
func (e *Engine) armTimer() {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.scheduleLocked()
}

// drainRemovals marks dirty each loaded service whose assets have removals
// queued, such as the bodies a Restore replaced: they go only after a save, and
// nothing else may be about to save. The timer it arms waits for the load.
func (e *Engine) drainRemovals(loaded map[string]*tracked) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, t := range loaded {
		if t.unsafe == nil && t.assets.queued() {
			t.dirty = true
			e.dirtyAgain = true
		}
	}

	e.scheduleLocked()
}

// timedSave is what a caller waiting for sem does to a flush by the timer that
// runs or waits to run.
type timedSave int

const (
	// preempt cancels it at once. Load and ResetAssets replace or discard the
	// state it would write, so finishing it only delays them, and a Load that
	// waited for it would restore what it wrote rather than the state last saved
	// before the Load was asked for.
	preempt timedSave = iota + 1 // the zero value is no mode, so never the destructive one
	// lend lets it finish under the caller's deadline: it is cancelled only when
	// the caller's ctx ends. A Snapshot cannot be cut short, so cancelling a
	// timed save at once throws away a Snapshot already taken and makes the
	// caller, such as a shutdown, take a second one. Lent the deadline, a timed
	// save that can finish in time does, and the caller then saves only what is
	// still dirty. Of several callers waiting, the first whose ctx ends cancels
	// it.
	lend
)

// errPreempted is the cause a Load or ResetAssets cancels a timed save with. It
// wraps context.Canceled, so the services the save did not reach gave way.
var errPreempted = fmt.Errorf("preempted by a load or reset: %w", context.Canceled)

// timedFlush cancels the context of one flush by the timer. preempt cancels it
// with errPreempted, and lend with the cause of the caller's ctx. They are two
// contexts, the preemptable one the parent, because a cause is set once: a
// preempt after a lent deadline ran out must still stop the rename.
type timedFlush struct {
	preempt context.CancelCauseFunc
	lend    context.CancelCauseFunc
}

// preemptKey is the context key under which a timed flush's context holds the
// cause of its preemptable parent, as a func() error.
type preemptKey struct{}

// newTimedFlush returns the context of a flush by the timer and what cancels it.
func newTimedFlush() (context.Context, *timedFlush) {
	parent, preemptFn := context.WithCancelCause(context.Background())
	cause := func() error { return context.Cause(parent) }
	ctx, lendFn := context.WithCancelCause(context.WithValue(parent, preemptKey{}, cause))

	return ctx, &timedFlush{preempt: preemptFn, lend: lendFn}
}

// stop cancels the context, once the flush has ended, to release it.
func (t *timedFlush) stop() {
	t.lend(nil)
	t.preempt(nil)
}

// preempted reports whether ctx is that of a flush by the timer that a Load or
// ResetAssets cancelled. Such a save renames nothing: the Load would restore
// what it wrote. Any other end of a save's context, its own deadline or a lent
// one, a caller that went away, leaves a snapshot that is taken, and whose
// syncs have returned clean, free to rename (US-2 log #49).
func preempted(ctx context.Context) bool {
	cause, _ := ctx.Value(preemptKey{}).(func() error)

	return cause != nil && errors.Is(cause(), errPreempted)
}

// acquire waits for sem until ctx ends, and does what mode says to a flush by
// the timer. A preempted timed flush fails before it renames anything and
// leaves its services dirty, with no backoff. A lent one that runs out of time
// renames the service whose snapshot and syncs are done, and leaves the rest
// dirty, as a save past its own deadline does. A sync of assets it started
// still runs, and the next save renames only once that sync has returned.
//
// A Flush (lend) that finds sem shared by a flush by the timer joins it at
// once and reports shared: it may then save the services whose save no other
// flush has begun (see takeDirty), and leaves with unshare. The join and a
// timed flush's start are decided under mu, so a Flush either joins or is a
// waiter the timed flush gives way to. It does not join while a Load or
// ResetAssets waits: it waits behind them, so a Load restores what was on disk
// when it was called, not what a Flush wrote after.
func (e *Engine) acquire(ctx context.Context, mode timedSave) (shared bool, err error) {
	e.mu.Lock()
	if mode == lend && e.sharers > 0 && e.preempting == 0 {
		e.sharers++
		e.mu.Unlock()

		return true, nil
	}

	e.waiting++

	if mode == preempt {
		e.preempting++

		if e.timed != nil {
			e.timed.preempt(errPreempted)
		}
	}
	e.mu.Unlock()

	err = e.wait(ctx)

	if mode == preempt {
		e.mu.Lock()
		e.preempting--
		e.mu.Unlock()
	}

	return false, err
}

// wait waits for sem until ctx ends, for a caller already counted in waiting,
// and lends a flush by the timer the cause of ctx's end.
func (e *Engine) wait(ctx context.Context) error {
	// The last waiter to stop waiting arms the timer that waiters hold back (see
	// scheduleLocked). It may hold sem by then, and the callback waits for it.
	defer func() { //nolint:contextcheck // arms a timer, whose callback has no caller context
		e.mu.Lock()
		e.waiting--
		e.scheduleLocked()
		e.mu.Unlock()
	}()

	select {
	case e.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		e.mu.Lock()
		if e.timed != nil {
			e.timed.lend(context.Cause(ctx))
		}
		e.mu.Unlock()

		return fmt.Errorf("wait for the running flush or load: %w", ctx.Err())
	}
}

// release frees sem, held by one caller alone.
func (e *Engine) release() {
	<-e.sem
}

// leave ends one sharer's hold of sem, and frees sem when it was the last.
func (e *Engine) leave() {
	e.mu.Lock()
	e.sharers--
	last := e.sharers == 0
	e.mu.Unlock()

	if last {
		e.release()
	}
}

// unshare ends a Flush's share of sem, and leaves it holding sem alone: at
// once when the flushes it shared sem with have ended, or once they do, as
// acquire waits for it, until ctx ends. It returns an error when the Flush does
// not hold sem.
func (e *Engine) unshare(ctx context.Context) error {
	e.mu.Lock()
	e.sharers--

	if e.sharers == 0 {
		e.mu.Unlock()

		return nil // the last sharer keeps sem
	}

	e.waiting++
	e.mu.Unlock()

	return e.wait(ctx)
}

// flushBeside writes, while sem is shared, the dirty services whose save no
// other flush has begun, those a flush by the timer took and has not reached
// among them. A panic that passes through it ends the share.
func (e *Engine) flushBeside(ctx context.Context) (results []outcome, err error) {
	ended := false

	defer func() {
		if !ended {
			e.leave()
		}
	}()

	results, err = e.writeDirty(ctx, byHand)
	ended = true

	return results, err
}

// scheduleLocked arms the flush timer when a service is dirty that no flush has
// taken, and no flush is armed, running or under way. The timer waits
// PERSIST_FREQUENCY from now, and for a service whose save failed until its own
// retryAt instead, so a failing disk is retried with a backoff even when
// PERSIST_FREQUENCY asks for no wait. That retryAt can come sooner than
// PERSIST_FREQUENCY from now, and once it has passed the service is due now.
// The timer fires for the earliest such service: a change to one that is due sooner than the armed timer brings the
// timer forward, so a service in backoff never delays another. A change that is
// due no sooner leaves the timer as it is. Nothing is armed while a Flush, Load,
// Close or ResetAssets waits for sem: the callback would give way to it at once
// and arm again, which with no wait is a spin. The last waiter arms it as it
// stops waiting.
func (e *Engine) scheduleLocked() {
	if !e.dirtyAgain || e.running || e.flushing > 0 || e.closed || e.waiting > 0 {
		return
	}

	// A timer armed for no later than PERSIST_FREQUENCY after its arming, which
	// a retryAt sooner than that makes it, fires no later than any service's
	// change could ask for, so MarkDirty's hot path stops here.
	if e.timer != nil && !e.backedOff {
		return
	}

	now := time.Now()
	at := e.nextDueLocked(now)

	if at.IsZero() { // a service turned unsafe took its dirty bit with it
		e.dirtyAgain = false

		return
	}

	if e.timer != nil {
		if !at.Before(e.timerAt) {
			return
		}

		e.timer.Stop()
	}

	e.gen++
	gen := e.gen
	e.timerAt = at
	e.backedOff = at.After(now.Add(e.freq))
	e.timer = time.AfterFunc(at.Sub(now), func() { e.flushOnTimer(gen) })
}

// nextDueLocked is when the timer should fire: the earliest, over the dirty
// services, of PERSIST_FREQUENCY from now or, for a service in its backoff, its
// retryAt. A retryAt is the service's own time, which another service's save
// does not move. Once it has passed, the service is due now: its timer ran past
// it while another service's save was under way, or gave way to a waiter. A
// [Follower] is saved only with its leader, so it is due no sooner than its
// leader would be, and never when its leader is not saved at all; otherwise a
// dirty follower whose leader is in its backoff would fire the timer for a
// flush that takes nothing, with no wait at PERSIST_FREQUENCY 0.
func (e *Engine) nextDueLocked(now time.Time) time.Time {
	var at time.Time

	for _, t := range e.services {
		if !t.dirty || t.unsafe != nil {
			continue
		}

		due := e.dueLocked(t, now)

		if leader := follows(t.snap); leader != "" {
			l, ok := e.services[leader]
			if !ok || l.unsafe != nil {
				continue
			}

			due = later(due, e.dueLocked(l, now))
		}

		if at.IsZero() || due.Before(at) {
			at = due
		}
	}

	return at
}

// dueLocked is when t's service would be due if it were dirty:
// PERSIST_FREQUENCY from now, or in its backoff its retryAt.
func (e *Engine) dueLocked(t *tracked, now time.Time) time.Time {
	if t.retryAt.IsZero() {
		return now.Add(e.freq)
	}

	return later(t.retryAt, now)
}

// later is the later of two times.
func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}

	return b
}

// disarmLocked cancels the armed timer, if any.
func (e *Engine) disarmLocked() {
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}

	e.timerAt = time.Time{}
	e.gen++
}

// flushOnTimer is the timer's callback. While it runs, from before it waits for
// sem to after it schedules the next flush, it is the only flush goroutine: a
// change in the meantime sets dirtyAgain and arms nothing. Its flush has no
// deadline of its own: a caller waiting for sem cancels it or lends it its
// deadline (see acquire), and one already waiting when the callback starts, or
// when it takes sem, makes it skip the flush. It holds sem shared, so a Flush
// that comes while it writes saves beside it what it has not begun to save.
func (e *Engine) flushOnTimer(gen uint64) {
	ctx, timed := newTimedFlush()
	defer timed.stop()

	e.mu.Lock()
	if gen != e.gen {
		e.mu.Unlock()

		return
	}

	e.timer = nil
	e.running = true
	e.timed = timed

	if e.waiting > 0 {
		timed.lend(context.Canceled)
	}
	e.mu.Unlock()

	var results []outcome

	select {
	case e.sem <- struct{}{}: // a Load or a Flush by hand may hold it
		if !e.share(ctx) {
			e.release()

			break
		}

		results = e.writeTimed(ctx)
	case <-ctx.Done():
	}

	e.mu.Lock()
	e.running = false
	e.timed = nil
	e.scheduleLocked()
	e.mu.Unlock()

	e.logOutcomes(results)
}

// writeTimed is the flush of the timer's callback, which holds sem as its
// first sharer (see share), and leaves it by defer, so a panic does not hold
// it. A panic that passes through it, from a [Committer] or a [Leader] (a
// Snapshot's is recovered where it is called), would end the process, since
// this goroutine is gostack's own: it is logged with its stack instead.
// writeDirty has made the services the flush took dirty again, and the
// callback arms the next flush.
func (e *Engine) writeTimed(ctx context.Context) (results []outcome) {
	defer e.leave()

	defer func() {
		r := recover()
		if r != nil {
			e.log.Error("persist: a flush panicked outside any Snapshot; the services it took not yet saved are dirty again",
				"panic", r, "stack", string(debug.Stack()))
		}
	}()

	results, _ = e.writeDirty(ctx, byTimer)

	return results
}

// logOutcomes logs a flush by the timer, for each service it did not save or
// ended the failures of. Of a service's run of failures it logs the first in
// full, then each failure whose error changed, then every tenth with the count,
// and then the save that ends the run. A refused service has been logged by
// writeAll.
func (e *Engine) logOutcomes(results []outcome) {
	for _, result := range results {
		switch {
		case result.recovered > 0:
			e.logRecovered(result.service, result.recovered)
		case result.failures == 0:
		case result.failures == 1 || result.changed:
			e.log.Error("persist: flush failed; retrying with a backoff",
				"service", result.service, "failures", result.failures, "error", result.err)
		case result.failures%10 == 0:
			e.log.Error("persist: flush still failing",
				"service", result.service, "failures", result.failures, "error", result.err)
		default:
			// The same error again, logged at the next tenth failure.
		}
	}
}

func (e *Engine) logRecovered(service string, failures int) {
	e.log.Info(fmt.Sprintf("persist: save recovered after %d failures", failures), "service", service)
}

// share reports whether the timer's flush, which has taken sem, may run, and
// makes it the first sharer of sem when it may. It may not after Stop, which
// flushes by itself, once ctx has ended, or while a caller waits for sem: one
// that began to wait after the callback started is given sem rather than kept
// waiting for the whole flush.
func (e *Engine) share(ctx context.Context) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed || ctx.Err() != nil || e.waiting > 0 {
		return false
	}

	e.sharers = 1

	return true
}

// flushBy is who asks for a flush.
type flushBy int

const (
	// byHand is a Flush, or the one that Stop and Close end with. It takes every
	// dirty service, so a failing service is still tried at shutdown.
	byHand flushBy = iota
	// byTimer is the flush timer's. It leaves a service in its backoff dirty.
	byTimer
)

// writeDirty writes the dirty services the flush by takes. The caller holds
// sem, alone or shared. A flush that runs out of time is logged
// once, at warn: the services it did not save keep their last state files, and
// their assets stay pending for the next save. A panic that passes through the
// flush, from a [Committer] or a [Leader], ends it as it passes (see
// flushRun.interrupt), and is not stopped here: its stack stays whole for
// whoever recovers it.
func (e *Engine) writeDirty(ctx context.Context, by flushBy) ([]outcome, error) {
	run := newFlushRun(e, by)

	pending := e.takeDirty(by, run)
	if len(pending) == 0 {
		return nil, nil
	}

	ended := false

	// close runs after interrupt, and whether or not interrupt itself panics.
	defer run.close()

	defer func() {
		if !ended {
			run.interrupt()
		}
	}()

	// The next start is told of what the deadline cuts when it comes, not when the
	// uninterruptible work the flush is in returns (see abandonedFile). It is
	// ended before the flush returns, and before interrupt settles the services.
	e.markAtDeadline(ctx, run)
	defer run.endMark()

	err := e.writeAll(ctx, run, pending)
	ended = true

	run.endMark()

	switch {
	case ranOutOfTime(ctx, err):
		e.logAbandoned(ctx, run)
	case run.by == byHand && ranOutOfTime(ctx, ctx.Err()):
		// The flush saved all it took, and the deadline came after, with a service
		// changed since: a change nothing will save.
		e.markUnsaved(ctx, run, nil)
	default:
		// In time, or a flush by the timer, whose deadline is lent to it.
	}

	return run.results, err
}

// logAbandoned logs the services of a flush that ran out of time and did not
// save, and marks for the next start the ones the deadline cut, with the ones
// that are dirty (see unsavedLocked). A service whose save failed for a reason
// of its own is named with the cause and not marked: a marker says a deadline
// cost its changes, and its warning advises a longer SHUTDOWN_TIMEOUT, which
// would not have saved it. A follower whose leader failed for a reason of its
// own is named so, with the leader's cause; one whose leader the deadline cut,
// or whose leader is not in the flush's failures, is cut with it.
func (e *Engine) logAbandoned(ctx context.Context, run *flushRun) {
	var (
		cut   []string
		other []string
	)

	e.mu.Lock()

	for _, u := range run.failed {
		switch {
		case !run.ownFaultLocked(ctx, u):
			cut = append(cut, u.name)
		case errors.Is(u.err, errLeaderUnsaved):
			leader := follows(u.snap)
			other = append(other, fmt.Sprintf("%s: %s (%s: %s)", u.name, u.err, leader, run.lost[leader].err))
		default:
			other = append(other, u.name+": "+u.err.Error())
		}
	}

	e.mu.Unlock()

	attrs := []any{"services", cut}
	if len(other) > 0 {
		attrs = append(attrs, "failed_for_other_reasons", other)
	}

	e.log.Warn("persist: save abandoned at its deadline; the state files on disk are the last saved", attrs...)

	// The process is likely on its way out, so the next start has to be told.
	e.markUnsaved(ctx, run, cut)
}

// ranOutOfTime reports whether err, a flush's error, holds the end of a
// deadline: the save's own, or one a waiter lent a timed save (see acquire).
// A lent deadline cancels the timed save, so err holds context.Canceled and
// the deadline is the context's cause. A preempt is no deadline, even after a
// lent one ran out. It only picks the log line: what counts as a failure is
// settle's.
func ranOutOfTime(ctx context.Context, err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	return errors.Is(err, context.Canceled) && !preempted(ctx) &&
		errors.Is(context.Cause(ctx), context.DeadlineExceeded)
}

// settleLocked ends one service's part in a flush by what its save failed
// with, u, or nil when it did not fail, never by the flush's context: a
// context can end after the service is saved, or beside a real failure. It
// runs as the service's save ends, not when the flush does, so the service is
// free for a flush beside this one at once. Each service is judged on its own.
// One that gave way to a cancellation, to another flush or a caller that went
// away, did not fail: it is dirty again, with no backoff. Any other unsaved
// service is a failure, counted with a backoff of its own, and a service that
// saved ends its run of failures. It returns what the save did to the
// service's run of failures, and false when it is nothing to report.
func (e *Engine) settleLocked(name string, u *unsaved) (outcome, bool) {
	t := e.services[name]
	t.owner, t.started = nil, false

	switch {
	case u == nil:
		recovered := t.failures
		t.failures, t.lastFailure, t.retryAt = 0, "", time.Time{}

		if t.unsafe == nil && recovered > 0 {
			return outcome{service: name, recovered: recovered}, true
		}

		return outcome{}, false
	case u.gaveWay:
		e.redirtyLocked(*u)

		return outcome{}, false
	default:
		return e.failLocked(t, *u), true
	}
}

// failLocked counts a failed save of t's service: the service is dirty again,
// and the timer waits for the backoff before it tries it. A failure is the same
// as the last when its failureKey is: the error text holds a temp file's random
// name, so comparing the text would log every retry of one failing disk.
func (e *Engine) failLocked(t *tracked, u unsaved) outcome {
	t.failures++
	text := failureKey(u.err)
	result := outcome{
		service: u.name, err: u.err, failures: t.failures, changed: text != t.lastFailure,
	}
	t.lastFailure = text
	t.retryAt = time.Now().Add(e.backoff(t.failures))
	e.redirtyLocked(u)

	return result
}

// redirtyLocked marks a service a flush did not save dirty again.
func (e *Engine) redirtyLocked(u unsaved) {
	t := e.services[u.name]
	if t.unsafe == nil {
		t.dirty = true
		e.dirtyAgain = true
	}
}

// takeDirty clears the dirty bit of each service it takes and returns the safe
// ones in name order; writeAll puts them in saveOrder. A flush byTimer leaves a
// service still in its backoff dirty, for the timer to arm again; one byHand
// takes it. A [Follower] is taken only with its leader, and then whether or
// not it is dirty, unless the flush is byTimer and the follower is in its own
// backoff: the leader is then taken alone. A follower whose leader is not
// taken stays dirty. A dirty follower that is due takes its clean leader with
// it, so a follower whose save failed after its leader's is saved again, with
// its leader, rather than never. It also cancels the armed timer, because the
// services it was armed for are now in the caller's hands.
//
// A service another flush holds, one whose save there has not ended, is not
// taken, and stays dirty; nor is the leader of a follower another flush
// holds, so a leader saved again never falls between its rename and its
// follower's snapshot in that flush. The exception, for a flush by hand, is a
// service whose save in a flush by the timer beside it has not begun, such as
// one queued behind another service's slow Snapshot: it is moved to run,
// which saves it instead, and the timed flush skips it (see flushRun.own). It
// never goes back, and one run does not reach before its deadline gave way (see
// flushRun.abandon). A [Leader] and its follower move together, or not at all:
// a follower moves only with its leader, and a leader whose follower may not
// move stays. Each service it takes is held, by run, until its save ends.
func (e *Engine) takeDirty(by flushBy, run *flushRun) []pendingService {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.disarmLocked()
	e.dirtyAgain = false

	now := time.Now()
	due := func(t *tracked) bool { return by == byHand || !now.Before(t.retryAt) }

	wanted := e.wantedLocked(due)
	held, movable := e.holdsLocked(by)

	var pending []pendingService

	// from is the flush each service taken was moved from, or nil.
	from := make(map[string]*flushRun, len(e.services))
	taken := make(map[string]bool, len(e.services))

	for _, name := range e.leadersFirstLocked() {
		t := e.services[name]

		var take bool

		switch leader := follows(t.snap); {
		case leader != "":
			// Taken only with its leader, and when another flush holds it, only when it
			// moves with its leader from that flush.
			take = taken[leader] && (t.owner == nil || t.owner == from[leader] && !t.started && t.unsafe == nil)
		case t.owner != nil:
			take = movable[name]
		default:
			take = wanted[name] && !held[name]
		}

		switch {
		case !take:
			e.dirtyAgain = e.dirtyAgain || t.dirty
		case t.unsafe != nil:
			t.dirty = false
		case !due(t):
			e.dirtyAgain = true
		default:
			from[name] = t.owner
			t.dirty, t.owner, t.started, t.renamed = false, run, false, false
			taken[name] = true
			run.holding[name] = pendingService{
				name: name, snap: t.snap, assets: t.assets, took: t.took, moved: from[name] != nil,
			}
			pending = append(pending, run.holding[name])
		}
	}

	slices.SortFunc(pending, func(a, b pendingService) int { return strings.Compare(a.name, b.name) })

	if len(pending) > 0 {
		e.flushing++
	}

	return pending
}

// wantedLocked is the dirty services and the leaders of the due dirty
// followers.
func (e *Engine) wantedLocked(due func(*tracked) bool) map[string]bool {
	wanted := make(map[string]bool, len(e.services))

	for name, t := range e.services {
		if t.dirty && t.unsafe == nil {
			wanted[name] = true

			if leader := follows(t.snap); leader != "" && due(t) {
				wanted[leader] = true
			}
		}
	}

	return wanted
}

// holdsLocked sorts what other flushes hold. held is the leaders no flush may
// take, because a follower another flush holds is saved there after them:
// that follower's save has begun, or its leader is not held with it, unsaved,
// by the same flush. movable is the services, none of them a follower, whose
// save in the flush by the timer that holds them has not begun, and that a
// flush by hand, by, may take from it: a timed flush never takes back what a
// Flush that joined it took first. The followers that move with them are
// decided by takeDirty.
func (e *Engine) holdsLocked(by flushBy) (held, movable map[string]bool) {
	held = make(map[string]bool, len(e.services))
	movable = make(map[string]bool, len(e.services))

	for _, t := range e.services {
		leader := follows(t.snap)
		if t.owner == nil || leader == "" {
			continue
		}

		l, ok := e.services[leader]
		if t.started || !ok || l.owner != t.owner || l.started {
			held[leader] = true
		}
	}

	for name, t := range e.services {
		if by == byHand && t.owner != nil && t.owner.by == byTimer && follows(t.snap) == "" && !t.started &&
			t.unsafe == nil && !held[name] {
			movable[name] = true
		}
	}

	return held, movable
}

// leadersFirstLocked is the service names, the ones that follow no service
// first, each part in name order. A follower's leader is not a follower itself.
func (e *Engine) leadersFirstLocked() []string {
	names := slices.Sorted(maps.Keys(e.services))
	rank := func(name string) int {
		if follows(e.services[name].snap) != "" {
			return 1
		}

		return 0
	}

	slices.SortStableFunc(names, func(a, b string) int { return cmp.Compare(rank(a), rank(b)) })

	return names
}

// backoff is the wait after a service's failures-th failed save in a row:
// max(retryDelay, PERSIST_FREQUENCY) after the first, doubled after each one
// that follows, up to max(maxRetryDelay, PERSIST_FREQUENCY).
func (e *Engine) backoff(failures int) time.Duration {
	delay, limit := max(retryDelay, e.freq), max(maxRetryDelay, e.freq)

	for range failures - 1 {
		if delay >= limit {
			break
		}

		delay *= 2
	}

	return min(delay, limit)
}

// writeAll saves each pending service, and returns the errors joined. Each
// service is settled as its save ends: run records what the ones to try again
// failed with, and whether they gave way to a cancellation.
// A service whose file turns out to be a link that leaves the base dir is not
// tried again: saving is turned off for it.
//
// It saves in two passes, each in saveOrder. The first takes every service's
// snapshot into temp files, and renames at once each one whose assets have
// nothing to sync, and no earlier sync to wait for, when its Snapshot has
// returned: services with no assets among them. It defers the others, with
// their temp files, to the second pass, which syncs their assets and renames
// them. A sync cannot be interrupted and can take the whole of the deadline,
// so it never runs before a save that needs none, even when a request commits
// an asset during the service's own Snapshot. saveOrder does the same for a
// slow Snapshot, which cannot be interrupted either. A deferred snapshot is renamed
// by the save that took it or not at all, so no older snapshot of a service
// replaces a newer one. A change after its Snapshot returned is for the next
// save; an asset committed between the Snapshot and the sync is synced with
// the rest, though the snapshot does not name it. One service's failure does
// not stop the services after it; only the end of ctx does, since no save can
// then finish. All services share ctx.
//
// The assets a service removed before its snapshot is taken are deleted once
// that snapshot is on disk, and kept for the next save when it is not.
//
// A service another flush moved away before its save here began is skipped
// (see flushRun.own). A panic that passes through it, from a [Committer] or a
// [Leader], is not stopped here; writeDirty ends the flush as it passes (see
// flushRun.close).
func (e *Engine) writeAll(ctx context.Context, run *flushRun, pending []pendingService) error {
	err := os.MkdirAll(e.dir, dirMode)
	if err != nil {
		return run.failAll(pending, fmt.Errorf("create base dir: %w", err))
	}

	root, err := os.OpenRoot(e.dir)
	if err != nil {
		return run.failAll(pending, fmt.Errorf("open base dir: %w", err))
	}

	defer func() { _ = root.Close() }()

	run.root = root
	deferred := run.firstPass(ctx, saveOrder(pending))
	run.secondPass(ctx, deferred)

	return errors.Join(run.errs...)
}

// failAll is every service of pending failing with err, before any is tried.
func (r *flushRun) failAll(pending []pendingService, err error) error {
	for _, p := range pending {
		r.lose(unsaved{pendingService: p, err: err})
	}

	return err
}

// saveOrder orders pending, which is in name order, for both passes of a
// flush, so that a service that can take the shared deadline goes after the
// ones that cannot. It sorts by, in turn:
//   - whether the service has assets to sync, or an earlier sync to wait for,
//     at flush start: those go after the ones with none, since their sync can
//     take the whole deadline;
//   - whether the service is one the flush took from a timed flush: those go
//     first, so a service the shutdown moved to save is never queued behind
//     one of the flush's own whose Snapshot was never measured, however slow
//     it is, and a measured one is no reason to hold it back: the timed flush
//     would have saved it before that service's;
//   - the service's last measured Snapshot in this process, or the Restore of
//     a later Load until its next Snapshot, shortest first, so a large S3 with
//     nothing pending, whose Snapshot grows with its objects, does not cost SQS
//     and STS their save; a service with neither counts as zero, so the first
//     flush of a process with no state to load is in name order;
//   - name.
//
// A [Follower] and the service it follows, when both are pending, are one
// unit: the unit sorts by the larger of its two measured times, and goes with
// the services that have assets to sync when either of the two has some, with
// the leader first and the follower right after it. That is a declared order
// rather than a name sort: dynamodbstreams is saved straight after dynamodb's
// rename (takeDirty takes it only with dynamodb), so the streams file holds
// the records of the cut the table file holds, and only a kill between the
// two, or a failed streams save, leaves it the older one. Ordering the pair by
// the leader's time alone would run a slow follower of a fast leader before
// the fast services its own time puts it behind.
//
// A service whose assets have failed or been released sorts with the pending
// ones; its save fails in firstPass, which reports why.
func saveOrder(pending []pendingService) []pendingService {
	present := make(map[string]bool, len(pending))
	for _, p := range pending {
		present[p.name] = true
	}

	// head is the name of the unit p is in: its leader when that is pending, or
	// p itself.
	head := func(p pendingService) string {
		if leader := follows(p.snap); leader != "" && present[leader] {
			return leader
		}

		return p.name
	}

	due := make(map[string]bool, len(pending))
	took := make(map[string]time.Duration, len(pending))
	// own is 1 for a unit with a service the flush did not take from a timed flush.
	own := make(map[string]int, len(pending))

	for _, p := range pending {
		d, err := p.assets.syncDue()
		h := head(p)
		due[h] = due[h] || d || err != nil
		took[h] = max(took[h], p.took)

		if !p.moved {
			own[h] = 1
		}
	}

	sorted := slices.SortedStableFunc(slices.Values(pending), func(a, b pendingService) int {
		ha, hb := head(a), head(b)

		if due[ha] != due[hb] {
			if due[hb] {
				return -1
			}

			return 1
		}

		return cmp.Or(
			cmp.Compare(own[ha], own[hb]), cmp.Compare(took[ha], took[hb]),
			strings.Compare(ha, hb), strings.Compare(a.name, b.name))
	})

	return afterLeaders(sorted, func(p pendingService) (string, string) { return p.name, follows(p.snap) })
}

// flushRun is what the saves of one flush failed with.
type flushRun struct {
	e    *Engine
	root *os.Root
	// by is who asked for the flush.
	by flushBy
	// endDeadline ends the marking armed for the ctx's deadline, and watching is
	// true while the flush is registered with e.slow. Only the goroutine of the
	// flush touches them. slow is what e.slow keeps of it, under its lock.
	endDeadline func()
	watching    bool
	slow        slowFlush

	failed []unsaved
	errs   []error
	// results is what each save that has ended did to its service's run of
	// failures, and holding the services taken whose save has not ended.
	results []outcome
	holding map[string]pendingService
	// lost is what each service the flush did not save failed with, by name. It
	// is written and read under mu, since the marking at a deadline (see
	// markAtDeadline) reads it while the flush runs. The first loss makes it, so
	// a flush that saves everything allocates none.
	lost map[string]unsaved
	// ended is true once the end of ctx is in errs.
	ended bool
	// committed are the services whose snapshot this flush renamed into place,
	// and deferred the ones it deferred to the second pass.
	committed, deferred map[string]bool
	// followed are the [Leader]s this flush told a follower is saved after
	// them, by name, told otherwise once the saves of both have ended.
	followed map[string]Leader
	// begun are the saves the flush began, and current is the service whose
	// save or leader call is under way: what a panic leaves to interrupt.
	begun   []*staged
	current string
	// interrupting is true once a panic ended the flush (see interrupt).
	interrupting bool
}

// newFlushRun returns the run of a flush by by, which takeDirty fills.
func newFlushRun(e *Engine, by flushBy) *flushRun {
	return &flushRun{
		e: e, by: by, committed: map[string]bool{}, deferred: map[string]bool{},
		holding: map[string]pendingService{}, followed: map[string]Leader{},
	}
}

// ownFaultLocked reports whether u, a service the flush did not save, was lost
// to a failure of its own and not to the deadline of ctx: a failure that is not
// the end of a deadline, or a [Follower] whose leader was lost to one. The
// caller holds mu.
func (r *flushRun) ownFaultLocked(ctx context.Context, u unsaved) bool {
	switch {
	case ranOutOfTime(ctx, u.err):
		return false
	case errors.Is(u.err, errLeaderUnsaved):
		leader, ok := r.lost[follows(u.snap)]

		return ok && r.ownFaultLocked(ctx, leader)
	default:
		return true
	}
}

// lostToOwnFaultLocked is ownFaultLocked for the service called name, false when
// the flush did not lose it, or when r is nil. The caller holds mu.
func (r *flushRun) lostToOwnFaultLocked(ctx context.Context, name string) bool {
	if r == nil {
		return false
	}

	u, ok := r.lost[name]

	return ok && r.ownFaultLocked(ctx, u)
}

// own begins the service's save in this flush, and reports whether the flush
// still holds it: a flush beside this one may have moved it away while its
// save here had not begun (see takeDirty), and then this flush skips it. Once
// own returns true the service stays with this flush until its save ends.
func (r *flushRun) own(name string) bool {
	e := r.e

	e.mu.Lock()
	defer e.mu.Unlock()

	t := e.services[name]
	if t.owner != r {
		delete(r.holding, name)

		return false
	}

	t.started = true

	return true
}

// settle ends the service's part in the flush, with u what its save failed
// with, or nil, and frees it for another flush (see settleLocked). It reports
// whether the flush still held the service: one moved away is another flush's
// to settle.
func (r *flushRun) settle(name string, u *unsaved) bool {
	p, ok := r.holding[name]
	if !ok {
		return false
	}

	r.unfollowUnit(p)

	e := r.e

	e.mu.Lock()
	defer e.mu.Unlock()

	delete(r.holding, name)

	if e.services[name].owner != r {
		return false
	}

	if u != nil {
		if r.lost == nil {
			r.lost = map[string]unsaved{}
		}

		r.lost[name] = *u
	}

	if result, ok := e.settleLocked(name, u); ok {
		r.results = append(r.results, result)
	}

	return true
}

// lose records a service the flush did not save, and settles it. It reports
// whether the flush still held the service (see settle).
func (r *flushRun) lose(u unsaved) bool {
	if !r.settle(u.name, &u) {
		return false
	}

	r.failed = append(r.failed, u)

	return true
}

// close ends the flush. A service still held, which nothing but a panic
// should leave and interrupt settles, is freed dirty again.
func (r *flushRun) close() {
	r.unfollow()

	e := r.e

	e.mu.Lock()
	defer e.mu.Unlock()

	for name := range r.holding {
		if t := e.services[name]; t.owner == r {
			t.owner, t.started, t.dirty = nil, false, t.unsafe == nil
			e.dirtyAgain = e.dirtyAgain || t.dirty
		}
	}

	r.holding = nil
	e.flushing--
}

// staged is one service's save between its snapshot and its rename.
type staged struct {
	p pendingService
	// removed is the batch of removals begin returned.
	removed []string
	// writers hold the snapshot in temp files that are not renamed yet.
	writers []*stateWriter
	// late is a follower whose leader was deferred: the second pass takes its
	// snapshot once the leader's is renamed.
	late bool
	// finished is true once finishSave has ended the save.
	finished bool
}

// begin starts the save of p, and records it for interrupt.
func (r *flushRun) begin(p pendingService, late bool) *staged {
	s := &staged{p: p, removed: p.assets.begin(), late: late}
	r.begun = append(r.begun, s)

	return s
}

// errPanicked is why a service the engine was saving when a panic passed
// through its flush is not saved.
var errPanicked = errors.New("a panic passed through its flush")

// interrupt ends a flush a panic passes through, from a [Committer] or a
// [Leader], which no recover here stops: the saves it began are finished, their
// temp files removed and their removals queued again, each leader is told the
// flush has ended, and every service it still holds is settled. A service
// whose snapshot was renamed is saved, its Committed's panic or not. The one
// under way when
// the panic began fails, with a backoff, so a panic every flush does not make
// the timer spin; the rest were taken and not written, so they gave way and are
// dirty again at once. It runs with the panic.
func (r *flushRun) interrupt() {
	r.interrupting = true

	for _, s := range r.begun {
		finishSave(s, errPanicked)
	}

	r.unfollow()

	for _, name := range slices.Sorted(maps.Keys(r.holding)) {
		if r.committed[name] { // its Committed panicked
			r.settle(name, nil)

			continue
		}

		r.lose(unsaved{pendingService: r.holding[name], err: errPanicked, gaveWay: name != r.current})
	}
}

// errLeaderUnsaved is why a [Follower] is not saved: its leader's save in the
// same flush did not end in a rename.
var errLeaderUnsaved = errors.New("its leader was not saved in this flush")

// firstPass snapshots each service and renames the ones with nothing to sync.
// It returns the services deferred to the second pass. A [Follower] is saved
// only after its leader's snapshot is renamed in the same flush: it goes to
// the second pass behind a deferred leader, and gives way, dirty again with no
// backoff, when its leader failed.
func (r *flushRun) firstPass(ctx context.Context, pending []pendingService) []*staged {
	var deferred []*staged

	followed := followedIn(pending)

	for i, p := range pending {
		if ctx.Err() != nil {
			r.abandon(ctx, pending[i:])

			break
		}

		// A flush beside this one took the service before its save here began.
		if !r.own(p.name) {
			continue
		}

		r.current = p.name

		if leader := follows(p.snap); leader != "" && !r.committed[leader] {
			if r.deferred[leader] {
				r.deferred[p.name] = true
				deferred = append(deferred, r.begin(p, true))
			} else {
				r.giveWay(p)
			}

			continue
		}

		if l, ok := p.snap.(Leader); ok {
			// Listed first: a Followed that panics has been told, and unfollow tells
			// it the flush has ended.
			if followed[p.name] {
				r.followed[p.name] = l
			}

			l.Followed(followed[p.name])
		}

		s := r.begin(p, false)

		err := r.e.snapshot(r.root, s)
		if err == nil {
			var due bool

			due, err = p.assets.syncDue()
			if err == nil && due {
				r.deferred[p.name] = true
				deferred = append(deferred, s)

				continue
			}
		}

		if err == nil {
			err = r.e.rename(ctx, r.root, s)
		}

		r.end(s, err)
	}

	return deferred
}

// followedIn is the services a [Follower] in pending follows: a flush saves
// that follower right after its leader.
func followedIn(pending []pendingService) map[string]bool {
	followed := make(map[string]bool, len(pending))

	for _, p := range pending {
		if leader := follows(p.snap); leader != "" {
			followed[leader] = true
		}
	}

	return followed
}

// secondPass syncs the assets of each deferred service and renames its
// snapshot. The ones ctx ends before are abandoned with their temp files.
func (r *flushRun) secondPass(ctx context.Context, deferred []*staged) {
	for i, s := range deferred {
		if ctx.Err() != nil {
			for _, rest := range deferred[i:] {
				finishSave(rest, ctx.Err())
				r.abandon(ctx, []pendingService{rest.p})
			}

			return
		}

		var err error

		r.current = s.p.name

		if s.late {
			if !r.committed[follows(s.p.snap)] {
				finishSave(s, errLeaderUnsaved)
				r.giveWay(s.p)

				continue
			}

			err = r.e.snapshot(r.root, s)
		}

		if err == nil {
			err = s.p.assets.sync(ctx)
		}

		if err == nil {
			err = r.e.rename(ctx, r.root, s)
		}

		r.end(s, err)
	}
}

// giveWay records a follower that is not saved because its leader was not: it
// failed at nothing of its own, so it is dirty again with no backoff.
func (r *flushRun) giveWay(p pendingService) {
	r.lose(unsaved{pendingService: p, err: errLeaderUnsaved, gaveWay: true})
}

// unfollowUnit tells the [Leader] of p's unit, when this flush told it a
// follower is saved after it, that none is, once p is the last of the unit
// this flush holds: whatever happened to either save, nothing it kept for that
// follower is needed past them. It is told before the last hold is freed, so a
// flush beside this one that takes the pair next is never told false between
// its leader's Snapshot and its follower's.
func (r *flushRun) unfollowUnit(p pendingService) {
	head := p.name
	if leader := follows(p.snap); leader != "" {
		head = leader
	}

	l, ok := r.followed[head]
	if !ok {
		return
	}

	for name, other := range r.holding {
		if name != p.name && (name == head || follows(other.snap) == head) {
			return
		}
	}

	delete(r.followed, head)
	r.tellUnfollowed(head, l)
}

// unfollow tells each [Leader] still told a follower is saved after it that
// none is, at the end of the flush. Every save that ends tells its unit's
// leader already (see unfollowUnit); this is for a flush a panic cut short.
func (r *flushRun) unfollow() {
	for head, l := range r.followed {
		delete(r.followed, head)
		r.tellUnfollowed(head, l)
	}
}

// tellUnfollowed calls Followed(false) of the leader called head. A leader
// whose Snapshot panicked is turned off, and the engine never calls it again:
// the panic may have left its lock held, and the call would wait for it. In a
// flush a panic is ending, a panic of the call is logged and not passed on,
// since it would stop the rest of the flush's cleanup: the services it still
// holds, the guard and the count of flushes.
func (r *flushRun) tellUnfollowed(head string, l Leader) {
	e := r.e

	e.mu.Lock()
	off := e.services[head] != nil && errors.Is(e.services[head].unsafe, errSnapshotPanicked)
	e.mu.Unlock()

	if off {
		return
	}

	if r.interrupting {
		defer func() {
			p := recover()
			if p != nil {
				e.log.Error("persist: Followed(false) of a leader panicked in a flush a panic ended; the leader is not told again",
					"service", head, "panic", p, "stack", string(debug.Stack()))
			}
		}()
	}

	l.Followed(false)
}

// end ends one service's save with err and records what it failed with. A
// save that ended in its rename is committed, and a [Committer] is told so.
func (r *flushRun) end(s *staged, err error) {
	finishSave(s, err)

	if err == nil {
		r.committed[s.p.name] = true

		if c, ok := s.p.snap.(Committer); ok {
			c.Committed()
		}

		r.settle(s.p.name, nil)

		return
	}

	p := s.p

	r.errs = append(r.errs, fmt.Errorf("persist %s: %w", p.name, err))

	if leaves(r.root, err) {
		r.e.markUnsafe(p.name, err)
		r.e.log.Error("persist: a state file is a link that leaves the base dir or is absolute, "+
			"so the service is no longer saved; use a relative link inside the base dir",
			"service", p.name, "error", err)
		r.settle(p.name, nil)

		return
	}

	// The store, or callSnapshot, has turned the service off and logged why; it
	// is not tried again.
	if errors.Is(err, errSyncFailed) || errors.Is(err, errSnapshotPanicked) {
		r.settle(p.name, nil)

		return
	}

	// A save that was cancelled, or that Close's release of the assets cut
	// short at exit, gave way: it failed at nothing a retry would fail at again.
	r.lose(unsaved{
		pendingService: p, err: err,
		gaveWay: errors.Is(err, context.Canceled) || errors.Is(err, errReleased),
	})
}

// abandon records the services the end of ctx stopped before they were saved,
// and the end of ctx in errs, once: not when a flush beside this one moved
// every one of them away, since this flush then lost nothing to it.
//
// A service this flush took from a flush by the timer and did not reach gave
// way: it was never tried, so it is dirty again with no failure and no backoff,
// as one a cancellation stopped is. The flush by the timer it came from cannot
// take it back: ownership moves from the timer to a flush by hand and never
// back, and a Flush that ends at its deadline lends that deadline to the timed
// flush (see unshare), which stops at the same moment.
func (r *flushRun) abandon(ctx context.Context, ps []pendingService) {
	lost := false

	for _, p := range ps {
		gaveWay := errors.Is(ctx.Err(), context.Canceled) || p.moved && !r.reached(p.name)
		lost = r.lose(unsaved{pendingService: p, err: ctx.Err(), gaveWay: gaveWay}) || lost
	}

	if lost && !r.ended {
		r.ended = true
		r.errs = append(r.errs, ctx.Err())
	}
}

// reached reports whether the flush has begun the service's save.
func (r *flushRun) reached(name string) bool {
	return slices.ContainsFunc(r.begun, func(s *staged) bool { return s.p.name == name })
}

// finishSave ends the save of s: a failed one removes its temp files, and the
// assets delete the removals of the batch once the snapshot is on disk, or
// queue them again when it is not. A save ends once: a second call does nothing.
func finishSave(s *staged, err error) {
	if s.finished {
		return
	}

	s.finished = true

	if err != nil {
		for _, w := range s.writers {
			w.discard()
		}

		s.writers = nil
	}

	ref, _ := s.p.snap.(Referencer)
	s.p.assets.finish(s.removed, err, ref)
}

// snapshot snapshots one service straight into a temp file for each listed
// format, written out and fsynced. A failed Snapshot or write leaves the state
// files as they were.
func (e *Engine) snapshot(root *os.Root, s *staged) error {
	p := s.p

	err := root.MkdirAll(p.name, dirMode)
	if err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	outs := make([]io.Writer, 0, len(e.formats))

	for _, f := range e.formats {
		var w *stateWriter

		w, err = createState(root, p.name+"/"+f.fileName())
		if err != nil {
			return err
		}

		s.writers = append(s.writers, w)
		outs = append(outs, w.buf)
	}

	start := time.Now()
	err = e.callSnapshot(p, io.MultiWriter(outs...))
	e.measured(p.name, time.Since(start))

	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}

	for _, w := range s.writers {
		err = w.finish()
		if err != nil {
			return err
		}
	}

	return nil
}

// errSnapshotPanicked is the error of a save whose Snapshot panicked, which
// turned saving off for its service.
var errSnapshotPanicked = errors.New("its Snapshot panicked")

// callSnapshot runs the Snapshot of p. A panic in it fails the save, which
// removes the temp files, and turns saving and loading off for the service for
// good: the panic may have left a lock of the service held, and a retry, or a
// Restore at a later Load, would wait for it inside the guard, so the engine
// never calls the service again. The panic is logged, with its stack, once.
// The services after it still save, so one service's bug neither takes the
// process down nor wedges the guard.
func (e *Engine) callSnapshot(p pendingService, w io.Writer) (err error) {
	defer func() {
		r := recover()
		if r != nil {
			err = fmt.Errorf("%w: %v", errSnapshotPanicked, r)

			e.markUnsafe(p.name, err)
			e.log.Error("persist: a Snapshot panicked, so saving and loading are off for the service until restart; "+
				"the state file on disk is the last one saved, and what changed since is lost at restart",
				"service", p.name, "panic", r, "stack", string(debug.Stack()))
		}
	}()

	return p.snap.Snapshot(w) //nolint:wrapcheck // snapshot adds the context
}

// measured records how long a Snapshot of service took, or how long its
// Restore did when a Load restored it, for saveOrder. A later Snapshot replaces
// a restore's figure.
func (e *Engine) measured(service string, took time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if t, ok := e.services[service]; ok {
		t.took = took
	}
}

// markRenamed records that the save of service has renamed its snapshot into
// place: from then a deadline marks nothing for it (see unsavedLocked), though
// the flush still holds it.
func (e *Engine) markRenamed(service string) {
	e.mu.Lock()
	e.services[service].renamed = true
	e.mu.Unlock()
}

// rename renames a service's snapshot over its state files, removes the ones of
// formats that are no longer listed, and fsyncs the directories it changed.
// Every asset the snapshot names is synced by then. A save a Load or
// ResetAssets preempted renames nothing: the Load would restore what it wrote.
func (e *Engine) rename(ctx context.Context, root *os.Root, s *staged) error {
	if preempted(ctx) {
		return fmt.Errorf("save cancelled before its rename: %w", errPreempted)
	}

	dirs := []string{s.p.name}

	// listed is true when a listing of the service's directory, which the sweep
	// of a writer's temp files reads after its rename, is the answer to whether
	// the service has a marker (see clearAbandoned); marked is that answer.
	var listed, marked bool

	for i, w := range s.writers {
		err := w.commit()
		if err != nil {
			s.writers = s.writers[i:] // the ones before are renamed, so not discarded

			return err
		}

		dir := dirOf(w.target)

		seen, err := sweepTemps(root, dir, baseOf(w.target), abandonedFile)
		if err != nil {
			s.writers = s.writers[i+1:]

			return err
		}

		if dir == s.p.name {
			listed, marked = true, marked || seen
		}

		if !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}

	s.writers = nil

	e.markRenamed(s.p.name)

	// A completed save ends the loss an abandoned one left (see abandonedFile).
	// A save with no listing of the directory looks for the marker itself.
	if marked || !listed {
		e.clearAbandoned(root, s.p.name)
	}

	// Formats that are no longer listed go only after the listed ones are safe.
	for _, f := range knownFormats {
		if slices.Contains(e.formats, f) {
			continue
		}

		err := removeFile(root, s.p.name+"/"+f.fileName())
		if err != nil {
			return err
		}
	}

	for _, dir := range dirs {
		err := syncDir(root, dir)
		if err != nil {
			return err
		}
	}

	return nil
}

// loaded is what loading one service's state files did.
type loaded struct {
	// restored is true when a state file restored.
	restored bool
	// setAside is true when a state file failed.
	setAside bool
	// unsafe is true when a state file failed and could not be moved aside,
	// which turned the service off.
	unsafe bool
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

	e.removeStateTemps(root, name)

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

	onDisk := make(map[string]struct{})
	for id := range ref.Restored() {
		onDisk[id] = struct{}{}
	}

	// A service the load turned off is neither restored nor swept: every asset
	// stays for the restart that loads it again.
	if result.unsafe {
		return
	}

	e.sweepAssets(root, name, ref, assets, result, onDisk)
}

// removeStateTemps deletes the temp files of the service's state files, in
// every format: a save killed between creating one and renaming it leaves it,
// and a later save removes only the temp files of the file it renamed. A state
// file that is a link is swept in two places: a save puts its temp file beside
// the file the links lead to (see target), and a build before that put it beside
// the link. Only temp names of the file's own name go, in either directory. The
// load holds the guard a save holds, so no writer owns one. A failure is logged
// and does not stop the load.
func (e *Engine) removeStateTemps(root *os.Root, name string) {
	for _, f := range e.loadFormats {
		file := name + "/" + f.fileName()
		files := []string{file}

		// A link that leaves the base dir has refused the service before this runs.
		path, err := target(root, file)
		if err == nil && path != file {
			files = append(files, path)
		}

		for _, swept := range files {
			err = removeTemps(root, dirOf(swept), baseOf(swept))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				e.log.Error("persist: remove state temp files", "service", name, "format", f.fileName(), "error", err)
			}
		}
	}
}

// restore restores s from the first of the service's state files that loads.
// A file that fails and cannot be moved aside turns the service off, and no
// later format is tried: the service starts empty, as an unsafe service does,
// rather than run a state the engine will not save.
func (e *Engine) restore(root *os.Root, name string, s Snapshotter) loaded {
	var result loaded

	for _, f := range e.loadFormats {
		file := name + "/" + f.fileName()

		start := time.Now()

		found, err := restoreFile(root, file, s)
		if !found {
			continue
		}

		if err == nil {
			// A service that restored slowly will snapshot slowly: its restore
			// time is its first timing, so saveOrder has a figure for it before
			// its first Snapshot.
			e.measured(name, time.Since(start))

			result.restored = true

			return result
		}

		result.setAside = true

		if !e.setAside(root, name, file, err) {
			result.unsafe = true

			return result
		}
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

	a.onFail = func(cause error) { e.syncFailed(service, a.dir, cause) }

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
// save cannot replace it, and reports whether it did. When the file cannot be
// moved, it turns loading and saving off for the service instead.
func (e *Engine) setAside(root *os.Root, service, file string, cause error) bool {
	path := filepath.Join(e.dir, file)

	moved, err := moveAside(root, file, time.Now())
	if err == nil {
		e.log.Error("persist: state file failed to load and was moved aside; the service starts without it",
			"service", service, "file", path, "moved_to", filepath.Join(e.dir, moved), "error", cause)

		return true
	}

	e.markUnsafe(service, fmt.Errorf("%s failed to load and could not be moved aside: %w", path, err))
	e.log.Error("persist: state file failed to load and could not be moved aside, "+
		"so the service starts empty and is neither loaded nor saved",
		"service", service, "file", path, "move_error", err, "error", cause)

	return false
}

// refuseAll turns loading and saving off for every registered service, whose
// state files a base dir that exists and cannot be opened hides from the startup
// load, and logs why once: a load that follows while the base dir still cannot
// be opened refuses no service that is not refused already, and logs nothing.
func (e *Engine) refuseAll(cause error) {
	e.mu.Lock()
	names := slices.Sorted(maps.Keys(e.services))
	e.mu.Unlock()

	var refused []string

	for _, name := range names {
		if e.markUnsafe(name, fmt.Errorf("the base dir could not be opened at startup: %w", cause)) {
			refused = append(refused, name)
		}
	}

	if len(refused) == 0 {
		return
	}

	e.log.Error("persist: the base dir exists and could not be opened at startup, so no service is loaded or saved "+
		"until restart; saving would replace state that was not loaded",
		"dir", e.dir, "services", refused, "error", cause)
}

// syncFailed turns the service off after a sync of its assets failed, and logs
// why, once: the kernel reports a lost write once, so no later save may name
// the assets. The state file on disk stays the last one saved; a restart loads
// it, and its load sweep removes the assets it does not name.
func (e *Engine) syncFailed(service, dir string, cause error) {
	e.markUnsafe(service, fmt.Errorf("%w: %w", errSyncFailed, cause))
	e.log.Error("persist: an asset sync failed, so saving and loading are off for the service until restart; "+
		"the state file on disk is the last one saved, and what changed since is lost at restart",
		"service", service, "dir", dir, "error", cause)
}

// markUnsafe turns loading and saving off for the service, for reason, and
// reports whether it did: a service already off keeps its first reason.
func (e *Engine) markUnsafe(service string, reason error) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	t, registered := e.services[service]
	if !registered || t.unsafe != nil {
		return false
	}

	t.unsafe = reason
	t.dirty = false

	return true
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
