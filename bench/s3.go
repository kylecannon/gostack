package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file holds the S3 workloads. Each speaks S3 over plain HTTP, path style,
// with the headers the services route on and no signature check, which neither
// gostack nor LocalStack makes. The SDK's s3 module is not a dependency of the
// repository, and a hand-built request lets the bench stream a body of any size
// from one shared buffer.
//
//	s3/objects    PutObject, then GetObject of the same key, at four sizes. Each
//	              worker overwrites a key of its own, so the stored bytes stay at
//	              one object a worker.
//	s3/listing    ListObjectsV2 over a bucket seeded with 1000 or 10000 keys, flat
//	              or with delimiter "/". A cycle pages through the whole listing,
//	              1000 keys a page, and every page is one timed operation.
//	s3/multipart  CreateMultipartUpload, eight UploadPart of 8 MiB, and
//	              CompleteMultipartUpload: a 64 MiB object.
//
// The agentic cycle is create bucket, put 10 objects, list them, delete them,
// delete the bucket.

const (
	serviceS3         = "s3"
	workloadObjects   = "objects"
	workloadListing   = "listing"
	workloadMultipart = "multipart"
	persistEnvS3      = "PERSIST_S3"

	opPutObject = "put"
	opGetObject = "get"
	opList      = "list"
	opCreate    = "create"
	opPart      = "part"
	opComplete  = "complete"

	optKeys       = "keys"
	optMode       = "mode"
	modeFlat      = "flat"
	modeDelimiter = "delimiter"

	s3Payload1KiB  = 1 << 10
	s3Payload64KiB = 64 << 10
	s3Payload1MiB  = 1 << 20
	s3Payload16MiB = 16 << 20
	s3Payload64MiB = 64 << 20

	// partBytes is the size of one part of the multipart cells. S3's smallest
	// part but the last is 5 MiB.
	partBytes = 8 << 20
	// levelMultipart is the top concurrency of the multipart cells: a 64 MiB
	// upload on 64 workers would hold 4 GiB in flight.
	levelMultipart = 4

	listPageKeys  = 1000
	listDirs      = 100
	listSmall     = 1000
	listLarge     = 10000
	listSeedBytes = 16
	seedWorkers   = 32

	agenticObjects   = 10
	agenticBodyBytes = 1 << 10

	// s3Auth is a SigV4 header that names the s3 signing name and a region. The
	// signature is not checked.
	s3Auth = "AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=abc"
	s3UnsignedPayload = "UNSIGNED-PAYLOAD"
	s3Date            = "20260101T000000Z"
	statusClassOK     = 2
	statusClassScale  = 100
)

// s3Persistence are the persistence settings of the S3 cells, "" being off.
var s3Persistence = []string{"", persistEnvS3}

var (
	errS3      = errors.New("s3")
	errNoCycle = errors.New("the run ended before a cycle began")
)

// s3Client sends S3 requests to one endpoint.
type s3Client struct {
	http     *http.Client
	endpoint string
}

func newS3Client(endpoint string, workers int) s3Client {
	transport := newTransport(workers)
	transport.DisableCompression = true

	return s3Client{http: &http.Client{Transport: transport}, endpoint: endpoint}
}

// send sends one request. The caller closes the response body. A nil body is an
// empty one.
func (c s3Client) send(ctx context.Context, method, target string, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+target, reader)
	if err != nil {
		return nil, fmt.Errorf("build %s %s: %w", method, target, err)
	}

	req.Header.Set("Authorization", s3Auth)
	req.Header.Set("X-Amz-Content-Sha256", s3UnsignedPayload)
	req.Header.Set("X-Amz-Date", s3Date)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, target, err)
	}

	return resp, nil
}

// checked fails on a status outside 2xx, with the start of the answer.
func checked(method, target string, resp *http.Response) error {
	if resp.StatusCode/statusClassScale == statusClassOK {
		return nil
	}

	head, _ := io.ReadAll(io.LimitReader(resp.Body, formErrorHead))

	return fmt.Errorf("%w: %s %s: status %d: %s", errS3, method, target, resp.StatusCode, head)
}

// exchange sends a request and returns the whole answer.
func (c s3Client) exchange(ctx context.Context, method, target string, body []byte) ([]byte, http.Header, error) {
	resp, err := c.send(ctx, method, target, body)
	if err != nil {
		return nil, nil, err
	}

	defer func() { _ = resp.Body.Close() }()

	err = checked(method, target, resp)
	if err != nil {
		return nil, nil, err
	}

	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: read the answer: %w", method, target, err)
	}

	return answer, resp.Header, nil
}

