//go:build darwin

package persist

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// chooseSync takes the per-file path on macOS, split in two. Each asset and
// directory gets a plain fsync(2), and one F_FULLFSYNC on the assets directory
// runs after the last of them.
//
// Go's [os.File.Sync] is F_FULLFSYNC on macOS, which flushes the drive's whole
// write cache, so a save with N pending assets paid N+M cache flushes. A
// shutdown save of 3,000 assets then ran past its 4.5 s deadline and was
// abandoned (SQ-424); with one flush the whole shutdown takes 0.21 to 0.27 s.
//
// The durability argument is Apple's own. fsync(2) says the call hands the
// file's data to the drive, which may keep it in its write cache. The guarantee
// across files is in fcntl(2): "F_FULLFSYNC Does the same thing as fsync(2)
// then asks the drive to flush all buffered data to the permanent storage
// device (arg is ignored). As this drains the entire queue of the device and
// acts as a barrier, data that had been fsync'd on the same device before is
// guaranteed to be persisted when this call returns." So one F_FULLFSYNC after
// all the fsyncs have returned makes all of them permanent, which is what a
// snapshot's rename needs before it names them. The order is the same as with a
// flush per file: no rename before the last flush returned. fsync(2) still
// reports an error of its own file, so a lost write is still found per file,
// and the flush's own error is final like any other (see flushDrive), except
// ENOTSUP, where the directory is fsynced instead, as Go's own Sync does.
//
// F_BARRIERFSYNC is not used. It orders writes against later ones without
// flushing the cache, so it does not make the assets permanent before the
// snapshot is renamed, which a power loss needs.
func chooseSync(*os.Root) (syncMode, error) {
	return syncMode{sync: fsyncFile, flush: fullFsync, fs: runtime.GOOS}, nil
}

// fsyncFile fsyncs one file or directory with fsync(2) alone, which on macOS
// leaves the data in the drive's cache; see chooseSync. The error is final like
// any fsync's: a file's writeback error is reported once, and the flush after
// it cannot report a lost write for a file it never saw.
func fsyncFile(f *os.File) error {
	return control(f, "fsync", unix.Fsync)
}

// fullFsync flushes the drive's write cache with F_FULLFSYNC, on the descriptor
// f. It does what [os.File.Sync] does here, except that it leaves the fallback
// on ENOTSUP to flushDrive, and is called by name so a change to how Go syncs a
// file cannot turn the one flush a save runs into a no-op or the one per file
// back.
func fullFsync(f *os.File) error {
	return control(f, "F_FULLFSYNC", func(fd int) error {
		_, err := unix.FcntlInt(uintptr(fd), unix.F_FULLFSYNC, 0)

		return err //nolint:wrapcheck // control adds the call's name
	})
}

// control runs call, named name for the error, on the descriptor of f, and
// again when a signal interrupts it.
func control(f *os.File, name string, call func(fd int) error) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}

	var callErr error

	err = conn.Control(func(fd uintptr) {
		for {
			callErr = call(int(fd))
			if !errors.Is(callErr, unix.EINTR) {
				return
			}
		}
	})
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}

	if callErr != nil {
		return fmt.Errorf("%s: %w", name, callErr)
	}

	return nil
}
