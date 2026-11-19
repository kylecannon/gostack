package s3_test

import (
	"crypto/rand"
	"crypto/sha256"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/blob"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	s3Name     = "s3"
	savedKey   = objectPath
	codeNoKey  = "NoSuchKey"
	partOneLen = 5 << 20
)

// persisted is a gateway serving an S3 persisted under dir, as gostack wires
// it: the assets open after Register and before Load, and S3 takes them after
// Load. The engine never flushes by itself, so a test saves with Flush, and a
// crash is a second persisted on the same dir with the first never closed.
type persisted struct {
	*server

	eng      *persist.Engine
	provider *s3.Provider
	dir      string
	handler  http.Handler
}

func startPersisted(t *testing.T, dir string) *persisted {
	t.Helper()

	return startPersistedAs(t, dir, nil)
}

// startPersistedAs is startPersisted with the engine seeing S3 through as, so a
// test can act between the engine's calls. A nil as is S3 itself.
func startPersistedAs(t *testing.T, dir string, as func(*s3.Provider) persist.Snapshotter) *persisted {
	t.Helper()

	eng := persist.New(persist.Config{
		PersistVarSet: true, BaseDir: dir, Frequency: time.Hour,
		MountDetector: func(string) bool { return false },
	})

	provider := s3.New(func() s3.Settings { return s3.Settings{LocalstackHost: localstackHost} },
		blob.NewEphemeral(t.TempDir()))
	t.Cleanup(provider.Close)

	reg := provider.Registration()
	if as != nil {
		reg.Persist = as(provider)
	}

	eng.Register(s3Name, reg.Persist)

	assets, err := eng.Assets(s3Name)
	require.NoError(t, err)
	require.NotNil(t, assets)
	require.NoError(t, eng.Load(t.Context()))
	provider.UseBlobs(persist.NewBlobStore(assets))

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{benchListen}} },
		[]service.Registration{reg}, nil, eng,
	)
	require.NoError(t, err)

	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)

	return &persisted{
		server: &server{t: t, url: srv.URL}, eng: eng, provider: provider, dir: dir, handler: gw.Handler(),
	}
}

func (p *persisted) flush() {
	p.t.Helper()
	require.NoError(p.t, p.eng.Flush(p.t.Context()))
}

// blobsDir is where the bodies are on disk.
func (p *persisted) blobsDir() string {
	return filepath.Join(p.dir, s3Name, "assets", ".gostack", "blobs")
}

// blobFiles counts the bodies on disk.
func (p *persisted) blobFiles() int {
	p.t.Helper()

	count := 0
	err := filepath.WalkDir(p.blobsDir(), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			count++
		}

		return err
	})
	require.NoError(p.t, err)

	return count
}

func (p *persisted) put(target string, body []byte) {
	p.t.Helper()

	got := p.do(call{method: http.MethodPut, target: target, region: regionUSEast1, body: body})
	require.Less(p.t, got.status, http.StatusMultipleChoices, string(got.body))
}

func (p *persisted) get(target string) reply {
	p.t.Helper()

	return p.do(call{method: http.MethodGet, target: target, region: regionUSEast1})
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()

	data := make([]byte, n)
	_, err := rand.Read(data)
	require.NoError(t, err)

	return data
}

type versionEntry struct {
	VersionID string `xml:"VersionId"`
}

type versionList struct {
	Versions []versionEntry `xml:"Version"`
	Markers  []versionEntry `xml:"DeleteMarker"`
}

type partEntry struct {
	Number int `xml:"PartNumber"`
}

type partList struct {
	Parts []partEntry `xml:"Part"`
}

