//go:build linux

package persist

import (
	"context"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// defaultSync flushes the whole filesystem f is on with syncfs(2). The man page
// (man7.org, syncfs(2)): "syncfs() is like sync(), but synchronizes just the
// filesystem containing file referred to by the open file descriptor fd", and
// "sync() or syncfs() provide the same guarantees as fsync() called on every
// file in the system or filesystem respectively". So one call covers every
// asset's data and every rename into blobs/<xx>, whatever else on the
// filesystem it writes back too. Since Linux 5.8 it reports an inode that
// failed to write back; an older kernel returns no such error.
func defaultSync(f *os.File) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return fmt.Errorf("syncfs: %w", err)
	}

	var syncErr error

	err = conn.Control(func(fd uintptr) {
		syncErr = unix.Syncfs(int(fd))
	})
	if err != nil {
		return fmt.Errorf("syncfs: %w", err)
	}

	if syncErr != nil {
		return fmt.Errorf("syncfs: %w", syncErr)
	}

	return nil
}

// syncPending makes every pending asset durable with one sync of the assets
// directory's filesystem: syncfs flushes the dirty data and metadata of every
// file on it, so it needs neither the ids nor their directories. It cannot be
// interrupted; sync stops waiting for it when ctx ends.
func (a *Assets) syncPending(context.Context, map[string]struct{}) error {
	f, err := a.root.Open(".")
	if err != nil {
		return fmt.Errorf("open the assets dir: %w", err)
	}

	err = a.fsync(f)
	if err != nil {
		_ = f.Close()

		return fmt.Errorf("sync the assets filesystem: %w", err)
	}

	err = f.Close()
	if err != nil {
		return fmt.Errorf("close the assets dir: %w", err)
	}

	return nil
}
