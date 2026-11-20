package kms

import (
	"bytes"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/require"
)

// Test_Snapshot_anyValidTo saves a key whose ValidTo RFC 3339 cannot write:
// past year 9999, before year 0, and a last second of 9999 kept in a zone
// east of UTC, where it is already year 10000. Each snapshot is written and
// restores the same instant. Before, encoding/json/v2 refused such a time and
// every KMS save failed from then on.
func Test_Snapshot_anyValidTo(t *testing.T) {
	ctx := request.With(t.Context(), &request.Context{AccountID: "000000000000", Region: testRegion})

	for name, validTo := range map[string]time.Time{
		"after 9999":  time.Unix(300000000000, 0),
		"before 0":    time.Unix(-100000000000, 5),
		"east of UTC": time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC).In(time.FixedZone("east", 14*60*60)),
	} {
		t.Run(name, func(t *testing.T) {
			p := New(nil)
			id := newImportedKey(ctx, t, p, specSymmetric, "ENCRYPT_DECRYPT", bytes.Repeat([]byte{1}, 32)).id

			k := p.stateAt("000000000000", testRegion).key(*id)
			k.mu.Lock()
			k.validTo = validTo
			k.mu.Unlock()

			var saved bytes.Buffer
			require.NoError(t, p.Snapshot(&saved))

			restored := New(nil)
			require.NoError(t, restored.Restore(bytes.NewReader(saved.Bytes())))

			got := restored.stateAt("000000000000", testRegion).key(*id)
			require.NotNil(t, got)
			require.True(t, validTo.Equal(got.validTo), "restored %v, saved %v", got.validTo, validTo)
		})
	}
}