func (c s3Client) call(ctx context.Context, method, target string, body []byte) error {
	_, _, err := c.exchange(ctx, method, target, body)

	return err
}

// get reads an object to the end without keeping it, and checks its size.
func (c s3Client) get(ctx context.Context, target string, size int) error {
	resp, err := c.send(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()

	err = checked(http.MethodGet, target, resp)
	if err != nil {
		return err
	}

	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		return fmt.Errorf("GET %s: read the object: %w", target, err)
	}

	if n != int64(size) {
		return fmt.Errorf("%w: GET %s: %d bytes, want %d", errS3, target, n, size)
	}

	return nil
}

// newBucket creates a bucket with a name no other run uses and returns its path.
func (c s3Client) newBucket(ctx context.Context, prefix string) (string, error) {
	bucket := "/" + fmt.Sprintf("%s-%x", prefix, time.Now().UnixNano())

	err := c.call(ctx, http.MethodPut, bucket, nil)
	if err != nil {
		return "", fmt.Errorf("create bucket: %w", err)
	}

	return bucket, nil
}

// between returns the text between the first open and the close after it, or "".
func between(body []byte, open, closing string) string {
	_, rest, found := bytes.Cut(body, []byte(open))
	if !found {
		return ""
	}

	text, _, found := bytes.Cut(rest, []byte(closing))
	if !found {
		return ""
	}

	return string(text)
}

// recorder collects what one worker timed.
type recorder struct {
	timings  map[string][]time.Duration
	failed   map[string]int
	firstErr error
	cycles   int
}

func newRecorder() recorder {
	return recorder{timings: map[string][]time.Duration{}, failed: map[string]int{}}
}

// measure runs one call and records its duration under op, or counts a failure.
func (r *recorder) measure(op string, call func() error) bool {
	began := time.Now()
	err := call()
	took := time.Since(began)

	if err != nil {
		r.failed[op]++

		if r.firstErr == nil {
			r.firstErr = err
		}

		return false
	}

	r.timings[op] = append(r.timings[op], took)

	return true
}

func (r *recorder) failures() int {
	total := 0
	for _, n := range r.failed {
		total += n
	}

	return total
}

// loop is how a workload runs a cell: the workers, the time, and the
// operations it times.
type loop struct {
	workers  int
	duration time.Duration
	ops      []string
}

// run runs cycle on every worker until the deadline. A cycle in flight at the
// deadline finishes and counts.
func (l loop) run(ctx context.Context, cycle func(ctx context.Context, worker int, rec *recorder)) (runResult, error) {
	recs := make([]recorder, l.workers)
	started := time.Now()
	end := started.Add(l.duration)

	var wg sync.WaitGroup

	for i := range l.workers {
		recs[i] = newRecorder()
		rec := &recs[i]

		wg.Go(func() {
			for time.Now().Before(end) && ctx.Err() == nil {
				if rec.failures() > maxWorkerErrors {
					return
				}

				cycle(ctx, i, rec)
			}
		})
	}

	wg.Wait()

	return l.fold(recs, time.Since(started))
}

// fold merges the recorders of the workers into one run.
func (l loop) fold(recs []recorder, elapsed time.Duration) (runResult, error) {
	timings := map[string][]time.Duration{}
	failed := map[string]int{}
	run := runResult{Seconds: elapsed.Seconds(), Ops: map[string]opStats{}}

	var first error

	for i := range recs {
		run.Cycles += recs[i].cycles

		if first == nil {
			first = recs[i].firstErr
		}

		for op, t := range recs[i].timings {
			timings[op] = append(timings[op], t...)
		}

		for op, n := range recs[i].failed {
			failed[op] += n
		}
	}

	for _, op := range l.ops {
		run.Ops[op] = statsOf(timings[op], failed[op])
	}

	run.CyclesPerSec = float64(run.Cycles) / elapsed.Seconds()

	if run.Cycles == 0 {
		if first == nil {
			first = errNoCycle
		}

		return run, fmt.Errorf("no cycle completed, failures %v: %w", failed, first)
	}

	return run, nil
}

// checkLevels refuses a cell no cycle can run.
func checkLevels(cell cellSpec) error {
	if cell.Concurrency < 1 {
		return fmt.Errorf("%w: concurrency %d must be at least 1", errBadAxis, cell.Concurrency)
	}

	return nil
}

// objectsWorkload is PutObject and GetObject of one key a worker, and the
// agentic bucket cycle.
type objectsWorkload struct{}

func (objectsWorkload) Service() string { return serviceS3 }
func (objectsWorkload) Name() string    { return workloadObjects }
func (objectsWorkload) Ops() []string   { return []string{opPutObject, opGetObject} }

