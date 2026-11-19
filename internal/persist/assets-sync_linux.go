//go:build linux

package persist

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// syncfsProven are the filesystems on which syncfs is proven to report a lost
// asset write to gostack even after another process's sync(2) or syncfs saw it
// first, by the foreign-sync tests scripts/writeback-fault.sh runs. Only these
// take syncfs; every other filesystem, and one whose type cannot be read, takes
// the per-file path, whose fsync on a descriptor opened for the asset reports
// the error however many other syncs ran (SQ-175). On overlayfs, for one, the
// first syncfs or sync(2) on the mount after a failed writeback takes the error,
// whichever process calls it, and the next syncfs returns nil.
//
// ext4, xfs and btrfs passed them on kernel 7.2. ext2 and ext3 have ext4's
// magic number and are driven by the same kernel code, so they ride on ext4's
// proof.
var syncfsProven = map[uint32]string{
	unix.BTRFS_SUPER_MAGIC: "btrfs",
	unix.EXT4_SUPER_MAGIC:  "ext4",
	unix.XFS_SUPER_MAGIC:   "xfs",
}

// fsNames names other filesystems for the log.
var fsNames = map[uint32]string{
	unix.CIFS_SUPER_MAGIC:      "cifs",
	unix.F2FS_SUPER_MAGIC:      "f2fs",
	unix.FUSE_SUPER_MAGIC:      "fuse",
	unix.NFS_SUPER_MAGIC:       "nfs",
	unix.OVERLAYFS_SUPER_MAGIC: "overlay",
	unix.SMB2_SUPER_MAGIC:      "smb2",
	unix.TMPFS_MAGIC:           "tmpfs",
	unix.V9FS_MAGIC:            "9p",
}

// chooseSync reads the type of the filesystem the assets directory is on. On a
// filesystem in syncfsProven it keeps the directory open for syncfs: a file
// opened before any asset is written is told of every writeback error after
// it. Anywhere else it fsyncs each asset.
func chooseSync(root *os.Root) (syncMode, error) {
	dir, err := root.Open(".")
	if err != nil {
		return syncMode{}, fmt.Errorf("open the assets dir: %w", err)
	}

	magic, statErr := fsType(dir)

	name, proven := syncfsProven[magic]
	if statErr == nil && proven {
		return syncMode{dir: dir, sync: syncFS, fs: name}, nil
	}

	_ = dir.Close()

	return syncMode{sync: syncFile, fs: fsName(magic, statErr)}, nil
}

// fsName names the filesystem with magic number magic for the log, or says why
// its type is unknown.
func fsName(magic uint32, statErr error) string {
	if statErr != nil {
		return "unknown (" + statErr.Error() + ")"
	}

	name, known := fsNames[magic]
	if !known {
		name = fmt.Sprintf("0x%x", magic)
	}

	return name
}

// fsType returns the magic number of the filesystem f is on.
func fsType(f *os.File) (uint32, error) {
	conn, err := f.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("statfs: %w", err)
	}

	var (
		st      unix.Statfs_t
		statErr error
	)

	err = conn.Control(func(fd uintptr) {
		statErr = unix.Fstatfs(int(fd), &st)
	})
	if err != nil {
		return 0, fmt.Errorf("statfs: %w", err)
	}

	if statErr != nil {
		return 0, fmt.Errorf("statfs: %w", statErr)
	}

	return uint32(st.Type), nil //nolint:gosec // a magic number, compared as its 32 bits
}

// syncFS flushes the whole filesystem f is on with syncfs(2). The man page
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
func syncFS(f *os.File) error {
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
