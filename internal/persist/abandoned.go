package persist

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"
)

// abandonedFile is the marker a save that ran out of time leaves in a service's
// directory, beside its state files: <base>/<service>/.gostack-save-abandoned.
// A save abandoned at its deadline loses every change since the last completed
// save, and the process that abandoned it is on its way out, so nothing else
// would tell the next one. The marker is written, and fsynced, from the moment
// the save's context ends (see markAtDeadline), not when the work the save
// cannot interrupt returns: a Snapshot that outlasts the deadline outlasts the
// kill after it as well. The next save of the service that completes removes
// it; a shutdown that saved everything leaves none. Its presence is the whole
// signal, its content only for a person who finds it.
const abandonedFile = ".gostack-save-abandoned"

// markBudget is the time a round of markers has. A round runs after a deadline,
// and the last one, from the engine's Close, starts [lifecycle.CloseMargin]
// (100 ms) before the supervisor's SIGKILL, of which the caller's own wait for a
// Close leaves 80 ms, so no round may take longer. The engine keeps it in a
// field, which tests replace.
//
// A round is two steps. The markers of every service are created and written
// one after the other, which is all a kill needs: the kill of a process loses
// no file it has written. Then they are fsynced at once, with their
// directories, which a power cut needs and which is the slow part, since each
// fsync is a drive flush on macOS. For 8 services on the APFS volume of an
// Apple M1 Max the last marker existed after 1.4 to 1.9 ms and the fsyncs
// ended after 82 to 102 ms (Test_Engine_MarkAbandonedOfEightServices measures
// both). The budget ends the round either way, and the fsyncs it did not wait
// for go on while the process lives. A round after the first costs one lstat
// for each service that has a marker: 0.6 ms for 8.
const markBudget = 60 * time.Millisecond

// abandonedPath is the marker of service, relative to the base dir.
func abandonedPath(service string) string {
	return service + "/" + abandonedFile
}

// markAbandoned records that the saves of services were abandoned at their
// deadline, so the next start can say so. It runs as a flush gives up, so it
// takes no deadline of the caller's and tries every service whatever one of
// them fails with: a marker that cannot be written is logged, and the abandoned
// save has already said so itself. It returns once every marker is written and
// as many are fsynced as markBudget allows: a service that has a marker is
// skipped, so a second round, such as the engine's Close after its Stop, costs
// a lstat each, and one that has no time left writes none.
func (e *Engine) markAbandoned(ctx context.Context, services []string) {
	if len(services) == 0 {
		return
	}

	root, err := os.OpenRoot(e.dir)
	if err != nil {
		e.log.Warn("persist: the abandoned save could not be marked for the next start",
			"services", services, "error", err)

		return
	}

	defer func() { _ = root.Close() }()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.markBudget)
	defer cancel()

	written, done := make(chan struct{}), make(chan struct{})

	go func() {
		defer close(done)

		marked := e.writeMarkers(ctx, root, services)

		close(written)

		var wg sync.WaitGroup

		for _, m := range marked {
			wg.Go(func() {
				err := m.sync(root)
				if err != nil {
					e.log.Warn("persist: the abandoned save could not be marked for the next start",
						"service", m.service, "error", err)
				}
			})
		}

		wg.Wait()
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
		}
	}

	return marked
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

// sync fsyncs the marker and closes it, then fsyncs its directory, and the base
// dir when the directory is new.
func (m *marker) sync(root *os.Root) error {
	err := m.f.Sync()
	if err != nil {
		_ = m.f.Close()

		return fmt.Errorf("sync the marker: %w", err)
	}

	err = m.f.Close()
	if err != nil {
		return fmt.Errorf("close the marker: %w", err)
	}

	err = syncDir(root, m.service)
	if err != nil {
		return err
	}

	if m.madeDir {
		return syncDir(root, ".")
	}

	return nil
}

// markAtDeadline arranges that when ctx ends at a deadline (see ranOutOfTime)
// the services run still holds, which are not saved, are marked at that moment,
// whatever the flush is then inside: a Snapshot cannot be cut short, and the
// markers written after it returns come after the kill (SQ-435). It returns a
// function that ends it, which the caller calls once its flush has ended,
// waiting for a marking that has begun. A service that saves anyway, as one
// whose Snapshot returns after the deadline does, has its marker removed by
// that save (see clearAbandoned); a marker written while the save renames is at
// worst one warning too many.
func (e *Engine) markAtDeadline(ctx context.Context, run *flushRun) (end func()) {
	done := make(chan struct{})

	stop := context.AfterFunc(ctx, func() {
		defer close(done)

		if !ranOutOfTime(ctx, ctx.Err()) {
			return
		}

		e.mu.Lock()
		held := make([]string, 0, len(run.holding))

		for name := range run.holding {
			if e.services[name].owner == run {
				held = append(held, name)
			}
		}
		e.mu.Unlock()

		e.markAbandoned(ctx, held)
	})

	return sync.OnceFunc(func() {
		if !stop() {
			<-done
		}
	})
}

// markWaiting marks the services no flush has saved or holds, dirty and safe to
// save, when a Flush's ctx ended at a deadline before the Flush could save them,
// while it waited for the flush that held sem or for a Load. The services that
// flush held are marked by its own end of ctx (see markAtDeadline), whose
// deadline a Flush lends it when it waits (see wait).
func (e *Engine) markWaiting(ctx context.Context, err error) {
	if !ranOutOfTime(ctx, err) {
		return
	}

	e.mu.Lock()
	names := make([]string, 0, len(e.services))

	for name, t := range e.services {
		if t.dirty && t.unsafe == nil {
			names = append(names, name)
		}
	}
	e.mu.Unlock()

	e.markAbandoned(ctx, names)
}

// clearAbandoned removes the marker of service once a save of it has renamed
// its snapshot into place, so the directory sync that ends the save covers the
// removal. The marker goes after the rename: a crash between the two leaves a
// marker beside a good save, a warning too many, and the other order would
// leave a save that was abandoned unmarked. A failure to remove it is logged and
// no failure of the save, which is on disk.
func (e *Engine) clearAbandoned(root *os.Root, service string) {
	err := removeIfPresent(root, abandonedPath(service))
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