func (objectsWorkload) Run(ctx context.Context, c client, cell cellSpec, duration time.Duration) (runResult, error) {
	err := checkLevels(cell)
	if err != nil {
		return runResult{}, err
	}

	if cell.Payload < 1 {
		return runResult{}, fmt.Errorf("%w: payload %d must be at least 1", errBadAxis, cell.Payload)
	}

	s3c := newS3Client(c.endpoint, cell.Concurrency)

	bucket, err := s3c.newBucket(ctx, "bench-cell")
	if err != nil {
		return runResult{}, err
	}

	body := []byte(payloadBody(cell.Payload))
	l := loop{workers: cell.Concurrency, duration: duration, ops: objectsWorkload{}.Ops()}

	return l.run(ctx, func(ctx context.Context, worker int, rec *recorder) {
		target := bucket + "/obj-" + strconv.Itoa(worker)

		if !rec.measure(opPutObject, func() error { return s3c.call(ctx, http.MethodPut, target, body) }) {
			return
		}

		if rec.measure(opGetObject, func() error { return s3c.get(ctx, target, len(body)) }) {
			rec.cycles++
		}
	})
}

func objectsCell(payload int, persistEnv string, concurrency int) cellSpec {
	return cellSpec{
		Service: serviceS3, Workload: workloadObjects, Payload: payload,
		PersistEnv: persistEnv, Concurrency: concurrency,
	}
}

// Cells is every payload at every concurrency, persistence off and on. The quick
// profile is the base cell, a size above and below it, and its two other
// concurrencies, and the base cell with persistence on.
func (objectsWorkload) Cells(profile string) []cellSpec {
	if profile == profileQuick {
		return []cellSpec{
			objectsCell(s3Payload64KiB, "", levelLow),
			objectsCell(s3Payload64KiB, "", levelMid),
			objectsCell(s3Payload64KiB, "", levelHigh),
			objectsCell(s3Payload1KiB, "", levelMid),
			objectsCell(s3Payload16MiB, "", levelMid),
			objectsCell(s3Payload64KiB, persistEnvS3, levelMid),
		}
	}

	var cells []cellSpec

	for _, persistEnv := range s3Persistence {
		for _, payload := range []int{s3Payload1KiB, s3Payload64KiB, s3Payload1MiB, s3Payload16MiB} {
			for _, level := range []int{levelLow, levelMid, levelHigh} {
				cells = append(cells, objectsCell(payload, persistEnv, level))
			}
		}
	}

	return cells
}

func (objectsWorkload) Agentic(ctx context.Context, c client, cycles, workers int) (agenticRun, error) {
	s3c := newS3Client(c.endpoint, workers)
	prefix := fmt.Sprintf("bench-agentic-%x", time.Now().UnixNano())

	return runAgenticCycles(ctx, cycles, workers, func(ctx context.Context, n int) error {
		return agenticBucketCycle(ctx, s3c, "/"+prefix+"-"+strconv.Itoa(n))
	})
}

// agenticBucketCycle is create bucket, put 10 objects, list them, delete them,
// delete the bucket.
func agenticBucketCycle(ctx context.Context, c s3Client, bucket string) error {
	err := c.call(ctx, http.MethodPut, bucket, nil)
	if err != nil {
		return fmt.Errorf("create bucket: %w", err)
	}

	body := []byte(payloadBody(agenticBodyBytes))

	for i := range agenticObjects {
		err = c.call(ctx, http.MethodPut, bucket+"/object-"+strconv.Itoa(i), body)
		if err != nil {
			return fmt.Errorf("put object: %w", err)
		}
	}

	page, err := c.listPage(ctx, bucket, modeFlat, "")
	if err != nil {
		return err
	}

	if page.keys != agenticObjects {
		return fmt.Errorf("%w: listed %d objects, want %d", errS3, page.keys, agenticObjects)
	}

	for i := range agenticObjects {
		err = c.call(ctx, http.MethodDelete, bucket+"/object-"+strconv.Itoa(i), nil)
		if err != nil {
			return fmt.Errorf("delete object: %w", err)
		}
	}

	err = c.call(ctx, http.MethodDelete, bucket, nil)
	if err != nil {
		return fmt.Errorf("delete bucket: %w", err)
	}

	return nil
}

// listing is ListObjectsV2 over a seeded bucket.
type listingWorkload struct{}

func (listingWorkload) Service() string { return serviceS3 }
func (listingWorkload) Name() string    { return workloadListing }
func (listingWorkload) Ops() []string   { return []string{opList} }

// page is what one ListObjectsV2 answer says: how many keys and common prefixes
// it holds, and where the next page starts.
type page struct {
	keys, prefixes int
	next           string
}

