package persist

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sync"
	"syscall"
	"time"
)

// abandonedFile is the marker a save that ran out of time leaves in a service's
// directory, beside its state files: <base>/<service>/.gostack-save-abandoned.
// A save abandoned at its deadline loses every change since the last completed
// save, and the process that abandoned it is on its way out, so nothing else
// would tell the next one. The marker is written from the moment the save's
// context ends at a deadline (see markAtDeadline), not when the work the save
// cannot interrupt returns: a Snapshot that outlasts the deadline outlasts the
// kill after it as well. A context with no deadline that ends with a deadline's
// cause is marked within [markWatchAfter] of its end once the flush has lasted
// that long (see slowWatch), and when the flush ends before that. It is fsynced
// after, within the round's budget, so a marker created and not yet fsynced when
// the process is killed can be lost by a power cut, which loses the warning and
// never state. The next save of the service that
// completes removes it, unless the service changed since that save took its
// snapshot (see clearAbandoned); a shutdown that saved everything leaves none.
// Its presence is the whole signal, its content only for a person who finds it.
const abandonedFile = ".gostack-save-abandoned"

// markBudget is the time the marking rounds of one flush share (see
// markAllowance). A flush marks in up to three rounds, and the last flush, from
// the engine's Close, starts [lifecycle.CloseMargin] (100 ms) before the
// supervisor's SIGKILL, of which the caller's own wait for a Close leaves 80 ms,
// so the rounds together may not take longer. The engine keeps it in a field,
// which tests replace.
//
// A round is two steps. The markers of every service are created and written
// one after the other, which is all a kill needs: the kill of a process loses
// no file it has written. Then they are made durable, which a power cut needs
// and which is the slow part: each marker and its directory gets fsync(2), and
// on macOS, where that leaves the data in the drive's cache, one F_FULLFSYNC on
// the base dir follows (see markerSync). A marker created and not yet fsynced
// when the process is killed can be lost by a power cut, which costs the warning
// and never state. The budget ends the round either way, and the fsyncs it did
// not wait for go on while the process lives. A round after the first costs one
// lstat for each service that has a marker.
//
// Test_Engine_MarkAbandonedOfEightServices measures a round of 8 services: the
// figures are in docs/architecture.md, Persistence, Assets.
const markBudget = 60 * time.Millisecond

// markAllowance is the time the marking rounds of one flush share: the one at
// the deadline (see markAtDeadline), the one that ends the flush (see
// logAbandoned), and the one a Flush that waited runs (see markWaiting). Each
// round has what the ones before it left of markBudget, and spends the time it
// took, so the three together take markBudget, not three times it. They do not
// share the clock: a round that comes after the Snapshot the flush was inside
// returned has the time the first did not use. A round with none left writes
// nothing and logs what it could not mark. Flush makes one for its call when its
// ctx has a deadline (see hasDeadline), at two allocations; a call with none has
// no allowance, and each round of it has the whole budget.
type markAllowance struct {
	mu    sync.Mutex
	spent time.Duration
}

// left returns what budget leaves after the rounds that have ended.
func (a *markAllowance) left(budget time.Duration) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()

	return budget - a.spent
}

// spend records a round that took d.
func (a *markAllowance) spend(d time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.spent += d
}

// markAllowanceKey is the context key under which a Flush keeps its markAllowance.
type markAllowanceKey struct{}

// hasDeadline reports whether ctx has a deadline, which is how it usually ends
// at one; a caller that cancels one with a deadline's cause ends at it without.
// A flush with a ctx that has none does not pay, in its allocations, for marking
// it will not need: Flush makes it no allowance, so each of its rounds has the
// whole budget (see allowanceOf), and markAtDeadline arms nothing for it until
// the flush has lasted [markWatchAfter] (see slowWatch).
func hasDeadline(ctx context.Context) bool {
	_, ok := ctx.Deadline()

	return ok
}

