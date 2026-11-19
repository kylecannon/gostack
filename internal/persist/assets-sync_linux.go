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
// filesystem it writes back too.
//
// Since Linux 5.8 it reports a writeback error that happened on the filesystem
// since f was opened, once per open file (fs/sync.c, lib/errseq.c). An older
// kernel reports none, so there a lost write goes unnoticed. A writeback error
// of any file on the filesystem, not only an asset, fails the sync.
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

// openFSDir opens the assets directory once, for every syncfs: a file opened
// before any asset is written is told of every writeback error after it,
// whichever process's syncfs saw the error first.
func openFSDir(root *os.Root) (*os.File, error) {
	f, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("open the assets dir: %w", err)
	}

	return f, nil
}

// syncPending makes every pending asset durable with one sync of the assets
// directory's filesystem: syncfs flushes the dirty data and metadata of every
// file on it, so it needs neither the ids nor their directories. It cannot be
// interrupted; sync stops waiting for it when ctx ends.
func (a *Assets) syncPending(context.Context, map[string]struct{}) error {
	err := a.fsync(a.fsDir)
	if err != nil {
		return fmt.Errorf("sync the assets filesystem: %w", err)
	}

	return nil
}
