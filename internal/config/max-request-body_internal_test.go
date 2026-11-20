package config

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_parseMaxRequestBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		give string
		want int64
	}{
		{"", 16 << 20},
		{"0", 0},
		{"1", 1},
		{"16777216", 16 << 20},
		{"1KiB", 1 << 10},
		{"16MiB", 16 << 20},
		{"2GiB", 2 << 30},
		{"0MiB", 0},
		{"16mib", 16 << 20},
		{"16 MiB", 16 << 20},
	}

	for _, tt := range tests {
		got, err := parseMaxRequestBody(tt.give)
		require.NoError(t, err, tt.give)
		assert.Equal(t, tt.want, got, tt.give)
	}

	a, _ := parseMaxRequestBody("16MiB")
	b, _ := parseMaxRequestBody("16777216")
	assert.Equal(t, a, b)
}

func Test_parseMaxRequestBody_invalid(t *testing.T) {
	t.Parallel()

	for _, give := range []string{
		"abc", "-1", "1.5MiB", "MiB", "16TiB", "16MB", "1e3", "0x10", "1_000",
		"9223372036854775808",              // one over an int64
		"8589934592GiB",                    // 2^33 GiB overflows int64
		"9007199254740992" + "000" + "KiB", // far over
		"  ", "16 M iB",
	} {
		_, err := parseMaxRequestBody(give)
		require.Error(t, err, give)
	}

	// The largest value that fits.
	got, err := parseMaxRequestBody("8589934591GiB")
	require.NoError(t, err)
	assert.Equal(t, int64(8589934591)<<30, got)
	assert.Less(t, got, int64(math.MaxInt64))
}

func Test_Load_maxRequestBody(t *testing.T) {
	isolate(t)
	require.Equal(t, int64(16<<20), mustLoad(t).Settings().MaxRequestBody)

	for give, want := range map[string]int64{
		"1048576": 1 << 20,
		"1MiB":    1 << 20,
		"64KiB":   64 << 10,
		"1GiB":    1 << 30,
		"0":       0,
		"banana":  16 << 20, // a value that cannot be parsed keeps the default
		"-5":      16 << 20,
	} {
		t.Setenv("GOSTACK_MAX_REQUEST_BODY", give)
		assert.Equal(t, want, mustLoad(t).Settings().MaxRequestBody, give)
	}
}

// Test_Load_maxRequestBody_onlyTheGostackForm: the setting is gostack's own, so
// the LocalStack and bare spellings are ignored.
func Test_Load_maxRequestBody_onlyTheGostackForm(t *testing.T) {
	isolate(t)
	t.Setenv("LOCALSTACK_MAX_REQUEST_BODY", "1")
	t.Setenv("MAX_REQUEST_BODY", "1")

	require.Equal(t, int64(16<<20), mustLoad(t).Settings().MaxRequestBody)
}

// Test_Set_maxRequestBody_isNotApplied: the limit is read at start-up, so a
// runtime update leaves the snapshot as it was.
func Test_Set_maxRequestBody_isNotApplied(t *testing.T) {
	isolate(t)

	cfg := mustLoad(t)
	require.NoError(t, cfg.Set("MAX_REQUEST_BODY", "1"))
	assert.Equal(t, int64(16<<20), cfg.Settings().MaxRequestBody)
}

func Test_Load_requestBodyIdleTimeout(t *testing.T) {
	isolate(t)
	require.Equal(t, 20*time.Second, mustLoad(t).Settings().RequestBodyIdleTimeout)

	for give, want := range map[string]time.Duration{
		"5":      5 * time.Second,
		"0":      0,
		"20s":    20 * time.Second, // seconds only, as SHUTDOWN_TIMEOUT: anything else keeps the default
		"banana": 20 * time.Second,
		"-5":     20 * time.Second,
	} {
		t.Setenv("GOSTACK_REQUEST_BODY_IDLE_TIMEOUT", give)
		assert.Equal(t, want, mustLoad(t).Settings().RequestBodyIdleTimeout, give)
	}

	t.Setenv("GOSTACK_REQUEST_BODY_IDLE_TIMEOUT", "")
	t.Setenv("REQUEST_BODY_IDLE_TIMEOUT", "1")
	assert.Equal(t, 20*time.Second, mustLoad(t).Settings().RequestBodyIdleTimeout, "only the GOSTACK_ form")
}
