//go:build !linux

package persist

import (
	"os"
	"runtime"
)

// chooseSync fsyncs each asset off Linux: there is no syncfs.
func chooseSync(*os.Root) (syncMode, error) {
	return syncMode{sync: syncFile, fs: runtime.GOOS}, nil
}
