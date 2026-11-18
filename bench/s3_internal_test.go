package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeS3 answers the calls the S3 workloads make, in memory: enough of the wire
// to check that the workloads send what S3 takes and read what it answers.
type fakeS3 struct {
	mu      sync.Mutex
	buckets map[string]map[string]int // bucket -> key -> size
	parts   map[string][]int          // upload id -> part sizes
	signed  int                       // requests with the s3 signing name
	total   int
}

func newFakeS3(t *testing.T) (*fakeS3, client) {
	t.Helper()

	f := &fakeS3{buckets: map[string]map[string]int{}, parts: map[string][]int{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	return f, client{endpoint: srv.URL}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.total++

	if strings.Contains(r.Header.Get("Authorization"), "/s3/aws4_request") {
		f.signed++
	}

	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	query := r.URL.Query()

	switch {
	case key == "" && r.Method == http.MethodPut:
		f.buckets[bucket] = map[string]int{}
	case key == "" && r.Method == http.MethodDelete:
		delete(f.buckets, bucket)
		w.WriteHeader(http.StatusNoContent)
	case key == "":
		f.list(w, bucket, query)
	default:
		f.object(w, r, bucket, key)
	}
}

// list answers ListObjectsV2: one page of the sorted keys, or with a delimiter
// the common prefixes of all of them.
func (f *fakeS3) list(w http.ResponseWriter, bucket string, query url.Values) {
	keys := make([]string, 0, len(f.buckets[bucket]))
	for k := range f.buckets[bucket] {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	var out strings.Builder

	out.WriteString("<ListBucketResult><Prefix></Prefix>")

	if query.Get("delimiter") != "" {
		seen := map[string]bool{}

		for _, k := range keys {
			dir, _, _ := strings.Cut(k, "/")
			if !seen[dir] {
				seen[dir] = true

				out.WriteString("<CommonPrefixes><Prefix>" + dir + "/</Prefix></CommonPrefixes>")
			}
		}

		keys = nil
	}

	start, _ := strconv.Atoi(strings.TrimPrefix(query.Get("continuation-token"), "t"))
	end := min(start+listPageKeys, len(keys))

	out.WriteString("<IsTruncated>" + strconv.FormatBool(end < len(keys)) + "</IsTruncated>")

	if end < len(keys) {
		out.WriteString("<NextContinuationToken>t" + strconv.Itoa(end) + "</NextContinuationToken>")
	}

	for _, k := range keys[start:end] {
		out.WriteString("<Contents><Key>" + k + "</Key></Contents>")
	}

	_, _ = io.WriteString(w, out.String()+"</ListBucketResult>")
}

func (f *fakeS3) object(w http.ResponseWriter, r *http.Request, bucket, key string) {
	objects, ok := f.buckets[bucket]
	if !ok {
		http.Error(w, "<Error><Code>NoSuchBucket</Code></Error>", http.StatusNotFound)

		return
	}

	query := r.URL.Query()

	switch {
	case r.Method == http.MethodPost && query.Has("uploads"):
		id := fmt.Sprintf("upload-%d", len(f.parts))
		f.parts[id] = nil

		_, _ = io.WriteString(w, "<InitiateMultipartUploadResult><UploadId>"+id+"</UploadId></InitiateMultipartUploadResult>")
	case r.Method == http.MethodPut && query.Has("uploadId"):
		n, _ := io.Copy(io.Discard, r.Body)
		id := query.Get("uploadId")
		f.parts[id] = append(f.parts[id], int(n))
		w.Header().Set("ETag", `"etag-`+query.Get("partNumber")+`"`)
	case r.Method == http.MethodPost && query.Has("uploadId"):
		body, _ := io.ReadAll(r.Body)
		id := query.Get("uploadId")

		listed := strings.Count(string(body), "<Part>")
		if listed != len(f.parts[id]) || !strings.Contains(string(body), `<ETag>&#34;etag-1&#34;</ETag>`) {
			_, _ = io.WriteString(w, "<Error><Code>InvalidPart</Code></Error>")

			return
		}

		objects[key] = 0

		for _, n := range f.parts[id] {
			objects[key] += n
		}

		_, _ = io.WriteString(w, "<CompleteMultipartUploadResult></CompleteMultipartUploadResult>")
	case r.Method == http.MethodPut:
		n, _ := io.Copy(io.Discard, r.Body)
		objects[key] = int(n)
	case r.Method == http.MethodGet:
		size, found := objects[key]
		if !found {
			http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", http.StatusNotFound)

			return
		}

		_, _ = io.WriteString(w, payloadBody(size))
	case r.Method == http.MethodDelete:
		delete(objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "<Error><Code>MethodNotAllowed</Code></Error>", http.StatusMethodNotAllowed)
	}
}

const testRun = 80 * time.Millisecond

func Test_objects_cell_puts_and_gets_the_payload_it_puts(t *testing.T) {
	fake, c := newFakeS3(t)
	cell := objectsCell(s3Payload64KiB, "", 3)

	run, err := objectsWorkload{}.Run(t.Context(), c, cell, testRun)

	require.NoError(t, err)
	assert.Positive(t, run.Cycles)
	assert.Equal(t, run.Cycles, run.Ops[opPutObject].Count)
	assert.Equal(t, run.Cycles, run.Ops[opGetObject].Count, "a cycle is one put and one get")
	assert.Zero(t, run.Ops[opPutObject].Errors+run.Ops[opGetObject].Errors)
	assert.Equal(t, fake.total, fake.signed, "every request names the s3 signing name")

	for _, objects := range fake.buckets {
		assert.Len(t, objects, 3, "a key a worker, overwritten each cycle")

		for _, size := range objects {
			assert.Equal(t, s3Payload64KiB, size)
		}
	}
}

func Test_a_get_that_comes_back_short_is_a_failure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, "short")
		}
	}))
	t.Cleanup(srv.Close)

	_, err := objectsWorkload{}.Run(t.Context(), client{endpoint: srv.URL}, objectsCell(s3Payload1KiB, "", 1), testRun)

	require.ErrorIs(t, err, errS3)
	assert.Contains(t, err.Error(), "5 bytes, want 1024")
}

