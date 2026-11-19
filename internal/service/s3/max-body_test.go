package s3_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	cfgMaxMessageLengthExceeded = "MaxMessageLengthExceeded"
	cfgSmallLimit               = 1024
)

// setMaxBody sets protocol.MaxBody for one test.
func setMaxBody(t *testing.T, limit int64) {
	t.Helper()

	t.Cleanup(func() { protocol.SetMaxBody(protocol.DefaultMaxBody) })
	protocol.SetMaxBody(limit)
}

// taggingOfSize is a valid Tagging document of exactly size bytes: one tag and
// white space between the elements.
func taggingOfSize(t *testing.T, size int) string {
	t.Helper()

	bare := len(cfgTagging(cfgTag("k", "v")))
	require.GreaterOrEqual(t, size, bare)

	return cfgTagging(cfgTag("k", "v") + strings.Repeat(" ", size-bare))
}

// TestXMLBodyOverMaxBodyAnswersMaxMessageLengthExceeded: the generated reader of
// an XML payload stops at protocol.MaxBody. A document of exactly that size is
// decoded, and one byte more answers 400 MaxMessageLengthExceeded in S3's error
// XML, not MalformedXML.
func TestXMLBodyOverMaxBodyAnswersMaxMessageLengthExceeded(t *testing.T) {
	setMaxBody(t, cfgSmallLimit)

	s := cfgBucket(t)

	cfgExpect(t, cfgPut(s, bucketAPath+"?tagging", taggingOfSize(t, cfgSmallLimit), nil), http.StatusNoContent, "")

	got := cfgPut(s, bucketAPath+"?tagging", taggingOfSize(t, cfgSmallLimit+1), nil)
	cfgExpect(t, got, http.StatusBadRequest, cfgMaxMessageLengthExceeded)
	assert.Equal(t, "Your request was too big.", got.err(t).Message)
	assert.Contains(t, string(got.body), "<Error>")

	// The refused document changed nothing.
	read := cfgGet(s, bucketAPath+"?tagging")
	require.Equal(t, http.StatusOK, read.status, string(read.body))
	assert.Contains(t, string(read.body), "<Key>k</Key>")
}

// TestXMLBodyOverTheDefaultMaxBody: with GOSTACK_MAX_REQUEST_BODY unset the cap
// is 16 MiB.
func TestXMLBodyOverTheDefaultMaxBody(t *testing.T) {
	s := cfgBucket(t)

	limit := int(protocol.DefaultMaxBody)

	cfgExpect(t, cfgPut(s, bucketAPath+"?tagging", taggingOfSize(t, limit), nil), http.StatusNoContent, "")
	cfgExpect(t, cfgPut(s, bucketAPath+"?tagging", taggingOfSize(t, limit+1), nil),
		http.StatusBadRequest, cfgMaxMessageLengthExceeded)
}

// TestXMLBodyWithNoMaxBody: a limit of 0 is LocalStack's, none, so a document
// over 16 MiB is decoded.
func TestXMLBodyWithNoMaxBody(t *testing.T) {
	setMaxBody(t, 0)

	s := cfgBucket(t)

	cfgExpect(t, cfgPut(s, bucketAPath+"?tagging", taggingOfSize(t, int(protocol.DefaultMaxBody)+1), nil),
		http.StatusNoContent, "")
}

// TestStreamedPutObjectIsNotCapped: an object body is streamed, so the limit on
// the bodies that are decoded does not apply to it.
func TestStreamedPutObjectIsNotCapped(t *testing.T) {
	setMaxBody(t, cfgSmallLimit)

	s := newServer(t)
	createBucket(t, s)

	body := bytes.Repeat([]byte("0123456789abcdef"), (8*cfgSmallLimit)/16)
	require.Greater(t, len(body), cfgSmallLimit)

	put := s.do(call{method: http.MethodPut, target: "/bucket/streamed", region: regionUSEast1, body: body})
	require.Equal(t, http.StatusOK, put.status, string(put.body))

	got := s.do(call{method: http.MethodGet, target: "/bucket/streamed", region: regionUSEast1})
	require.Equal(t, http.StatusOK, got.status)
	assert.True(t, bytes.Equal(body, got.body))
}
