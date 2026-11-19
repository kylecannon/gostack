//go:build !linux

package persist

import "testing"

// syncModes forces each sync path the platform has: off Linux only per-file.
var syncModes = map[string]func(*testing.T, *Assets){
	syncByFile: forcePerFile,
}
