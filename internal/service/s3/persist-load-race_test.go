package s3_test

import (
	"encoding/xml"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// raceFor is how long a race test keeps trying before it calls the run clean.
	raceFor = 2 * time.Second
	// stormKeys is the number of keys the storm writes to.
	stormKeys = 12
	// stormRounds is how many steps each worker of the storm takes, so the test
	// costs the same work on every machine.
	stormRounds = 100
)

// afterRestore is S3 as the engine sees it, with a step that runs once right
// after the next Restore returns: a request that lands as the transfer lock is
// released, before the engine reads the state for its sweep.
type afterRestore struct {
	*s3.Provider

	then func()
}

func (a *afterRestore) Restore(r io.Reader) error {
	err := a.Provider.Restore(r)

	then := a.then
	a.then = nil

	if then != nil {
		then()
	}

	return err //nolint:wrapcheck // the test passes S3's error through as it is
}

// TestPersistedS3LiveLoadKeepsBodiesTheSavedStateNames deletes a saved object
// right after a load's Restore returns. The state file on disk still names the
// body, so the load must keep it until the next save, and a crash before that
// save brings the object back whole.
func TestPersistedS3LiveLoadKeepsBodiesTheSavedStateNames(t *testing.T) {
	dir := t.TempDir()
	hook := &afterRestore{}
	first := startPersistedAs(t, dir, func(p *s3.Provider) persist.Snapshotter {
		hook.Provider = p

		return hook
	})

	createBucket(t, first.server)
	first.put(savedKey, []byte("saved"))
	first.flush()

	hook.then = func() {
		assert.Equal(t, http.StatusNoContent,
			first.do(call{method: http.MethodDelete, target: savedKey, region: regionUSEast1}).status)
	}
	require.NoError(t, first.eng.Load(t.Context()))

	assert.Equal(t, 1, first.blobFiles(), "the state file on disk names the body")

	// A crash: the first is never flushed or closed.
	second := startPersisted(t, dir)
	got := second.get(savedKey)
	assert.Equal(t, http.StatusOK, got.status, string(got.body))
	assert.Equal(t, "saved", string(got.body))
}

// TestPersistedS3LiveLoadKeepsAnAcknowledgedPut writes into a bucket of the
// saved state while loads run. No PUT that answers 200 reads back as 500: a
// load never deletes a body that a write is committing.
func TestPersistedS3LiveLoadKeepsAnAcknowledgedPut(t *testing.T) {
	p := startPersisted(t, t.TempDir())
	createBucket(t, p.server)
	p.flush()

	stop, loads := make(chan struct{}), make(chan struct{})

	go func() {
		defer close(loads)

		for {
			select {
			case <-stop:
				return
			default:
				_ = p.eng.Load(t.Context())
			}
		}
	}()

	lost := 0

	for i, deadline := 0, time.Now().Add(raceFor); time.Now().Before(deadline) && lost == 0; i++ {
		key := "/bucket/k" + strconv.Itoa(i)

		put := p.do(call{method: http.MethodPut, target: key, region: regionUSEast1, body: []byte("v")})
		if put.status != http.StatusOK {
			continue // NoSuchBucket: the PUT found a bucket a load replaced.
		}

		// A load in between may restore a state without the key, which answers
		// 404; only a key whose body is gone answers 500.
		got := p.get(key)
		if got.status == http.StatusInternalServerError {
			lost++

			t.Logf("PUT %s answered 200, GET answered %d: %s", key, got.status, got.body)
		}
	}

	close(stop)
	<-loads

	assert.Zero(t, lost)
}

// TestPersistedS3LeavesALocalstackPersistBucketAlone seeds the data directory
// as localstack-persist lays out S3, <base>/s3/assets/<bucket>/<key>@<version>,
// with buckets named tmp and blobs. A persisted start, saves, a delete and a
// load leave their files as they were.
func TestPersistedS3LeavesALocalstackPersistBucketAlone(t *testing.T) {
	dir := t.TempDir()
	foreign := []string{
		filepath.Join(dir, s3Name, "assets", "tmp", "report.csv@null"),
		filepath.Join(dir, s3Name, "assets", "blobs", "data.bin@null"),
	}

	for _, path := range foreign {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte("user data"), 0o600))
	}

	p := startPersisted(t, dir)
	createBucket(t, p.server)
	p.put(savedKey, []byte("saved"))
	p.flush()
	require.Equal(t, http.StatusNoContent,
		p.do(call{method: http.MethodDelete, target: savedKey, region: regionUSEast1}).status)
	p.flush()
	require.NoError(t, p.eng.Load(t.Context()))

	for _, path := range foreign {
		got, err := os.ReadFile(path)
		if assert.NoError(t, err, "a file gostack never wrote is gone") {
			assert.Equal(t, "user data", string(got))
		}
	}
}