// listPage lists one page of the bucket: flat, or grouped by "/".
func (c s3Client) listPage(ctx context.Context, bucket, mode, token string) (page, error) {
	target := bucket + "?list-type=2&max-keys=" + strconv.Itoa(listPageKeys)
	if mode == modeDelimiter {
		target += "&delimiter=%2F"
	}

	if token != "" {
		target += "&continuation-token=" + url.QueryEscape(token)
	}

	body, _, err := c.exchange(ctx, http.MethodGet, target, nil)
	if err != nil {
		return page{}, err
	}

	p := page{keys: bytes.Count(body, []byte("<Key>")), prefixes: bytes.Count(body, []byte("<CommonPrefixes>"))}

	if bytes.Contains(body, []byte("<IsTruncated>true</IsTruncated>")) {
		p.next = html.UnescapeString(between(body, "<NextContinuationToken>", "</NextContinuationToken>"))
		if p.next == "" {
			return page{}, fmt.Errorf("%w: a truncated listing with no continuation token", errS3)
		}
	}

	return p, nil
}

// listKey names the i-th seeded key: listDirs directories of equal size.
func listKey(i int) string { return fmt.Sprintf("d%03d/k%05d", i%listDirs, i) }

// seed puts keys, small objects, on seedWorkers workers. The first failure stops
// the rest.
func (c s3Client) seed(ctx context.Context, bucket string, keys int) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	jobs := make(chan int)
	body := []byte(payloadBody(listSeedBytes))

	var wg sync.WaitGroup

	for range seedWorkers {
		wg.Go(func() {
			for i := range jobs {
				err := c.call(ctx, http.MethodPut, bucket+"/"+listKey(i), body)
				if err != nil {
					cancel(fmt.Errorf("seed %s: %w", listKey(i), err))

					return
				}
			}
		})
	}

feed:
	for i := range keys {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}

	close(jobs)
	wg.Wait()

	return context.Cause(ctx) //nolint:wrapcheck // the cause is a seed failure, wrapped where it was raised.
}

func (listingWorkload) Run(ctx context.Context, c client, cell cellSpec, duration time.Duration) (runResult, error) {
	err := checkLevels(cell)
	if err != nil {
		return runResult{}, err
	}

	keys, err := strconv.Atoi(cell.option(optKeys))
	mode := cell.option(optMode)

	if err != nil || keys < listDirs || !slices.Contains([]string{modeFlat, modeDelimiter}, mode) {
		return runResult{}, fmt.Errorf("%w: %s=%q (want at least %d) %s=%q (want %s or %s)", errBadAxis,
			optKeys, cell.option(optKeys), listDirs, optMode, mode, modeFlat, modeDelimiter)
	}

	s3c := newS3Client(c.endpoint, max(cell.Concurrency, seedWorkers))

	bucket, err := s3c.newBucket(ctx, "bench-list")
	if err != nil {
		return runResult{}, err
	}

	err = s3c.seed(ctx, bucket, keys)
	if err != nil {
		return runResult{}, err
	}

	want := keys
	if mode == modeDelimiter {
		want = listDirs
	}

	l := loop{workers: cell.Concurrency, duration: duration, ops: listingWorkload{}.Ops()}

	return l.run(ctx, func(ctx context.Context, _ int, rec *recorder) {
		if listAll(ctx, s3c, bucket, mode, want, rec) {
			rec.cycles++
		}
	})
}

// listAll pages through the bucket and checks what it found against want, the
// keys or the common prefixes the cell's mode should return.
func listAll(ctx context.Context, c s3Client, bucket, mode string, want int, rec *recorder) bool {
	found, token := 0, ""

	for {
		var p page

		ok := rec.measure(opList, func() error {
			var err error

			p, err = c.listPage(ctx, bucket, mode, token)

			return err
		})
		if !ok {
			return false
		}

		found += p.keys + p.prefixes
		if mode == modeDelimiter {
			found -= p.keys
		}

		if p.next == "" {
			break
		}

		token = p.next
	}

	if found != want {
		rec.failed[opList]++
		rec.firstErr = fmt.Errorf("%w: listed %d, want %d", errS3, found, want)

		return false
	}

	return true
}

func listingCell(keys int, mode, persistEnv string, concurrency int) cellSpec {
	return cellSpec{
		Service: serviceS3, Workload: workloadListing,
		Options:    []option{{optKeys, strconv.Itoa(keys)}, {optMode, mode}},
		PersistEnv: persistEnv, Concurrency: concurrency,
	}
}

