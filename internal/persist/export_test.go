package persist

// PendAgain puts ids back as Commit left them before a save: committed, fresh
// and unsynced. A save consumes all three (begin clears fresh, sync takes
// unsynced, settle drops the committed ids the state names), so a benchmark
// that calls it between saves times the save of the same pending assets each
// iteration with no asset written or removed.
func PendAgain(a *Assets, ids []string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, id := range ids {
		a.committed[id] = struct{}{}
		a.fresh[id] = struct{}{}
		a.unsynced[id] = struct{}{}
	}
}
