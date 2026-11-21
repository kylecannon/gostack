package persist

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
)

// Test_Engine_MarkDirtyFieldsSitAheadOfTheWatchFields checks that the fields
// MarkDirty and the timer path touch are declared before slow and watchAfter,
// which only a flush with a ctx that can end reads. Ahead of them the new fields
// moved mu, services and dirtyAgain by 56 bytes and, with them, the Engine's size
// class, and BenchmarkMarkDirty read 16.6% slower on the CI runner's Xeon (17.69
// ns to 20.62 ns) with no change to its instructions, and none on darwin or
// Linux arm64. A field added later that MarkDirty touches belongs in the list.
func Test_Engine_MarkDirtyFieldsSitAheadOfTheWatchFields(t *testing.T) {
	var e Engine

	hot := map[string]uintptr{
		"mu":         unsafe.Offsetof(e.mu),
		"services":   unsafe.Offsetof(e.services),
		"timer":      unsafe.Offsetof(e.timer),
		"timerAt":    unsafe.Offsetof(e.timerAt),
		"backedOff":  unsafe.Offsetof(e.backedOff),
		"gen":        unsafe.Offsetof(e.gen),
		"running":    unsafe.Offsetof(e.running),
		"waiting":    unsafe.Offsetof(e.waiting),
		"flushing":   unsafe.Offsetof(e.flushing),
		"dirtyAgain": unsafe.Offsetof(e.dirtyAgain),
		"closed":     unsafe.Offsetof(e.closed),
	}
	watch := map[string]uintptr{
		"slow":       unsafe.Offsetof(e.slow),
		"watchAfter": unsafe.Offsetof(e.watchAfter),
	}

	for hotName, hotAt := range hot {
		for watchName, watchAt := range watch {
			assert.Less(t, hotAt, watchAt,
				"%s must be declared before %s: the fields MarkDirty touches keep their offsets", hotName, watchName)
		}
	}
}