// withMarkAllowance returns ctx with an allowance for the rounds of one flush,
// or ctx when it has one.
func withMarkAllowance(ctx context.Context) context.Context {
	if ctx.Value(markAllowanceKey{}) != nil {
		return ctx
	}

	return context.WithValue(ctx, markAllowanceKey{}, &markAllowance{})
}

// allowanceOf returns the allowance ctx carries, or a new one for a round that
// is alone.
func allowanceOf(ctx context.Context) *markAllowance {
	a, ok := ctx.Value(markAllowanceKey{}).(*markAllowance)
	if !ok {
		return &markAllowance{}
	}

	return a
}

// abandonedPath is the marker of service, relative to the base dir.
func abandonedPath(service string) string {
	return service + "/" + abandonedFile
}

// markAbandoned records that the saves of services were abandoned at their
// deadline, so the next start can say so. It runs as a flush gives up, so it
// takes no deadline of the caller's and tries every service whatever one of
// them fails with: a marker that cannot be written is logged, and the abandoned
// save has already said so itself. It returns once every marker is written and
// as many are fsynced as the allowance ctx's flush has left (see markAllowance):
// a service that has a marker is skipped, so a second round, such as the one that
// ends a flush, costs a lstat each, and one that has no time left writes none.
// The root stays open until the fsyncs have ended, whether or not the round
// waited for them: the goroutine that runs them closes it.
func (e *Engine) markAbandoned(ctx context.Context, services []string) {
	if len(services) == 0 {
		return
	}

	allowance := allowanceOf(ctx)

	root, err := os.OpenRoot(e.dir)
	if err != nil {
		e.log.Warn("persist: the abandoned save could not be marked for the next start",
			"services", services, "error", err)

		return
	}

	start := time.Now()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), allowance.left(e.markBudget))

	defer func() {
		cancel()
		allowance.spend(time.Since(start))
	}()

	written, done := make(chan struct{}), make(chan struct{})

	go func() {
		defer close(done)
		defer func() { _ = root.Close() }()

		marked := e.writeMarkers(ctx, root, services)

		close(written)
		e.syncMarkers(root, marked)
	}()

	select {
	case <-written:
	case <-ctx.Done():
		select {
		case <-written:
		default:
			e.log.Warn("persist: the abandoned saves could not all be marked for the next start in time",
				"services", services, "budget", e.markBudget)

			return
		}
	}

	// What is left is the fsyncs, which the process's exit may cut short.
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// writeMarkers creates the marker of each of services, one after the other, and
// returns the ones it wrote, to be fsynced. A marker that cannot be written is
// logged.
func (e *Engine) writeMarkers(ctx context.Context, root *os.Root, services []string) []*marker {
	var marked []*marker

	for _, service := range services {
		m, err := writeAbandoned(ctx, root, service)
		if err != nil {
			e.log.Warn("persist: the abandoned save could not be marked for the next start",
				"service", service, "error", err)

			continue
		}

		if m != nil {
			marked = append(marked, m)

			e.dropIfRenamed(root, service)
		}
	}

	return marked
}

// dropIfRenamed removes the marker just written for service when the service's
// save has renamed its snapshot into place since the services to mark were
// chosen, and nothing has changed it: the save listed the directory for a marker
// before this one was there, so it would never remove it, and its state is on
// disk. The save records the rename before it lists the directory (see
// markRenamed): a marker whose check comes before the record was written before
// the listing, and the save removes it, and one whose check comes after sees the
// record, and this removes it.
func (e *Engine) dropIfRenamed(root *os.Root, service string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	t, ok := e.services[service]
	if !ok || !t.renamed || t.dirty {
		return
	}

	err := removeIfPresent(root, abandonedPath(service))
	if err != nil {
		e.log.Warn("persist: the marker of a save that completed could not be removed; "+
			"the next start warns until a save removes it", "service", service, "error", err)
	}
}

// marker is a marker written and not yet fsynced.
type marker struct {
	service string
	f       *os.File
	// madeDir is true when the service's directory was made for the marker.
	madeDir bool
}

