package dynamodb_test

import (
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/heaptest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_decodeBudget_aDroppedGatewayIsGoneOnceTheHeapIsSettled: a gateway
// that filled a table of about 550 MiB and answered small requests after its
// last collection is dropped. Its router's pool still holds a routing context
// naming it, so the gateway and its table outlive the next collection. The
// densest batch of numbers sent to a new gateway at the room of
// protocol.UnknownMemory is then decided with that collection as its first,
// and refused as too large while the dead table counts as live.
// heaptest.Settle's two collections free it, and the batch is served: what
// an earlier test drops is not what the decode budget weighs a body against.
func Test_decodeBudget_aDroppedGatewayIsGoneOnceTheHeapIsSettled(t *testing.T) {
	roomyMemoryLimit(t)
	protocol.SetInFlight(protocol.DefaultInFlight)
	t.Cleanup(func() { protocol.SetInFlight(0) })

	func() {
		dropped := newStackLeftOpen(t)
		dropped.ok(t, opCreate, hashTable("pair"))

		for _, prefix := range []string{"d", "e"} {
			items := make([]string, 0, batchItems)
			for i := range batchItems {
				items = append(items, `{"PutRequest":{"Item":`+denseItem(fmt.Sprint(prefix, i), numberList(62_000))+`}}`)
			}

			body := `{"RequestItems":{"pair":[` + strings.Join(items, ",") + `]}}`
			require.Equal(t, http.StatusOK, dropped.call(t, usEast1, opBatchWrite, body).status)
		}

		// The routing contexts below are put after the last collection.
		runtime.GC()
		runtime.GC()

		// Each request puts its routing context back in the router's pool;
		// several at once, so that race mode, which drops one Put in four,
		// leaves one there.
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() { dropped.ok(t, "ListTables", `{}`) })
		}

		wg.Wait()
	}()

	t.Run("the batch at the room of the unknown memory", func(t *testing.T) {
		heaptest.Settle(t)
		noMemoryLimit(t)

		s := newStack(t)
		s.ok(t, opCreate, hashTable("dense"))

		got := s.call(t, usEast1, opBatchWrite, denseBatch("dense", oneNumber, 64_000))
		assert.Equal(t, http.StatusOK, got.status, got.body[:min(len(got.body), 300)])
	})
}
