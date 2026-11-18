package persist

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	dirMode  fs.FileMode = 0o750
	fileMode fs.FileMode = 0o600

	tempSuffix = ".tmp"

	// unloadableInfix joins a state file's name and the time it was moved aside.
	unloadableInfix = ".unloadable-"
	// unloadableTime is the layout of that time: UTC, sortable, and free of
	// characters that file systems reject.
	unloadableTime = "20060102T150405Z"

	// maxLinkHops bounds how many symbolic links locate follows, as the kernel does.
	maxLinkHops = 40
)

// stateFile is where a state file really lives: name inside the directory root.
type stateFile struct {
	root *os.Root
	name string
	// owned is true when root was opened for this file and must be closed.
	owned bool
}

func (s stateFile) close() {
	if s.owned {
		_ = s.root.Close()
	}
}

// locate finds where the state file at file, relative to root, lives. A regular
// file or a missing one lives in root. A symbolic link lives at its target, which
// may be anywhere the operator pointed it, so the result is the target's own
// directory: reads follow the link, and a write replaces the target with a temp
// file and a rename in that directory, which leaves the link in place.
// base is the path root was opened at, which a relative link target is read
// against.
func locate(root *os.Root, base, file string) (stateFile, error) {
	here := stateFile{root: root, name: file}

	info, err := root.Lstat(file)
	if err != nil || info.Mode()&fs.ModeSymlink == 0 {
		// Missing, unreadable or regular: the caller's own open says which.
		return here, nil //nolint:nilerr // an Lstat error resurfaces when the caller opens the file
	}

	link, err := root.Readlink(file)
	if err != nil {
		return here, fmt.Errorf("read link %s: %w", file, err)
	}

	path := resolveLink(filepath.Join(base, filepath.Dir(file)), link)

	for range maxLinkHops {
		info, err = os.Lstat(path)
		if err != nil || info.Mode()&fs.ModeSymlink == 0 {
			dir, openErr := os.OpenRoot(filepath.Dir(path))
			if openErr != nil {
				return here, fmt.Errorf("open directory of link target: %w", openErr)
			}

			return stateFile{root: dir, name: filepath.Base(path), owned: true}, nil
		}

		link, err = os.Readlink(path)
		if err != nil {
			return here, fmt.Errorf("read link %s: %w", path, err)
		}

		path = resolveLink(filepath.Dir(path), link)
	}

	return here, fmt.Errorf("follow link %s: too many levels of symbolic links", file)
}

// resolveLink returns the path a link with the text target points at, when the
// link sits in the directory dir.
func resolveLink(dir, target string) string {
	if filepath.IsAbs(target) {
		return target
	}

	return filepath.Join(dir, target)
}

// writeState replaces the state file at file, relative to root, with data. A
// symbolic link keeps pointing at the file that now holds the new state.
// base is the path root was opened at.
func writeState(root *os.Root, base, file string, data []byte) error {
	loc, err := locate(root, base, file)
	if err != nil {
		return err
	}

	defer loc.close()

	err = writeAtomic(loc.root, loc.name, data)
	if err != nil {
		return err
	}

	if loc.owned {
		// The rename happened in the target's directory, not the service's.
		return syncDir(loc.root, ".")
	}

	return nil
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

// writeAtomic replaces name with data: it writes name+".tmp" in the same
// directory, fsyncs it, then renames it over name. A crash before the rename
// leaves name as it was and a temp file that the next write replaces.
func writeAtomic(root *os.Root, name string, data []byte) (err error) {
	tmp := name + tempSuffix

	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fileMode)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}

	defer func() {
		if err != nil {
			_ = root.Remove(tmp)
		}
	}()

	_, err = f.Write(data)
	if err != nil {
		_ = f.Close()

		return fmt.Errorf("write %s: %w", tmp, err)
	}

	err = f.Sync()
	if err != nil {
		_ = f.Close()

		return fmt.Errorf("sync %s: %w", tmp, err)
	}

	err = f.Close()
	if err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}

	err = root.Rename(tmp, name)
	if err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}

	return nil
}

// removeFile deletes name and its temp file. A file that is already gone is not
// an error.
func removeFile(root *os.Root, name string) error {
	for _, n := range []string{name, name + tempSuffix} {
		err := root.Remove(n)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", n, err)
		}
	}

	return nil
}

// syncDir fsyncs the directory dir, so renames and removals in it survive a
// crash.
func syncDir(root *os.Root, dir string) error {
	d, err := root.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}

	err = d.Sync()
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