// writeAbandoned writes the marker of service, with its directory when the
// service has none yet, and returns it to be fsynced, or nil when it wrote none.
// It writes nothing when anything is at the marker's path already: the service
// is marked, by an earlier round or an earlier run, and a link planted there is
// left alone. The marker is created with O_EXCL, which no open of a link passes
// (open(2): with O_CREAT and O_EXCL, a link at the path fails with EEXIST
// whatever it points to), so a link that appears between the check and the
// create never has another file written through it. It starts nothing once ctx
// ends.
func writeAbandoned(ctx context.Context, root *os.Root, service string) (*marker, error) {
	_, err := root.Lstat(abandonedPath(service))
	if err == nil {
		return nil, nil //nolint:nilnil // marked already is no error
	}

	if ctx.Err() != nil {
		return nil, fmt.Errorf("out of time: %w", ctx.Err())
	}

	_, err = root.Stat(service)
	made := errors.Is(err, fs.ErrNotExist)

	err = root.MkdirAll(service, dirMode)
	if err != nil {
		return nil, fmt.Errorf("create dir: %w", err)
	}

	f, err := root.OpenFile(abandonedPath(service), os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if errors.Is(err, fs.ErrExist) {
		return nil, nil //nolint:nilnil // marked meanwhile is no error
	}

	if err != nil {
		return nil, fmt.Errorf("create the marker: %w", err)
	}

	_, err = f.WriteString(time.Now().UTC().Format(time.RFC3339) + "\n")
	if err != nil {
		_ = f.Close()

		return nil, fmt.Errorf("write the marker: %w", err)
	}

	return &marker{service: service, f: f, madeDir: made}, nil
}

// syncMarkers makes the markers durable: each one and its directory with sync,
// all at once, and then, when the platform has one, the drive's cache with a
// single flush on the base dir (see markerSync). A failure is logged, and the
// flush is tried whatever the syncs said, since it is what makes the ones that
// went through permanent.
func (e *Engine) syncMarkers(root *os.Root, marked []*marker) {
	if len(marked) == 0 {
		return
	}

	var wg sync.WaitGroup

	for _, m := range marked {
		wg.Go(func() {
			err := m.sync(root, e.markSync)
			if err != nil {
				e.log.Warn("persist: the abandoned save could not be marked for the next start",
					"service", m.service, "error", err)
			}
		})
	}

	wg.Wait()

	if e.markFlush == nil {
		return
	}

	err := flushMarkers(root, e.markSync, e.markFlush)
	if err != nil {
		e.log.Warn("persist: the abandoned saves could not all be made durable for the next start",
			"services", markedNames(marked), "error", err)
	}
}

// markedNames is the services of marked.
func markedNames(marked []*marker) []string {
	names := make([]string, len(marked))
	for i, m := range marked {
		names[i] = m.service
	}

	return names
}

// flushMarkers flushes the drive's cache with flush on the base dir, after every
// marker is synced with fsync. A filesystem with no drive flush answers ENOTSUP,
// as an SMB mount does, and the base dir is synced instead, as flushDrive does.
func flushMarkers(root *os.Root, fsync, flush func(*os.File) error) error {
	dir, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open the base dir to flush the drive: %w", err)
	}

	err = flush(dir)
	if errors.Is(err, syscall.ENOTSUP) {
		err = fsync(dir)
	}

	if err != nil {
		_ = dir.Close()

		return fmt.Errorf("flush the drive: %w", err)
	}

	err = dir.Close()
	if err != nil {
		return fmt.Errorf("close the base dir after flushing the drive: %w", err)
	}

	return nil
}

// sync makes the marker durable with fsync and closes it, then does the same for
// its directory, and for the base dir when the directory is new.
func (m *marker) sync(root *os.Root, fsync func(*os.File) error) error {
	err := fsync(m.f)
	if err != nil {
		_ = m.f.Close()

		return fmt.Errorf("sync the marker: %w", err)
	}

	err = m.f.Close()
	if err != nil {
		return fmt.Errorf("close the marker: %w", err)
	}

	err = syncDirWith(root, m.service, fsync)
	if err != nil {
		return err
	}

	if m.madeDir {
		return syncDirWith(root, ".", fsync)
	}

	return nil
}

