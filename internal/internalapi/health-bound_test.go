package internalapi_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// doCharged serves one request as the gateway does: with a protocol.Charge on
// its context, released when the handler returns.
func (h *harness) doCharged(t *testing.T, method, target, body string) response {
	t.Helper()

	charge := &protocol.Charge{}
	ctx := protocol.WithCharge(t.Context(), charge)
	request := httptest.NewRequestWithContext(ctx, method, target, strings.NewReader(body))
	recorder := httptest.NewRecorder()

	h.router.ServeHTTP(recorder, request)
	charge.Release()

	data, err := io.ReadAll(recorder.Result().Body)
	require.NoError(t, err)

	return response{status: recorder.Code, body: data}
}

// fillBound has other requests hold the in-flight bound until less than one
// MiB of it is free, each holding one MiB of decoding, and returns what gives
// it back.
func fillBound(t *testing.T) func() {
	t.Helper()

	var holders []*protocol.Charge

	for {
		holder := &protocol.Charge{}
		protocol.WithCharge(context.Background(), holder)

		_, err := holder.HoldDecode(protocol.FreeDecode + 1)
		if err != nil {
			holder.Release()

			break
		}

		holders = append(holders, holder)
	}

	require.NotEmpty(t, holders)

	return func() {
		for _, holder := range holders {
			holder.Release()
		}
	}
}

// Test_HealthBodies_HoldTheInFlightBound is SQ-320. A body the internal API
// decodes holds the in-flight bound with what decoding it may allocate, so
// many wide bodies wait for room in turn instead of all being decoded at once:
// 64 POST /_localstack/health bodies of 1 MiB of {} took the process to 1,310
// MiB (SQ-317). With the bound full, a 40 KiB body of {} (below the body's own
// free allowance, but not its decoding's) is answered 503 and changes nothing;
// once there is room it is served. Every path gives back all it held.
func Test_HealthBodies_HoldTheInFlightBound(t *testing.T) {
	protocol.SetInFlight(protocol.DefaultInFlight)
	protocol.SetBodyWait(100 * time.Millisecond)
	t.Cleanup(func() {
		protocol.SetInFlight(0)
		protocol.SetBodyWait(protocol.DefaultBodyWait)
	})

	wide := `{"action":"restart","a":[` + strings.Repeat("{},", 40<<10/3) + "{}]}"

	for _, tt := range []struct{ method, path string }{
		{http.MethodPost, healthPath},
		{http.MethodPut, healthPath},
	} {
		t.Run(tt.method, func(t *testing.T) {
			h := newHarness(t, nil)

			release := fillBound(t)

			got := h.doCharged(t, tt.method, tt.path, wide)
			assert.Equal(t, http.StatusServiceUnavailable, got.status, string(got.body))

			restarts, _ := h.actions.counts()
			assert.Zero(t, restarts, "a refused body changes nothing")

			release()

			got = h.doCharged(t, tt.method, tt.path, wide)
			assert.Equal(t, http.StatusOK, got.status, string(got.body))

			_, held := protocol.InFlight()
			assert.Zero(t, held, "every request gives back what it held")
		})
	}

	t.Run("bodies at once share the bound", func(t *testing.T) {
		h := newHarness(t, nil)

		var wg sync.WaitGroup

		for range 16 {
			wg.Go(func() {
				got := h.doCharged(t, http.MethodPost, healthPath, wide)
				assert.Contains(t, []int{http.StatusOK, http.StatusServiceUnavailable}, got.status)
			})
		}

		wg.Wait()

		_, held := protocol.InFlight()
		assert.Zero(t, held, "every request gives back what it held")
	})
}