func Test_listing_cell_pages_through_every_key(t *testing.T) {
	_, c := newFakeS3(t)
	spec := listingCell(2500, modeFlat, "", 2)

	run, err := listingWorkload{}.Run(t.Context(), c, spec, testRun)

	require.NoError(t, err)
	assert.Positive(t, run.Cycles)
	assert.Equal(t, 3*run.Cycles, run.Ops[opList].Count, "2500 keys is three pages of 1000")
	assert.Zero(t, run.Ops[opList].Errors)

	run, err = listingWorkload{}.Run(t.Context(), c, listingCell(2500, modeDelimiter, "", 2), testRun)

	require.NoError(t, err)
	assert.Equal(t, run.Cycles, run.Ops[opList].Count, "the 100 prefixes fit one page")
	assert.Zero(t, run.Ops[opList].Errors, "100 common prefixes were found")
}

func Test_listing_cell_refuses_an_axis_it_has_no_cycle_for(t *testing.T) {
	_, c := newFakeS3(t)

	for _, spec := range []cellSpec{
		listingCell(10, modeFlat, "", 1),
		listingCell(1000, "reverse", "", 1),
		{Service: serviceS3, Workload: workloadListing, Concurrency: 1},
	} {
		_, err := listingWorkload{}.Run(t.Context(), c, spec, testRun)
		require.ErrorIs(t, err, errBadAxis, spec.id())
	}
}

func Test_multipart_cell_uploads_parts_and_completes(t *testing.T) {
	fake, c := newFakeS3(t)
	cell := multipartCell("", 2)
	cell.Payload = 2 * partBytes

	run, err := multipartWorkload{}.Run(t.Context(), c, cell, testRun)

	require.NoError(t, err)
	assert.Positive(t, run.Cycles)
	assert.Equal(t, run.Cycles, run.Ops[opCreate].Count)
	assert.Equal(t, 2*run.Cycles, run.Ops[opPart].Count)
	assert.Equal(t, run.Cycles, run.Ops[opComplete].Count)

	for _, objects := range fake.buckets {
		for _, size := range objects {
			assert.Equal(t, 2*partBytes, size)
		}
	}
}

func Test_agentic_cycle_leaves_no_bucket_behind(t *testing.T) {
	fake, c := newFakeS3(t)

	run, err := objectsWorkload{}.Agentic(t.Context(), c, 6, 3)

	require.NoError(t, err)
	assert.Zero(t, run.Errors)
	assert.Equal(t, 6, run.Cycles)
	assert.Empty(t, fake.buckets, "each cycle deletes its objects and its bucket")
}

func Test_s3_cells(t *testing.T) {
	full := cellsFor(profileFull, target{persistable: true}, serviceS3)
	// 4 payloads x 3 concurrencies, 2 sizes x 2 modes x 3 concurrencies, and a
	// 64 MiB object at 2 concurrencies; each with persistence off and on.
	assert.Len(t, full, 2*(12+12+2))
	assert.Len(t, cellsFor(profileFull, target{}, serviceS3), 12+12+2, "a target that does not persist")

	ids := map[string]bool{}

	for _, c := range full {
		assert.False(t, ids[c.id()], "%s is listed twice", c.id())

		ids[c.id()] = true

		assert.Equal(t, c.persists(), c.PersistEnv == "PERSIST_S3", c.id())
		assert.Contains(t, c.id(), "persist="+map[bool]string{true: "on", false: "off"}[c.persists()])
	}

	for _, c := range cellsFor(profileQuick, target{persistable: true}, serviceS3) {
		assert.True(t, ids[c.id()], "%s is not a cell of the full profile", c.id())
	}

	assert.Equal(t, "s3/objects/persist=off/payload=16MiB/c=64", objectsCell(s3Payload16MiB, "", levelHigh).id())
	assert.Equal(t, "s3/listing/persist=on/keys=10000/mode=delimiter/c=16",
		listingCell(listLarge, modeDelimiter, persistEnvS3, levelMid).id(), "a listing has no payload axis")
	assert.Equal(t, "s3/multipart/persist=off/payload=64MiB/c=4", multipartCell("", levelMultipart).id())
}

func Test_every_cell_of_every_workload_names_a_registered_workload(t *testing.T) {
	for _, profile := range []string{profileFull, profileQuick} {
		for _, c := range cellsFor(profile, target{persistable: true}) {
			_, err := findWorkload(c.Service, c.Workload)
			require.NoError(t, err, c.id())
		}
	}
}

func Test_parseServices_names_the_services_a_run_covers(t *testing.T) {
	opts, err := parseOptions([]string{"-services", "s3"})
	require.NoError(t, err)
	assert.Equal(t, []string{serviceS3}, opts.services)

	for _, c := range cellsFor(profileQuick, target{persistable: true}, opts.services...) {
		assert.Equal(t, serviceS3, c.Service)
	}

	_, err = parseOptions([]string{"-services", "s3,nope"})
	require.ErrorIs(t, err, errNoWorkload)
}
