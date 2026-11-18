package s3_test

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/blob"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadWait is how long a test lets a live load run while a request is held
// between dropping a saved body and removing it. A load that finishes in that
// time ran inside the gap.
const loadWait = 100 * time.Millisecond

// heldRemove is a blob store whose first Remove waits, once entered is
// closed, until release is: it holds a request between dropping a body from
// the state and removing it.
type heldRemove struct {
	blob.Store

	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func holdFirstRemove(store blob.Store) *heldRemove {
	return &heldRemove{Store: store, entered: make(chan struct{}), release: make(chan struct{})}
}

func (h *heldRemove) Remove(id string) error {
	first := false
	h.once.Do(func() { first = true })

	if first {
		close(h.entered)
		<-h.release
	}

	return h.Store.Remove(id) //nolint:wrapcheck // the test passes the store's error through as it is
}

// dropRace is one request that drops a saved body: setup builds and saves the
// state it drops from, and drop is the request.
type dropRace struct {
	name  string
	setup func(p *persisted) (drop func() reply)
}

// dropRaces are the requests that drop a body the saved state names: a delete,
// an overwrite, an abort, a part uploaded again, a completion and the delete of
// a bucket with an upload in progress.
func dropRaces() []dropRace {
	do := func(p *persisted, method, target string, body []byte) func() reply {
		return func() reply { return p.do(call{method: method, target: target, region: regionUSEast1, body: body}) }
	}
	upload := func(p *persisted) string {
		id := startUpload(p.t, p.server, multipartKey, nil)
		putPart(p.t, p.server, id, 1, []byte("part"), nil)

		return id
	}

	return []dropRace{
		{"delete", func(p *persisted) func() reply {
			p.put(savedKey, []byte("saved"))

			return do(p, http.MethodDelete, savedKey, nil)
		}},
		{"overwrite", func(p *persisted) func() reply {
			p.put(savedKey, []byte("saved"))

			return do(p, http.MethodPut, savedKey, []byte("new"))
		}},
		{"abort", func(p *persisted) func() reply {
			return do(p, http.MethodDelete, multipartKey+"?uploadId="+upload(p), nil)
		}},
		{"part again", func(p *persisted) func() reply {
			return do(p, http.MethodPut, multipartKey+"?partNumber=1&uploadId="+upload(p), []byte("again"))
		}},
		{"complete", func(p *persisted) func() reply {
			id := startUpload(p.t, p.server, multipartKey, nil)
			etag := putPart(p.t, p.server, id, 1, []byte("part"), nil)

			return func() reply { return complete(p.server, id, completeBody([]string{etag}), nil) }
		}},
		{"delete bucket", func(p *persisted) func() reply {
			upload(p)

			return do(p, http.MethodDelete, "/bucket", nil)
		}},
	}
}

// missingBodies are the ids S3's state names whose bodies are not on disk.
func missingBodies(t *testing.T, p *persisted) []string {
	t.Helper()

	assets, err := p.eng.Assets(s3Name)
	require.NoError(t, err)

	var missing []string

	for id := range p.provider.References() {
		f, err := assets.Open(id)
		if err != nil {
			missing = append(missing, id)

			continue
		}

		_ = f.Close()
	}

	return missing
}

// TestPersistedS3LiveLoadBetweenADropAndItsRemove holds each request that drops
// a saved body between the drop and the Remove, and runs a live load meanwhile.
// The load restores the saved state, which names the body again, so the next
// save must keep it: the load waits for the Remove, which the load's sweep then
// takes back. Run in the gap instead, the load takes nothing back, the late
// Remove queues the body and the save deletes it, so the state names a body
// that is gone, live and after a restart.
func TestPersistedS3LiveLoadBetweenADropAndItsRemove(t *testing.T) {
	for _, race := range dropRaces() {
		t.Run(race.name, func(t *testing.T) {
			dir := t.TempDir()
			p := startPersisted(t, dir)
			createBucket(t, p.server)
			drop := race.setup(p)
			p.flush()

			assets, err := p.eng.Assets(s3Name)
			require.NoError(t, err)

			held := holdFirstRemove(persist.NewBlobStore(assets))
			p.provider.UseBlobs(held)

			dropped := make(chan reply, 1)
			go func() { dropped <- drop() }()

			<-held.entered

			loaded := make(chan error, 1)
			go func() { loaded <- p.eng.Load(t.Context()) }()

			select {
			case err = <-loaded:
				loaded <- err
			case <-time.After(loadWait):
			}

			close(held.release)

			got := <-dropped
			assert.Less(t, got.status, http.StatusMultipleChoices, string(got.body))
			require.NoError(t, <-loaded)

			p.put("/bucket/other", []byte("x"))
			p.flush()
			assert.Empty(t, missingBodies(t, p), "the live state names bodies the save deleted")

			second := startPersisted(t, dir)
			assert.Empty(t, missingBodies(t, second), "the restarted state names bodies that are gone")
		})
	}
}

// TestPersistedS3LiveLoadAfterARemove is the order the transfer lock keeps:
// each drop removes its body before a live load runs, the load restores the
// saved state that names it, and the save keeps it.
func TestPersistedS3LiveLoadAfterARemove(t *testing.T) {
	for _, race := range dropRaces() {
		t.Run(race.name, func(t *testing.T) {
			dir := t.TempDir()
			p := startPersisted(t, dir)
			createBucket(t, p.server)
			drop := race.setup(p)
			p.flush()

			got := drop()
			assert.Less(t, got.status, http.StatusMultipleChoices, string(got.body))
			require.NoError(t, p.eng.Load(t.Context()))

			p.put("/bucket/other", []byte("x"))
			p.flush()
			assert.Empty(t, missingBodies(t, p))

			second := startPersisted(t, dir)
			assert.Empty(t, missingBodies(t, second))
		})
	}
}