// TestPersistedS3SurvivesARestart saves bodies of three sizes, a versioned key
// with two versions under a delete marker, the bucket's versioning and tags,
// and an upload with two parts, then starts over on the same directory with the
// first never closed: everything is back, byte for byte, the upload completes,
// and no body is left that the state does not name.
func TestPersistedS3SurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	first := startPersisted(t, dir)

	createBucket(t, first.server)
	first.put("/bucket?tagging", []byte(`<Tagging><TagSet><Tag><Key>team</Key><Value>x</Value></Tag></TagSet></Tagging>`))

	bodies := map[string][]byte{
		"/bucket/body-1KiB": randomBytes(t, 1<<10),
		"/bucket/body-1MiB": randomBytes(t, 1<<20),
		"/bucket/body-3MiB": randomBytes(t, 3<<20),
	}
	for target, body := range bodies {
		first.put(target, body)
	}

	first.put(versionsPath, versioningBody("Enabled"))
	first.put(savedKey, []byte("one"))
	first.put(savedKey, []byte("two"))
	require.Equal(t, http.StatusNoContent,
		first.do(call{method: http.MethodDelete, target: savedKey, region: regionUSEast1}).status)

	upload := startUpload(t, first.server, multipartKey, nil)
	partOne, partTwo := randomBytes(t, partOneLen), randomBytes(t, 1<<10)
	etags := []string{
		putPart(t, first.server, upload, 1, partOne, nil),
		putPart(t, first.server, upload, 2, partTwo, nil),
	}

	first.flush()

	second := startPersisted(t, dir)

	for target, body := range bodies {
		got := second.get(target)
		require.Equal(t, http.StatusOK, got.status, string(got.body))
		assert.Equal(t, sha256.Sum256(body), sha256.Sum256(got.body), target)
	}

	assert.Equal(t, codeNoKey, second.get(savedKey).err(t).Code, "the delete marker is current")

	versions := decodeXML[versionList](t, second.get("/bucket?versions&prefix=key"))
	assert.Len(t, versions.Versions, 2)
	assert.Len(t, versions.Markers, 1)

	got := second.get(savedKey + "?versionId=" + versions.Versions[0].VersionID)
	require.Equal(t, http.StatusOK, got.status)
	assert.Equal(t, "two", string(got.body), "the newest version lists first")

	assert.Contains(t, string(second.get(versionsPath).body), "<Status>Enabled</Status>")
	assert.Contains(t, string(second.get("/bucket?tagging").body), "<Key>team</Key>")

	parts := decodeXML[partList](t, second.get(multipartKey+"?uploadId="+upload))
	assert.Len(t, parts.Parts, 2)

	done := complete(second.server, upload, completeBody(etags), nil)
	require.Equal(t, http.StatusOK, done.status, string(done.body))

	got = second.get(multipartKey)
	require.Equal(t, http.StatusOK, got.status)
	assert.Equal(t, sha256.Sum256(slices.Concat(partOne, partTwo)), sha256.Sum256(got.body))

	second.flush()
	// 3 bodies, 2 versions and the completed upload; its parts went with it.
	assert.Equal(t, 6, second.blobFiles())
}

// TestPersistedS3CrashOrderings crashes between a change and the save that
// would record it: the state on disk comes back, with its bodies, and the body
// the change wrote is swept as an orphan.
func TestPersistedS3CrashOrderings(t *testing.T) {
	for name, change := range map[string]func(p *persisted){
		"overwrite": func(p *persisted) { p.put(savedKey, []byte("new")) },
		"delete": func(p *persisted) {
			require.Equal(t, http.StatusNoContent,
				p.do(call{method: http.MethodDelete, target: savedKey, region: regionUSEast1}).status)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			first := startPersisted(t, dir)

			createBucket(t, first.server)
			first.put(savedKey, []byte("saved"))
			first.flush()

			change(first)

			second := startPersisted(t, dir)

			got := second.get(savedKey)
			require.Equal(t, http.StatusOK, got.status, string(got.body))
			assert.Equal(t, "saved", string(got.body))
			assert.Equal(t, 1, second.blobFiles(), "only the saved body is left")
		})
	}

	t.Run("new object", func(t *testing.T) {
		dir := t.TempDir()
		first := startPersisted(t, dir)

		createBucket(t, first.server)
		first.flush()
		first.put(savedKey, []byte("unsaved"))
		require.Equal(t, 1, first.blobFiles())

		second := startPersisted(t, dir)

		assert.Equal(t, codeNoKey, second.get(savedKey).err(t).Code)
		assert.Zero(t, second.blobFiles(), "the load swept the orphan")
	})
}

// TestPersistedS3MissingBodyFailsOnlyItsObject removes one object's body from
// disk: reading it answers 500 InternalError, and the other object reads.
func TestPersistedS3MissingBodyFailsOnlyItsObject(t *testing.T) {
	p := startPersisted(t, t.TempDir())

	createBucket(t, p.server)
	p.put("/bucket/lost", []byte("lost"))

	var lost string

	err := filepath.WalkDir(p.blobsDir(), func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			lost = path
		}

		return err
	})
	require.NoError(t, err)
	require.NotEmpty(t, lost)

	p.put("/bucket/kept", []byte("kept"))
	require.NoError(t, os.Remove(lost))

	for range 2 {
		got := p.get("/bucket/lost")
		require.Equal(t, http.StatusInternalServerError, got.status, string(got.body))
		assert.Equal(t, "InternalError", got.err(t).Code)
	}

	got := p.get("/bucket/kept")
	require.Equal(t, http.StatusOK, got.status)
	assert.Equal(t, "kept", string(got.body))
}

// gatedBody closes reading at its first Read, and waits for loaded before it
// reads.
type gatedBody struct {
	io.ReadCloser

	reading, loaded chan struct{}
	once            sync.Once
}

func (b *gatedBody) Read(p []byte) (int, error) {
	b.once.Do(func() {
		close(b.reading)
		<-b.loaded
	})

	return b.ReadCloser.Read(p)
}

