// Package persist saves service state to disk and loads it back. A service
// registers a Snapshotter; the gateway calls MarkDirty after each mutating
// operation, and the Engine flushes the dirty services to
// <base>/<service>/store.json or store.bin. A flush timer is armed only while
// something is dirty, and every file is replaced atomically.
//
// Config decides whether persistence runs at all and where it writes. When it
// is off, New still returns an Engine, and every method does nothing.
package persist
