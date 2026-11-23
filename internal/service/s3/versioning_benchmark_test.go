package s3_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// The versioning benchmark matrix (docs/standards.md, "Benchmarks"): the object
// calls that change with versioning and Object Lock, through the real gateway
// over HTTP at concurrency 1 and 16. Sub-benchmark names carry the axes:
//
//	BenchmarkPutObjectVersioned/versioning=enabled/payload=1KiB/concurrency=16
//
// A bucket with versioning Enabled keeps every version a PUT makes, so its 256 KiB
// payload row is left out: the iterations would pile up bodies without end. The
// unversioned PUT and GET are in BenchmarkPutObject and BenchmarkGetObject.

const (
	// chainLength is how many versions the key of BenchmarkGetObjectByVersion
	// has, so that the oldest version is found past the others.
	chainLength = 100

	lockDays = "1"
)

// versionedBenchCells are the versioning statuses PutObject is measured under,
// with the payloads of each.
var versionedBenchCells = []struct {
	name, status string
	payloads     []benchPayload
}{
	{lowerEnabled, "Enabled", benchPayloads[:1]},
	{"suspended", "Suspended", benchPayloads[:2]},
}

// versionedBenchServer is an objectBenchServer whose bucket has the versioning
// status.
func versionedBenchServer(b *testing.B, status string) *objectBenchServer {
	b.Helper()

	return enableVersioning(b, newObjectBenchServer(b), status)
}

// enableVersioning gives the bucket of s the versioning status.
func enableVersioning(b *testing.B, s *objectBenchServer, status string) *objectBenchServer {
	b.Helper()

	body := []byte(`<VersioningConfiguration><Status>` + status + `</Status></VersioningConfiguration>`)

	got, _, err := s.send(b.Context(), http.MethodPut, "/bucket?versioning", body, nil)
	require.NoError(b, err)
	require.Equal(b, http.StatusOK, got)

	return s
}

// lockedBenchServer is an objectBenchServer whose bucket was created with
// Object Lock.
func lockedBenchServer(b *testing.B) *objectBenchServer {
	b.Helper()

	s := newObjectBenchServer(b)

	got, _, err := s.send(b.Context(), http.MethodDelete, "/bucket", nil, nil)
	require.NoError(b, err)
	require.Equal(b, http.StatusNoContent, got)

	got, _, err = s.send(b.Context(), http.MethodPut, "/bucket", nil, map[string]string{headerLockEnabled: valueTrue})
	require.NoError(b, err)
	require.Equal(b, http.StatusOK, got)

	return s
}

