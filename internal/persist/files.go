package persist

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	dirMode  fs.FileMode = 0o750
	fileMode fs.FileMode = 0o600

	// tempSuffix starts the name of a temp file after the name of the file it
	// replaces. A temp file is <name>.tmp-<random>; builds before SQ-52 used
	// <name>.tmp. Both are swept after a save.
	tempSuffix = ".tmp"

	// unloadableInfix joins a state file's name and the time it was moved aside.
	unloadableInfix = ".unloadable-"
	// unloadableTime is the layout of that time: UTC, sortable, and free of
	// characters that file systems reject.
	unloadableTime = "20060102T150405Z"

	// maxLinkHops bounds how many symbolic links a save follows to the file it
	// replaces, as the kernel bounds a path lookup.
	maxLinkHops = 40

	// writeBuffer is the buffer between a Snapshot and its temp files.
	writeBuffer = 64 << 10
)

// errLinkLeaves is the reason a state file is refused when it is, or is reached
// through, a symbolic link that leaves the base dir. os.Root refuses every
// absolute link as well, even one that points inside the base dir.
var errLinkLeaves = errors.New(
	"a link on the way to the state file leaves the base dir or is absolute; use a relative link inside the base dir")

// leaves reports whether err is a path refused for leaving root: errLinkLeaves,
// or os.Root's own refusal. The os package does not export that error, so it is
// compared with the one root returns for "..", which always leaves the root.
func leaves(root *os.Root, err error) bool {
	if errors.Is(err, errLinkLeaves) {
		return true
	}

	var escape *fs.PathError

	_, probe := root.Lstat("..")

	return errors.As(probe, &escape) && errors.Is(err, escape.Err)
}

// dirOf returns file up to its last slash, without cleaning it, so that a ".."
// after a symbolic link is left for os.Root to resolve as the kernel does.
func dirOf(file string) string {
	dir, _, found := strings.CutLast(file, "/")
	if !found {
		return "."
	}

	return dir
}

// baseOf returns file after its last slash.
func baseOf(file string) string {
	_, base, found := strings.CutLast(file, "/")
	if !found {
		return file
	}

	return base
}

// target returns the path, relative to root, of the file that a save of file
// replaces: file itself, or the file its symbolic links lead to, so that the
// links stay. Each link's text is joined to the link's directory without
// cleaning, and root resolves the result, so a ".." after a directory link goes
// where the kernel would take it, and a path that leaves root is refused. A
// missing file is its own target.
func target(root *os.Root, file string) (string, error) {
	for range maxLinkHops {
		info, err := root.Lstat(file)

		switch {
		case errors.Is(err, fs.ErrNotExist):
			return file, nil
		case err != nil:
			return "", fmt.Errorf("look up %s: %w", file, err)
		case info.Mode()&fs.ModeSymlink == 0:
			return file, nil
		}

		link, err := root.Readlink(file)
		if err != nil {
			return "", fmt.Errorf("read link %s: %w", file, err)
		}

		if filepath.IsAbs(link) {
			return "", fmt.Errorf("%s -> %s: %w", file, link, errLinkLeaves)
		}

		file = dirOf(file) + "/" + link
	}

	return "", fmt.Errorf("follow %s: %w", file, syscall.ELOOP)
}

// stateWriter writes one state file: a temp file next to the file it replaces,
// created with O_EXCL under a name nobody can guess, so a file or link planted
// under a temp name is never opened. It is renamed over the target once it is
// complete and synced.
type stateWriter struct {
	root   *os.Root
	target string
	tmp    string
	f      *os.File
	buf    *bufio.Writer
}

// createState opens the temp file for a save of file.
func createState(root *os.Root, file string) (*stateWriter, error) {
	path, err := target(root, file)
	if err != nil {
		return nil, err
	}

	tmp := dirOf(path) + "/" + baseOf(path) + tempSuffix + "-" + rand.Text()

	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", tmp, err)
	}

	return &stateWriter{root: root, target: path, tmp: tmp, f: f, buf: bufio.NewWriterSize(f, writeBuffer)}, nil
}

// finish writes out the buffer, then fsyncs and closes the temp file.
func (w *stateWriter) finish() error {
	err := w.buf.Flush()
	if err != nil {
		return fmt.Errorf("write %s: %w", w.tmp, err)
	}

	err = w.f.Sync()
	if err != nil {
		return fmt.Errorf("sync %s: %w", w.tmp, err)
	}

	err = w.f.Close()
	if err != nil {
		return fmt.Errorf("close %s: %w", w.tmp, err)
	}

	return nil
}

// commit renames the temp file over the target.
func (w *stateWriter) commit() error {
	err := w.root.Rename(w.tmp, w.target)
	if err != nil {
		return fmt.Errorf("rename %s: %w", w.tmp, err)
	}

	return nil
}

// discard closes and removes a temp file that will not be committed. The file
// it was to replace is untouched.
func (w *stateWriter) discard() {
	_ = w.f.Close()
	_ = w.root.Remove(w.tmp)
}

// moveAside renames file to file + ".unloadable-" + the UTC time in the same
// directory, and returns the new name. A name that is taken gets a counter, so
// a file moved aside earlier is never replaced. When file is a symbolic link,
// the link moves and its target stays where it is.
func moveAside(root *os.Root, file string, at time.Time) (string, error) {
	stem := file + unloadableInfix + at.UTC().Format(unloadableTime)

	for n := 0; ; n++ {
		dest := stem
		if n > 0 {
			dest += "-" + strconv.Itoa(n)
		}

		_, err := root.Lstat(dest)
		if err == nil {
			continue
		}

		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("check %s: %w", dest, err)
		}

		err = root.Rename(file, dest)
		if err != nil {
			return "", fmt.Errorf("rename %s: %w", file, err)
		}

		return dest, nil
	}
}

// removeTemps deletes the temp files of the file named name in dir, which a
// crash between creating and renaming one leaves behind. A temp name that is a
// link is removed as a link; its target is untouched.
func removeTemps(root *os.Root, dir, name string) error {
	d, err := root.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}

	entries, err := d.ReadDir(-1)
	_ = d.Close()

	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}

	for _, entry := range entries {
		temp := name + tempSuffix
		if entry.Name() != temp && !strings.HasPrefix(entry.Name(), temp+"-") {
			continue
		}

		err = removeIfPresent(root, dir+"/"+entry.Name())
		if err != nil {
			return err
		}
	}

	return nil
}

// removeFile deletes file and its temp files. A file that is already gone is not
// an error.
func removeFile(root *os.Root, file string) error {
	err := removeIfPresent(root, file)
	if err != nil {
		return err
	}

	return removeTemps(root, dirOf(file), baseOf(file))
}

func removeIfPresent(root *os.Root, file string) error {
	err := root.Remove(file)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", file, err)
	}

	return nil
}

// syncDir fsyncs the directory dir, so renames and removals in it survive a
// crash.
func syncDir(root *os.Root, dir string) error {
	return syncDirWith(root, dir, syncFile)
}

// syncDirWith is syncDir with the sync it runs on the directory: [syncFile], or
// a platform's own.
func syncDirWith(root *os.Root, dir string, fsync func(*os.File) error) error {
	d, err := root.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}

	err = fsync(d)
	if err != nil {
		_ = d.Close()

		return fmt.Errorf("sync %s: %w", dir, err)
	}

	err = d.Close()
	if err != nil {
		return fmt.Errorf("close %s: %w", dir, err)
	}

	return nil
}
