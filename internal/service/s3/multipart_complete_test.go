package s3_test

import (
	"bytes"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two uploads of one key, each completed with If-None-Match: * at once, race
// for the key. The condition is checked again under the bucket's write lock, so
// exactly one creates it.
func TestConcurrentConditionalCompletesCreateTheKeyOnce(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	// Parts this size keep the copy long enough for both completions to pass the
	// early check before either commits.
	body := bytes.Repeat([]byte("x"), 24*mib)
	ids := []string{startUpload(t, s, multipartKey, nil), startUpload(t, s, multipartKey, nil)}
	etags := []string{putPart(t, s, ids[0], 1, body, nil), putPart(t, s, ids[1], 1, body, nil)}

	replies := make([]reply, len(ids))

	var wg sync.WaitGroup

	for i := range ids {
		wg.Go(func() {
			replies[i] = complete(s, ids[i], completeBody([]string{etags[i]}), map[string]string{headerIfNoneMatch: "*"})
		})
	}

	wg.Wait()

	statuses := []int{replies[0].status, replies[1].status}
	assert.ElementsMatch(t, []int{http.StatusOK, http.StatusPreconditionFailed}, statuses,
		"%s / %s", replies[0].body, replies[1].body)

	// The refused completion left its upload, so it can still finish without the
	// condition.
	loser := 0
	if replies[0].status == http.StatusOK {
		loser = 1
	}

	assert.Equal(t, "PreconditionFailed", replies[loser].err(t).Code)

	retry := complete(s, ids[loser], completeBody([]string{etags[loser]}), nil)
	assert.Equal(t, http.StatusOK, retry.status, string(retry.body))
}

func TestConditionalCompleteAgainstAnExistingKey(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	id := startUpload(t, s, multipartKey, nil)
	etag := putPart(t, s, id, 1, []byte("part"), nil)

	require.Equal(t, http.StatusOK, s.do(call{
		method: http.MethodPut, target: multipartKey, region: regionUSEast1, body: []byte("first"),
	}).status)

	got := complete(s, id, completeBody([]string{etag}), map[string]string{headerIfNoneMatch: "*"})
	require.Equal(t, http.StatusPreconditionFailed, got.status, string(got.body))
	assert.Equal(t, "PreconditionFailed", got.err(t).Code)
	assert.Equal(t, "first", string(s.do(call{method: http.MethodGet, target: multipartKey, region: regionUSEast1}).body))
}