type stormVersion struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId"`
}

type stormVersions struct {
	Versions []stormVersion `xml:"Version"`
}

type stormList struct {
	Keys []string `xml:"Contents>Key"`
}

type stormUpload struct {
	Key string `xml:"Key"`
	ID  string `xml:"UploadId"`
}

type stormUploads struct {
	Uploads []stormUpload `xml:"Upload"`
}

// assertStormSettled checks that the listing agrees with HEAD for every key,
// that every listed version reads, and that every listed upload lists its parts.
func assertStormSettled(t *testing.T, p *persisted, label string) {
	t.Helper()

	var listed stormList
	require.NoError(t, xml.Unmarshal(p.get("/bucket?list-type=2&max-keys=1000").body, &listed))

	keys := make(map[string]bool, len(listed.Keys))
	for _, key := range listed.Keys {
		keys[key] = true
	}

	for i := range stormKeys {
		key := "k" + strconv.Itoa(i)
		head := p.do(call{method: http.MethodHead, target: "/bucket/" + key, region: regionUSEast1}).status
		assert.Equal(t, head == http.StatusOK, keys[key], "%s: %s answers HEAD %d", label, key, head)
	}

	var versions stormVersions
	require.NoError(t, xml.Unmarshal(p.get("/bucket?versions").body, &versions))

	for _, v := range versions.Versions {
		got := p.get("/bucket/" + v.Key + "?versionId=" + v.VersionID)
		assert.Equal(t, http.StatusOK, got.status, "%s: %s@%s %s", label, v.Key, v.VersionID, got.body)
	}

	var uploads stormUploads
	require.NoError(t, xml.Unmarshal(p.get("/bucket?uploads").body, &uploads))

	for _, upload := range uploads.Uploads {
		got := p.get("/bucket/" + upload.Key + "?uploadId=" + upload.ID)
		assert.Equal(t, http.StatusOK, got.status, "%s: ListParts %s %s", label, upload.ID, got.body)
	}
}

// uploadInStorm starts an upload, uploads one part, and completes two in three.
func uploadInStorm(p *persisted, r *rand.Rand, key string) {
	got := p.do(call{method: http.MethodPost, target: key + "?uploads", region: regionUSEast1})
	if got.status != http.StatusOK {
		return
	}

	var started struct {
		UploadID string `xml:"UploadId"`
	}

	_ = xml.Unmarshal(got.body, &started)

	part := p.do(call{
		method: http.MethodPut, target: key + "?partNumber=1&uploadId=" + started.UploadID,
		region: regionUSEast1, body: []byte("part"),
	})
	if part.status != http.StatusOK || r.IntN(3) == 0 {
		return // Some uploads stay in progress.
	}

	p.do(call{
		method: http.MethodPost, target: key + "?uploadId=" + started.UploadID,
		region: regionUSEast1, body: completeBody([]string{part.header.Get("ETag")}),
	})
}

// TestPersistedS3StormOfWritesAndLoads runs PUT, versioned DELETE, CopyObject
// and multipart uploads against live loads and saves. Run with -race, it shows
// that no load reads a bucket a request is writing. Afterwards the listings
// agree with the objects, live and after a crash.
func TestPersistedS3StormOfWritesAndLoads(t *testing.T) {
	dir := t.TempDir()
	p := startPersisted(t, dir)
	createBucket(t, p.server)
	p.put(versionsPath, versioningBody("Enabled"))
	p.flush()

	var workers sync.WaitGroup

	key := func(r *rand.Rand) string { return fmt.Sprintf("/bucket/k%d", r.IntN(stormKeys)) }
	work := func(step func(r *rand.Rand)) {
		workers.Go(func() {
			r := rand.New(rand.NewPCG(rand.Uint64(), 1))

			for range stormRounds {
				step(r)
			}
		})
	}

	work(func(r *rand.Rand) {
		p.do(call{method: http.MethodPut, target: key(r), region: regionUSEast1, body: make([]byte, r.IntN(4096))})
	})
	work(func(r *rand.Rand) { p.do(call{method: http.MethodDelete, target: key(r), region: regionUSEast1}) })
	work(func(r *rand.Rand) {
		p.do(call{
			method: http.MethodPut, target: key(r), region: regionUSEast1,
			header: map[string]string{headerCopySource: key(r)},
		})
	})
	work(func(r *rand.Rand) { uploadInStorm(p, r, key(r)) })
	work(func(r *rand.Rand) {
		if r.IntN(2) == 0 {
			_ = p.eng.Load(t.Context())
		} else {
			_ = p.eng.Flush(t.Context())
		}
	})

	workers.Wait()

	assertStormSettled(t, p, "live")
	p.flush()
	assertStormSettled(t, startPersisted(t, dir), "after a crash")
}
