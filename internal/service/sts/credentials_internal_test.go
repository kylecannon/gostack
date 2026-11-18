package sts

import (
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/account"
	"github.com/stretchr/testify/assert"
)

func Test_newAccessKeyID(t *testing.T) {
	tests := []struct {
		name       string
		giveID     string
		giveParity bool
		wantPrefix string
	}{
		{name: "zero", giveID: "000000000000", wantPrefix: prefixLocal},
		{name: "one, the first odd account", giveID: "000000000001", wantPrefix: prefixLocal},
		{name: "even", giveID: "123456789012", wantPrefix: prefixLocal},
		{name: "odd", giveID: "123456789013", wantPrefix: prefixLocal},
		{name: "the largest", giveID: "999999999999", wantPrefix: prefixLocal},
		{name: "even under parity", giveID: "000000000002", giveParity: true, wantPrefix: prefixProduction},
		{name: "odd under parity", giveID: "000000000003", giveParity: true, wantPrefix: prefixProduction},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The tail and the marker character are random, so go round many times.
			for range 200 {
				key := newAccessKeyID(tt.giveID, tt.giveParity)

				assert.Len(t, key, 20)
				assert.Equal(t, tt.wantPrefix, key[:4])
				assert.Equal(t, tt.giveID, account.FromAccessKey(key, tt.giveParity), key)
				// With the other parity the key must not decode.
				assert.Equal(t, account.Default, account.FromAccessKey(key, !tt.giveParity), key)
			}
		})
	}
}

func Test_sessions(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	live := session{userID: "u", account: "000000000000", arn: "a", expires: now.Add(time.Hour)}

	t.Run("a live session is found", func(t *testing.T) {
		s := newSessions()
		s.put("k", live, now)

		got, ok := s.get("k", now)

		assert.True(t, ok)
		assert.Equal(t, live, got)
	})

	t.Run("an expired session is not", func(t *testing.T) {
		s := newSessions()
		s.put("k", live, now)

		_, ok := s.get("k", now.Add(time.Hour))

		assert.False(t, ok)
	})

	t.Run("an unknown key is not", func(t *testing.T) {
		_, ok := newSessions().get("nope", now)

		assert.False(t, ok)
	})

	t.Run("expired sessions are swept once enough pile up", func(t *testing.T) {
		s := newSessions()
		expired := session{expires: now.Add(-time.Minute)}

		for i := range minSweep {
			s.put(string(rune('a'))+time.Duration(i).String(), expired, now)
		}

		assert.Len(t, s.byKey, minSweep)

		s.put("fresh", live, now)

		assert.Len(t, s.byKey, 1)
	})
}