// markWatchAfter is how long a flush whose ctx has no deadline runs before
// markAtDeadline begins to watch for the ctx ending with a deadline's cause (see
// slowWatch). A flush in time is over before then and allocates nothing for it.
const markWatchAfter = 50 * time.Millisecond

// markAtDeadline arranges that when ctx ends at a deadline (see ranOutOfTime)
// the services the deadline puts at risk are marked at that moment, whatever the
// flush is then inside: a Snapshot cannot be cut short, and the markers written
// after it returns come after the kill (SQ-435). They are the ones run holds and
// has not saved, the ones changed since (see unsavedLocked), and the ones the
// flush by the timer holds, to which the deadline is lent here, not only when
// the Flush waits for sem (see wait): a Flush inside its own Snapshot at its
// deadline would otherwise leave those to the end of that Snapshot. It marks
// them itself, so it does not return, nor the Flush with it, before the
// markers are written. The caller ends it with [flushRun.endMark] once its flush
// has ended, which waits for a marking that has begun.
//
// The flush by the timer arms none: the Flush that lends it a deadline marks
// what it holds. A ctx with a deadline is watched with [context.AfterFunc]. One
// with none is not, since the flush in time would pay for it in allocations
// (SQ-461): a ctx that can end is left to the slowWatch, which watches it once
// the flush has lasted [markWatchAfter], and a ctx that cannot end is never
// watched. A service that saves anyway, as one whose Snapshot returns after the
// deadline does, has its marker removed by that save (see clearAbandoned), or by
// the marking that finds the save has renamed (see dropIfRenamed).
func (e *Engine) markAtDeadline(ctx context.Context, run *flushRun) {
	if run.by == byTimer {
		return
	}

	if !hasDeadline(ctx) {
		if ctx.Done() != nil {
			e.slow.watch(ctx, e, run)
			run.watching = true
		}

		return
	}

	stop, done := e.markWhenEnds(ctx, run)

	// The marking that has begun is waited for, so the flush returns after its
	// markers are written.
	run.endDeadline = sync.OnceFunc(func() {
		if !stop() {
			<-done
		}
	})
}

// markWhenEnds arranges that e marks what run puts at risk when ctx ends. It
// returns the stop of [context.AfterFunc], and a channel that closes when a
// marking that has begun has ended.
func (e *Engine) markWhenEnds(ctx context.Context, run *flushRun) (stop func() bool, done <-chan struct{}) {
	ended := make(chan struct{})

	stop = context.AfterFunc(ctx, func() {
		defer close(ended)

		e.markAtEnd(ctx, run)
	})

	return stop, ended
}

// markAtEnd marks what the end of ctx puts at risk, when it is the end of a
// deadline (see ranOutOfTime), and lends the flush by the timer that deadline.
func (e *Engine) markAtEnd(ctx context.Context, run *flushRun) {
	if !ranOutOfTime(ctx, ctx.Err()) {
		return
	}

	e.mu.Lock()
	if e.timed != nil {
		e.timed.lend(context.Cause(ctx))
	}

	names := e.unsavedLocked(ctx, run)
	e.mu.Unlock()

	e.markAbandoned(ctx, names)
}

// endMark ends what markAtDeadline arranged for the flush, and waits for a
// marking that has begun. It is safe to call twice.
func (r *flushRun) endMark() {
	if r.endDeadline != nil {
		r.endDeadline()
	}

	if r.watching {
		r.watching = false
		r.e.slow.unwatch(r)
	}
}