// Cells is 1000 and 10000 keys, flat and with a delimiter, at every concurrency,
// persistence off and on. The quick profile is two of them at concurrency 16,
// and the first with persistence on.
func (listingWorkload) Cells(profile string) []cellSpec {
	if profile == profileQuick {
		return []cellSpec{
			listingCell(listSmall, modeFlat, "", levelMid),
			listingCell(listLarge, modeDelimiter, "", levelMid),
			listingCell(listSmall, modeFlat, persistEnvS3, levelMid),
		}
	}

	var cells []cellSpec

	for _, persistEnv := range s3Persistence {
		for _, keys := range []int{listSmall, listLarge} {
			for _, mode := range []string{modeFlat, modeDelimiter} {
				for _, level := range []int{levelLow, levelMid, levelHigh} {
					cells = append(cells, listingCell(keys, mode, persistEnv, level))
				}
			}
		}
	}

	return cells
}

// multipartWorkload is a 64 MiB object uploaded in parts.
type multipartWorkload struct{}

func (multipartWorkload) Service() string { return serviceS3 }
func (multipartWorkload) Name() string    { return workloadMultipart }
func (multipartWorkload) Ops() []string   { return []string{opCreate, opPart, opComplete} }

func (multipartWorkload) Run(ctx context.Context, c client, cell cellSpec, duration time.Duration) (runResult, error) {
	err := checkLevels(cell)
	if err != nil {
		return runResult{}, err
	}

	if cell.Payload < partBytes || cell.Payload%partBytes != 0 {
		return runResult{}, fmt.Errorf("%w: payload %d must be a multiple of %d", errBadAxis, cell.Payload, partBytes)
	}

	s3c := newS3Client(c.endpoint, cell.Concurrency)

	bucket, err := s3c.newBucket(ctx, "bench-mpu")
	if err != nil {
		return runResult{}, err
	}

	part := []byte(payloadBody(partBytes))
	parts := cell.Payload / partBytes
	l := loop{workers: cell.Concurrency, duration: duration, ops: multipartWorkload{}.Ops()}

	return l.run(ctx, func(ctx context.Context, worker int, rec *recorder) {
		if s3c.upload(ctx, bucket+"/object-"+strconv.Itoa(worker), part, parts, rec) {
			rec.cycles++
		}
	})
}

// upload is one multipart upload of parts copies of part, each step timed.
func (c s3Client) upload(ctx context.Context, target string, part []byte, parts int, rec *recorder) bool {
	var id string

	created := rec.measure(opCreate, func() error {
		body, _, err := c.exchange(ctx, http.MethodPost, target+"?uploads", nil)
		id = between(body, "<UploadId>", "</UploadId>")

		if err == nil && id == "" {
			err = fmt.Errorf("%w: no UploadId in %.*s", errS3, formErrorHead, body)
		}

		return err
	})
	if !created {
		return false
	}

	var done strings.Builder

	done.WriteString("<CompleteMultipartUpload>")

	for n := 1; n <= parts; n++ {
		var etag string

		ok := rec.measure(opPart, func() error {
			_, header, err := c.exchange(ctx, http.MethodPut,
				target+"?partNumber="+strconv.Itoa(n)+"&uploadId="+url.QueryEscape(id), part)
			etag = header.Get("ETag")

			return err
		})
		if !ok {
			return false
		}

		fmt.Fprintf(&done, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", n, html.EscapeString(etag))
	}

	done.WriteString("</CompleteMultipartUpload>")

	return rec.measure(opComplete, func() error {
		answer, _, err := c.exchange(ctx, http.MethodPost, target+"?uploadId="+url.QueryEscape(id), []byte(done.String()))
		if err == nil && bytes.Contains(answer, []byte("<Error>")) {
			err = fmt.Errorf("%w: complete: %.*s", errS3, formErrorHead, answer)
		}

		return err
	})
}

func multipartCell(persistEnv string, concurrency int) cellSpec {
	return cellSpec{
		Service: serviceS3, Workload: workloadMultipart, Payload: s3Payload64MiB,
		PersistEnv: persistEnv, Concurrency: concurrency,
	}
}

// Cells is one 64 MiB object at concurrency 1 and 4, persistence off and on. The
// quick profile is the first at each persistence setting.
func (multipartWorkload) Cells(profile string) []cellSpec {
	if profile == profileQuick {
		return []cellSpec{multipartCell("", levelLow), multipartCell(persistEnvS3, levelLow)}
	}

	var cells []cellSpec

	for _, persistEnv := range s3Persistence {
		for _, level := range []int{levelLow, levelMultipart} {
			cells = append(cells, multipartCell(persistEnv, level))
		}
	}

	return cells
}
