package s3_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestBucketPolicyPastTwentyKBIsRefusedBeforeParsing is SQ-320. A bucket
// policy is held to S3's 20 KB ("Bucket policies are limited to 20 KB in
// size") before it is parsed: a 16 MiB policy of distinct short keys took the
// process to 270 MiB (SQ-317). A policy of exactly 20 KiB is stored.
func TestBucketPolicyPastTwentyKBIsRefusedBeforeParsing(t *testing.T) {
	s := cfgBucket(t)

	// keysOfLength is a json object of distinct keys, exactly n bytes long.
	keysOfLength := func(n int) string {
		var b strings.Builder

		b.WriteString(`{"Statement":[]`)

		for i := 0; b.Len() < n-20; i++ {
			b.WriteString(`,"` + strconv.Itoa(i) + `":0`)
		}

		b.WriteString(strings.Repeat(" ", n-b.Len()-1) + "}")

		return b.String()
	}

	limit := keysOfLength(20 << 10)
	cfgExpect(t, cfgPut(s, bucketAPath+"?policy", limit, nil), http.StatusNoContent, "")

	for _, n := range []int{20<<10 + 1, 16<<20 - 1024} {
		got := cfgPut(s, bucketAPath+"?policy", keysOfLength(n), nil)
		cfgExpect(t, got, http.StatusBadRequest, "MalformedPolicy")
		assert.Contains(t, string(got.body), "Policy exceeds the maximum allowed document size.", "%d bytes", n)
	}

	stored := cfgGet(s, bucketAPath+"?policy")
	cfgExpect(t, stored, http.StatusOK, "")
	assert.Equal(t, limit, string(stored.body), "a refused policy does not replace the stored one")
}