// slowWatch watches the flushes whose ctx has no deadline but can end, so that
// one that ends with a deadline's cause (see ranOutOfTime), as a
// [context.WithCancelCause] ctx can, is marked when it ends and not when the
// Snapshot it is inside returns. Watching a ctx costs allocations, which a flush
// in time must not pay (SQ-461), and no flush in tree has such a ctx end at a
// deadline, so the watch begins only for a flush that has lasted
// [Engine.watchAfter]: one timer for the engine, made at the first such flush and
// reset after it, looks at the registered flushes when it fires and arms
// [context.AfterFunc] for each that has lasted that long. A flush in time
// registers and unregisters, which allocates nothing once the registry has grown
// to the flushes that run at once. A ctx that ended before the watch began is
// marked as it begins, which is [Engine.watchAfter] after the flush began at
// most.
type slowWatch struct {
	mu    sync.Mutex
	runs  []*flushRun
	timer *time.Timer
	// timed is true while timer is set to fire.
	timed bool
}

// slowFlush is what a flushRun keeps of its registration with a slowWatch. The
// watch guards it.
type slowFlush struct {
	ctx   context.Context //nolint:containedctx // the ctx of the flush, which the watch arms
	since time.Time
	// stop, set once the watch has armed the flush, is the function of
	// [context.AfterFunc] that watches ctx, and done closes when the marking it
	// began has ended.
	stop func() bool
	done <-chan struct{}
}

// watch registers run, whose flush by hand runs with ctx, which has no deadline
// but can end.
func (w *slowWatch) watch(ctx context.Context, e *Engine, run *flushRun) {
	w.mu.Lock()
	defer w.mu.Unlock()

	run.slow = slowFlush{ctx: ctx, since: time.Now()}
	w.runs = append(w.runs, run)

	if !w.timed {
		w.schedule(e, e.watchAfter)
	}
}

// schedule sets the timer to fire in d. The caller holds mu.
func (w *slowWatch) schedule(e *Engine, d time.Duration) {
	w.timed = true

	if w.timer == nil {
		w.timer = time.AfterFunc(d, func() { w.look(e) })

		return
	}

	w.timer.Reset(d)
}

// look arms every registered flush that has lasted [Engine.watchAfter], with
// the same watch a ctx with a deadline has, and sets the timer for the next of
// the others. The marking runs in its own goroutine, which takes no lock of the
// watch.
func (w *slowWatch) look(e *Engine) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.timed = false

	var next time.Duration

	for _, run := range w.runs {
		if run.slow.stop != nil {
			continue
		}

		left := e.watchAfter - time.Since(run.slow.since)
		if left > 0 {
			if next == 0 || left < next {
				next = left
			}

			continue
		}

		run.slow.stop, run.slow.done = e.markWhenEnds(run.slow.ctx, run)
	}

	if next > 0 {
		w.schedule(e, next)
	}
}

// unwatch removes run, and waits for a marking that has begun, so the flush
// returns after its markers are written.
func (w *slowWatch) unwatch(run *flushRun) {
	w.mu.Lock()

	// run is registered: endMark calls unwatch once, for a run it registered.
	i := slices.Index(w.runs, run)
	w.runs = slices.Delete(w.runs, i, i+1)
	stop, done := run.slow.stop, run.slow.done
	run.slow = slowFlush{}

	if len(w.runs) == 0 && w.timed {
		w.timer.Stop()
		w.timed = false
	}

	w.mu.Unlock()

	if stop != nil && !stop() {
		<-done
	}
}

// markWaiting marks the services a Flush's ctx, ended at a deadline, left unsaved
// when it ended waiting for the flush that held sem or for a Load, before it
// could save them: the dirty ones nothing holds, and the ones the flush by the
// timer holds, to which the deadline was lent by wait. They are marked before
// the Flush returns.
func (e *Engine) markWaiting(ctx context.Context, err error) {
	if !ranOutOfTime(ctx, err) {
		return
	}

	e.markUnsaved(ctx, nil, nil)
}

// markUnsaved marks the services unsavedLocked returns for run, and cut besides:
// the ones a flush's deadline cut, which are dirty again unless saving is off
// for them.
func (e *Engine) markUnsaved(ctx context.Context, run *flushRun, cut []string) {
	e.mu.Lock()
	names := append(e.unsavedLocked(ctx, run), cut...)
	e.mu.Unlock()

	e.markAbandoned(ctx, names)
}

