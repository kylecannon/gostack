package s3

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// versionIDAlphabet is the alphabet of a version id: base64 with "." and "_"
// for "+" and "/".
const versionIDAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._"

// TestGeneratedVersionIDsSortByTheOrderTheyWereMadeIn is what ListObjectVersions
// relies on to resume after a version that has since been deleted: versionOlder
// decodes the ids, because their characters are not in byte order.
func TestGeneratedVersionIDsSortByTheOrderTheyWereMadeIn(t *testing.T) {
	const count = 500

	ids := make([]string, count)
	for i := range ids {
		ids[i] = generateVersionID()

		require.Len(t, ids[i], 32)
		assert.Empty(t, strings.Trim(ids[i], versionIDAlphabet), "every character is in the alphabet")
	}

	for i := 1; i < count; i++ {
		assert.True(t, versionOlder(ids[i-1], ids[i]), "id %d was made before id %d", i-1, i)
		assert.False(t, versionOlder(ids[i], ids[i-1]), "id %d was made after id %d", i, i-1)
	}
}

func TestLockActive(t *testing.T) {
	now := time.Now()
	later, earlier := now.Add(time.Hour), now.Add(-time.Hour)

	tests := []struct {
		name       string
		give       Lock
		giveBypass bool
		want       bool
	}{
		{"nothing", Lock{}, false, false},
		{"a retention in force", Lock{Mode: lockGovernance, Until: later}, false, true},
		{"a retention that has ended", Lock{Mode: lockCompliance, Until: earlier}, false, false},
		{"bypass lifts GOVERNANCE", Lock{Mode: lockGovernance, Until: later}, true, false},
		{"bypass does not lift COMPLIANCE", Lock{Mode: lockCompliance, Until: later}, true, true},
		{"a legal hold", Lock{LegalHold: legalHoldOn}, false, true},
		{"bypass does not lift a legal hold", Lock{Mode: lockGovernance, Until: later, LegalHold: legalHoldOn}, true, true},
		{"a legal hold turned off", Lock{LegalHold: legalHoldOff}, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.give.active(now, tt.giveBypass))
		})
	}
}