// TestPersistedS3LiveLoadDuringAPut loads the saved state while a PutObject
// streams its body: the put found the bucket the load replaced, so it fails
// and its body goes, and the restored bucket is as saved.
func TestPersistedS3LiveLoadDuringAPut(t *testing.T) {
	p := startPersisted(t, t.TempDir())

	createBucket(t, p.server)
	p.flush()

	// The handler reads the body once the put has found the bucket, in
	// writeBody: its first read waits there until the load is done.
	reading, loaded := make(chan struct{}), make(chan struct{})
	gated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = &gatedBody{ReadCloser: r.Body, reading: reading, loaded: loaded}
		p.handler.ServeHTTP(w, r)
	}))
	t.Cleanup(gated.Close)

	loadErr := make(chan error, 1)

	go func() {
		<-reading
		loadErr <- p.eng.Load(t.Context())
		close(loaded)
	}()

	got := (&server{t: t, url: gated.URL}).do(call{
		method: http.MethodPut, target: savedKey, region: regionUSEast1, body: []byte("ab"),
	})
	require.NoError(t, <-loadErr)
	require.Equal(t, http.StatusNotFound, got.status, string(got.body))
	assert.Equal(t, "NoSuchBucket", got.err(t).Code)

	assert.Equal(t, http.StatusOK, p.do(call{method: http.MethodHead, target: bucketPath, region: regionUSEast1}).status)
	assert.Equal(t, codeNoKey, p.get(savedKey).err(t).Code)
	assert.Zero(t, p.blobFiles(), "the put's body was removed")
}

// TestPersistedS3ScopedResetFreesBodies resets one region: the bodies of its
// buckets are deleted once the reset is saved, and the other region's stay.
func TestPersistedS3ScopedResetFreesBodies(t *testing.T) {
	p := startPersisted(t, t.TempDir())

	createBucket(t, p.server)
	p.put(savedKey, []byte("east"))

	created := p.do(call{
		method: http.MethodPut, target: "/west", region: regionEUWest1,
		body: []byte(`<CreateBucketConfiguration><LocationConstraint>eu-west-1</LocationConstraint>` +
			`</CreateBucketConfiguration>`),
	})
	require.Equal(t, http.StatusOK, created.status, string(created.body))

	for i := range 3 {
		p.put("/west/k"+strconv.Itoa(i), []byte("west"))
	}

	p.flush()
	require.Equal(t, 4, p.blobFiles())

	p.provider.Reset("", regionEUWest1)
	p.eng.MarkDirty(s3Name)
	p.flush()

	assert.Equal(t, 1, p.blobFiles(), "the reset region's bodies are gone")
	assert.Equal(t, "NoSuchBucket", p.get("/west/k0").err(t).Code)
	assert.Equal(t, "east", string(p.get(savedKey).body))
}

// dirtier records the services the gateway marks dirty.
type dirtier struct {
	mu    sync.Mutex
	marks int
}

func (d *dirtier) MarkDirty(string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.marks++
}

func (d *dirtier) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.marks
}

// TestS3WritesMarkItDirty checks that persistence hears of the operations that
// change S3, named by the generated router, and not of the ones that read it.
func TestS3WritesMarkItDirty(t *testing.T) {
	provider := s3.New(func() s3.Settings { return s3.Settings{LocalstackHost: localstackHost} },
		blob.NewEphemeral(t.TempDir()))
	t.Cleanup(provider.Close)

	marks := &dirtier{}

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{benchListen}} },
		[]service.Registration{provider.Registration()}, nil, marks,
	)
	require.NoError(t, err)

	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)

	s := &server{t: t, url: srv.URL}
	createBucket(t, s)

	upload := startUpload(t, s, multipartKey, nil)
	etag := putPart(t, s, upload, 1, []byte("part"), nil)

	for _, step := range []struct {
		name    string
		send    func() reply
		changes bool
	}{
		{"PutObject", func() reply {
			return s.do(call{method: http.MethodPut, target: savedKey, region: regionUSEast1, body: []byte("x")})
		}, true},
		{"GetObject", func() reply {
			return s.do(call{method: http.MethodGet, target: savedKey, region: regionUSEast1})
		}, false},
		{"ListObjectsV2", func() reply {
			return s.do(call{method: http.MethodGet, target: "/bucket?list-type=2", region: regionUSEast1})
		}, false},
		{"CompleteMultipartUpload", func() reply {
			return complete(s, upload, completeBody([]string{etag}), nil)
		}, true},
		{"DeleteObject", func() reply {
			return s.do(call{method: http.MethodDelete, target: savedKey, region: regionUSEast1})
		}, true},
	} {
		before := marks.count()
		got := step.send()
		require.Less(t, got.status, http.StatusMultipleChoices, "%s: %s", step.name, got.body)

		if step.changes {
			assert.Equal(t, before+1, marks.count(), "%s marks S3 dirty", step.name)
		} else {
			assert.Equal(t, before, marks.count(), "%s does not mark S3 dirty", step.name)
		}
	}
}
