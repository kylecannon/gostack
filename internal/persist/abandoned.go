package persist

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// abandonedFile is the marker a save that ran out of time leaves in a service's
// directory, beside its state files: <base>/<service>/.gostack-save-abandoned.
// A save abandoned at its deadline loses every change since the last completed
// save, and the process that abandoned it is on its way out, so nothing else
// would tell the next one. The marker is written, and fsynced, before the flush
// gives up, and the next save of the service that completes removes it; a
// shutdown that saved everything leaves none. Its presence is the whole signal,
// its content only for a person who finds it.
const abandonedFile = ".gostack-save-abandoned"

// abandonedPath is the marker of service, relative to the base dir.
func abandonedPath(service string) string {
	return service + "/" + abandonedFile
}

// markAbandoned records durably that the saves of services were abandoned at
// their deadline, so the next start can say so. It runs as a flush gives up, so
// it takes no deadline and tries every service whatever one of them fails with:
// a marker that cannot be written is logged, and the abandoned save has already
// said so itself.
func (e *Engine) markAbandoned(services []string) {
	root, err := os.OpenRoot(e.dir)
	if err != nil {
		e.log.Warn("persist: the abandoned save could not be marked for the next start",
			"services", services, "error", err)

		return
	}

	defer func() { _ = root.Close() }()

	for _, service := range services {
		err = writeAbandoned(root, service)
		if err != nil {
			e.log.Warn("persist: the abandoned save could not be marked for the next start",
				"service", service, "error", err)
		}
	}
}

// writeAbandoned writes the marker of service, with its directory when the
// service has none yet, and fsyncs what it changed.
func writeAbandoned(root *os.Root, service string) error {
	_, err := root.Stat(service)
	made := errors.Is(err, fs.ErrNotExist)

	err = root.MkdirAll(service, dirMode)
	if err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	f, err := root.OpenFile(abandonedPath(service), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fileMode)
	if err != nil {
		return fmt.Errorf("create the marker: %w", err)
	}

	_, err = f.WriteString(time.Now().UTC().Format(time.RFC3339) + "\n")
	if err == nil {
		err = f.Sync()
	}

	if err != nil {
		_ = f.Close()

		return fmt.Errorf("write the marker: %w", err)
	}

	err = f.Close()
	if err != nil {
		return fmt.Errorf("close the marker: %w", err)
	}

	err = syncDir(root, service)
	if err != nil {
		return err
	}

	if made {
		return syncDir(root, ".")
	}

	return nil
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
// at every start until a save of it completes and removes the marker.
func (e *Engine) warnAbandoned(root *os.Root, names []string) {
	var (
		services []string
		saved    []string
	)

	for _, name := range names {
		_, err := root.Stat(abandonedPath(name))
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
