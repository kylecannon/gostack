package persist

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

const (
	dirMode  fs.FileMode = 0o750
	fileMode fs.FileMode = 0o600

	tempSuffix = ".tmp"
)

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