// send sends one request with the headers and reads the whole response,
// returning its status and the version id it names.
func (s *objectBenchServer) send(
	ctx context.Context, method, target string, body []byte, header map[string]string,
) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.url+target, bytes.NewReader(body))
	if err != nil {
		return 0, "", fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Authorization", benchAuth)

	for name, value := range header {
		req.Header.Set(name, value)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("send request: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	_, err = io.Copy(io.Discard, resp.Body)
	if err != nil {
		return resp.StatusCode, "", fmt.Errorf("read response: %w", err)
	}

	return resp.StatusCode, resp.Header.Get(headerVersionID), nil
}

// expectStatus turns an unexpected status into an error.
func expectStatus(op string, got, want int, err error) error {
	if err == nil && got != want {
		return fmt.Errorf("%s answered %d", op, got)
	}

	return err
}

func BenchmarkPutObjectVersioned(b *testing.B) {
	for _, cell := range versionedBenchCells {
		for _, payload := range cell.payloads {
			for _, workers := range benchConcurrency {
				if tooSlowToGate(payload, workers, writeBudget) {
					continue
				}

				name := fmt.Sprintf("versioning=%s/payload=%s/concurrency=%d", cell.name, payload.name, workers)

				b.Run(name, func(b *testing.B) {
					s := versionedBenchServer(b, cell.status)
					body := bytes.Repeat([]byte("x"), payload.size)

					b.SetBytes(int64(payload.size * workers))
					b.ReportAllocs()

					for b.Loop() {
						err := fanOut(workers, func(worker int) error {
							got, _, err := s.send(b.Context(), http.MethodPut, fmt.Sprintf("/bucket/key-%d", worker), body, nil)

							return expectStatus("PUT", got, http.StatusOK, err)
						})
						require.NoError(b, err)
					}
				})
			}
		}
	}
}

func BenchmarkGetObjectByVersion(b *testing.B) {
	for _, workers := range benchConcurrency {
		b.Run(fmt.Sprintf("payload=1KiB/chain=%d/concurrency=%d", chainLength, workers), func(b *testing.B) {
			s := versionedBenchServer(b, "Enabled")
			body := bytes.Repeat([]byte("x"), 1<<10)

			var oldest string

			for i := range chainLength {
				got, id, err := s.send(b.Context(), http.MethodPut, "/bucket/key", body, nil)
				require.NoError(b, expectStatus("PUT", got, http.StatusOK, err))

				if i == 0 {
					oldest = id
				}
			}

			b.SetBytes(int64(len(body) * workers))
			b.ReportAllocs()

			for b.Loop() {
				err := fanOut(workers, func(int) error {
					got, _, err := s.send(b.Context(), http.MethodGet, "/bucket/key?versionId="+oldest, nil, nil)

					return expectStatus("GET", got, http.StatusOK, err)
				})
				require.NoError(b, err)
			}
		})
	}
}

// markerReset is how many iterations of the delete=marker row pass before the
// markers it wrote are removed with the timer stopped.
const markerReset = 64

// BenchmarkDeleteObjectVersioned measures the two deletes of a bucket with
// versioning. Under delete=marker an iteration is one DELETE of a key with no
// version id, which adds a delete marker; every markerReset iterations the
// markers are removed with the timer stopped, so the state the row holds, and
// the snapshot the persist=on rows save every persistFrequency, stays the same
// size however many iterations the run takes. Under delete=version an iteration
// is a PUT and a DELETE of the version it made, which leaves nothing behind.
func BenchmarkDeleteObjectVersioned(b *testing.B) {
	for _, persisted := range persistRows {
		for _, kind := range []string{"marker", "version"} {
			for _, workers := range benchConcurrency {
				name := persistName(persisted, fmt.Sprintf("delete=%s/concurrency=%d", kind, workers))

				b.Run(name, func(b *testing.B) {
					s := enableVersioning(b, serverFor(b, persisted), "Enabled")

					if kind == "marker" {
						benchDeleteMarker(b, s, workers)

						return
					}

					benchDeleteVersion(b, s, workers)
				})
			}
		}
	}
}

// benchDeleteMarker times the DELETEs that make delete markers. Each worker
// keeps the ids of its own markers, and removeMarkers removes them off the clock.
func benchDeleteMarker(b *testing.B, s *objectBenchServer, workers int) {
	b.Helper()

	markers := make([][]string, workers)

	b.ReportAllocs()

	for i := 1; b.Loop(); i++ {
		err := fanOut(workers, func(worker int) error {
			got, id, err := s.send(b.Context(), http.MethodDelete, fmt.Sprintf("/bucket/key-%d", worker), nil, nil)

			err = expectStatus("DELETE", got, http.StatusNoContent, err)
			if err != nil {
				return err
			}

			if id == "" {
				return errors.New("DELETE made no delete marker")
			}

			markers[worker] = append(markers[worker], id)

			return nil
		})
		require.NoError(b, err)

		if i%markerReset == 0 {
			b.StopTimer()
			removeMarkers(b, s, markers)
			b.StartTimer()
		}
	}
}

// removeMarkers deletes every marker version in markers and empties it.
func removeMarkers(b *testing.B, s *objectBenchServer, markers [][]string) {
	b.Helper()

	err := fanOut(len(markers), func(worker int) error {
		for _, id := range markers[worker] {
			got, _, err := s.send(b.Context(), http.MethodDelete,
				fmt.Sprintf("/bucket/key-%d?versionId=%s", worker, id), nil, nil)

			err = expectStatus("DELETE marker", got, http.StatusNoContent, err)
			if err != nil {
				return err
			}
		}

		markers[worker] = markers[worker][:0]

		return nil
	})
	require.NoError(b, err)
}

// benchDeleteVersion times a PUT and the DELETE of the version it made.
func benchDeleteVersion(b *testing.B, s *objectBenchServer, workers int) {
	b.Helper()

	body := bytes.Repeat([]byte("x"), 1<<10)

	b.ReportAllocs()

	for b.Loop() {
		err := fanOut(workers, func(worker int) error {
			target := fmt.Sprintf("/bucket/key-%d", worker)

			got, id, err := s.send(b.Context(), http.MethodPut, target, body, nil)

			err = expectStatus("PUT", got, http.StatusOK, err)
			if err != nil {
				return err
			}

			got, _, err = s.send(b.Context(), http.MethodDelete, target+"?versionId="+id, nil, nil)

			return expectStatus("DELETE", got, http.StatusNoContent, err)
		})
		require.NoError(b, err)
	}
}

// BenchmarkPutObjectLocked measures a PUT into a bucket with Object Lock whose
// lock comes from the request, from the bucket's default retention, or from
// neither.
func BenchmarkPutObjectLocked(b *testing.B) {
	for _, lock := range []string{"none", "request", "default"} {
		for _, workers := range benchConcurrency {
			b.Run(fmt.Sprintf("lock=%s/payload=1KiB/concurrency=%d", lock, workers), func(b *testing.B) {
				s := lockedBenchServer(b)
				body := bytes.Repeat([]byte("x"), 1<<10)

				var header map[string]string

				switch lock {
				case "request":
					header = map[string]string{headerLockMode: modeGovernance, headerLockUntil: farFuture}
				case "default":
					config := lockConfigBody(`<ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention>` +
						`<Mode>GOVERNANCE</Mode><Days>` + lockDays + `</Days></DefaultRetention></Rule>`)
					got, _, err := s.send(b.Context(), http.MethodPut, "/bucket?object-lock", config, nil)
					require.NoError(b, expectStatus("PUT object-lock", got, http.StatusOK, err))
				default:
				}

				b.SetBytes(int64(len(body) * workers))
				b.ReportAllocs()

				for b.Loop() {
					err := fanOut(workers, func(worker int) error {
						got, _, err := s.send(b.Context(), http.MethodPut, fmt.Sprintf("/bucket/key-%d", worker), body, header)

						return expectStatus("PUT", got, http.StatusOK, err)
					})
					require.NoError(b, err)
				}
			})
		}
	}
}
