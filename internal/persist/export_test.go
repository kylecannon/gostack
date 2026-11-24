package persist

// PendAgain puts ids, assets a has committed, back as commit leaves them before
// a save, through the same trackLocked and pendLocked: committed, fresh and
// unsynced. A save consumes all three (begin clears fresh, sync takes unsynced,
// settle drops the committed ids the state names), so a benchmark that calls it
// between saves times the save of the same pending assets each iteration with
// no asset written or removed.
//
// It is valid only between saves, never alongside Remove of any of ids, and
// only on assets whose sync has not failed: a save under way would not see the
// ids as fresh, a removed id would be pending again with no file, and after a
// failed sync the ids would be dropped from committed instead.
func PendAgain(a *Assets, ids []string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, id := range ids {
		a.trackLocked(id)
		a.pendLocked(id)
	}
}