// unsavedLocked returns the services a deadline of ctx puts at risk: the ones
// whose save the flush run holds, and when run is a Flush's, or nil for a Flush
// that is not running one, the ones the flush by the timer holds, since the
// deadline is lent to it; the ones a flush by another caller holds, whatever
// deadline that flush has, since the kill this deadline stands for may come
// before the deadline of its own, or it has none, as a request's save (POST
// /_gostack/state/save) has; and every service that is dirty, which a change
// since its last save, or since the snapshot a save took, left unsaved. A held
// service whose save has renamed its snapshot into place is none of these: its
// state is on disk, and a marker beside it would be a warning too many. A
// service run lost for a reason of its own is not one of them, though it is
// dirty again: its warning names it, and a longer SHUTDOWN_TIMEOUT would not
// have saved it (see logAbandoned). The caller holds mu.
func (e *Engine) unsavedLocked(ctx context.Context, run *flushRun) []string {
	lends := run == nil || run.by == byHand
	names := make([]string, 0, len(e.services))

	for name, t := range e.services {
		if t.unsafe != nil {
			continue
		}

		held := t.owner != nil && !t.renamed &&
			(t.owner == run || lends && t.owner.by == byTimer || t.owner.by == byHand)

		if held || t.dirty && !run.lostToOwnFaultLocked(ctx, name) {
			names = append(names, name)
		}
	}

	return names
}

// clearAbandoned removes the marker of service once a save of it has renamed
// its snapshot into place, so the directory sync that ends the save covers the
// removal, unless the service has changed since the save took its snapshot: that
// change is lost if the process is killed, and the marker is still true. The
// next save of the service that finds it unchanged removes it. The marker goes
// after the rename: a crash between the two leaves a marker beside a good save,
// a warning too many, and the other order would leave a save that was abandoned
// unmarked. A failure to remove it is logged and no failure of the save, which
// is on disk.
//
// The check and the removal are one step under mu, so a MarkDirty cannot come
// between them and leave a dirty service with no marker. Only a marker that is
// there takes mu, as the removal of one that is not has nothing to decide.
func (e *Engine) clearAbandoned(root *os.Root, service string) {
	_, err := root.Lstat(abandonedPath(service))
	if errors.Is(err, fs.ErrNotExist) {
		return
	}

	e.mu.Lock()

	if e.services[service].dirty {
		e.mu.Unlock()

		return
	}

	err = removeIfPresent(root, abandonedPath(service))
	e.mu.Unlock()

	if err != nil {
		e.log.Warn("persist: the marker of an earlier abandoned save could not be removed; "+
			"the next start warns again until a save removes it", "service", service, "error", err)
	}
}

// warnAbandoned logs one warning naming the services among names that have a
// marker, and for each the time of its last completed save, which is the
// modification time of the newest state file it has: the file Load restored, or
// that of a format it tried. The warning is logged once, so a service is named
// at every start until a save of it completes and removes the marker. A marker
// is what is at its path, a link included: the signal is its presence.
func (e *Engine) warnAbandoned(root *os.Root, names []string) {
	var (
		services []string
		saved    []string
	)

	for _, name := range names {
		_, err := root.Lstat(abandonedPath(name))
		if err != nil {
			continue
		}

		services = append(services, name)
		saved = append(saved, name+"="+e.lastSaved(root, name))
	}

	if len(services) == 0 {
		return
	}

	e.log.Warn("persist: a save was abandoned at its deadline before this start, so what changed after "+
		"the last completed save is lost; raise SHUTDOWN_TIMEOUT or lower PERSIST_FREQUENCY",
		"services", services, "last_completed_save", saved)
}

// lastSaved returns the UTC time of the newest state file of service, or
// "never" when it has none.
func (e *Engine) lastSaved(root *os.Root, service string) string {
	var newest time.Time

	for _, f := range e.loadFormats {
		info, err := root.Stat(service + "/" + f.fileName())
		if err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}

	if newest.IsZero() {
		return "never"
	}

	return newest.UTC().Format(time.RFC3339)
}
