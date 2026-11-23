package s3_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kylecannon/gostack/internal/blob"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/s3"
	"github.com/stretchr/testify/require"
)

// The SSE-KMS rows of the PutObject matrix: the payload and concurrency axes of
// BenchmarkPutObject, crossed with how the write names its KMS key. KMS is
// fakeKMS behind S3's in-process client, so a row measures S3's side of the
// call and not KMS's work. For example
//
//	BenchmarkPutObjectSSEKMS/sse=kms-validated/payload=1MiB/concurrency=16
//
// sse=kms-managed names no key, so the account's managed key is used (made on
// the first write); sse=kms-key names a key id; sse=kms-validated names it
// with S3_SKIP_KMS_KEY_VALIDATION=0, a DescribeKey on every write.

// kmsBenchPayloads are the 1 KiB and 1 MiB rows of benchPayloads: one in
// memory, one spilled to a file.
var kmsBenchPayloads = []benchPayload{benchPayloads[0], benchPayloads[2]}

type kmsBenchOption struct {
	name     string
	header   map[string]string
	validate bool
}

var kmsBenchOptions = []kmsBenchOption{
	{name: "kms-managed", header: map[string]string{headerSSE: sseKMS}},
	{name: "kms-key", header: map[string]string{headerSSE: sseKMS, headerSSEKMSKey: "bench"}},
	{name: "kms-validated", header: map[string]string{headerSSE: sseKMS, headerSSEKMSKey: "bench"}, validate: true},
}

// newKMSBenchServer serves S3 through the real gateway with its KMS calls
// answered by a fakeKMS that holds the key the rows name.
func newKMSBenchServer(b *testing.B, validate bool) *objectBenchServer {
	b.Helper()

	provider := s3.New(func() s3.Settings {
		return s3.Settings{LocalstackHost: localstackHost, ValidateKMSKeys: validate}
	}, blob.NewEphemeral(b.TempDir()))
	b.Cleanup(func() { provider.Close(context.WithoutCancel(b.Context())) })

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{benchListen}} },
		[]service.Registration{provider.Registration()},
		nil,
		nil,
	)
	require.NoError(b, err)

	fake := newFakeKMS()
	fake.add("arn:aws:kms:us-east-1:000000000000:key/bench")
	bindKMS(provider, gw.Handler(), fake)

	srv := httptest.NewServer(gw.Handler())
	b.Cleanup(srv.Close)

	transport := &http.Transport{MaxIdleConnsPerHost: 64}
	b.Cleanup(transport.CloseIdleConnections)

	s := &objectBenchServer{url: srv.URL, client: &http.Client{Transport: transport}}

	status, err := s.call(b.Context(), http.MethodPut, "/bucket", nil)
	require.NoError(b, err)
	require.Equal(b, http.StatusOK, status)

	return s
}

// putWith sends one PutObject with header and reads the whole response.
func (s *objectBenchServer) putWith(ctx context.Context, target string, body []byte, header map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.url+target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Authorization", benchAuth)

	for name, value := range header {
		req.Header.Set(name, value)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	_, err = io.Copy(io.Discard, resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK || resp.Header.Get(headerSSEKMSKey) == "" {
		return fmt.Errorf("PUT answered %d with key %q", resp.StatusCode, resp.Header.Get(headerSSEKMSKey))
	}

	return nil
}

// BenchmarkGetObjectSSEKMS is the read side of sse=kms-validated: with
// S3_SKIP_KMS_KEY_VALIDATION=0 GetObject looks up the key of the object it
// reads with a DescribeKey, on every read. The same payload and concurrency
// axes as BenchmarkGetObject, to read against its no-KMS rows. For example
//
//	BenchmarkGetObjectSSEKMS/sse=kms-validated/payload=1MiB/concurrency=16
func BenchmarkGetObjectSSEKMS(b *testing.B) {
	option := kmsBenchOptions[len(kmsBenchOptions)-1]

	for _, payload := range kmsBenchPayloads {
		for _, workers := range benchConcurrency {
			name := fmt.Sprintf("sse=%s/payload=%s/concurrency=%d", option.name, payload.name, workers)

			b.Run(name, func(b *testing.B) {
				s := newKMSBenchServer(b, option.validate)
				body := bytes.Repeat([]byte("x"), payload.size)

				for worker := range workers {
					err := s.putWith(b.Context(), fmt.Sprintf("/bucket/key-%d", worker), body, option.header)
					require.NoError(b, err)
				}

				b.SetBytes(int64(payload.size * workers))
				b.ReportAllocs()

				for b.Loop() {
					err := fanOut(workers, func(worker int) error {
						status, err := s.call(b.Context(), http.MethodGet, fmt.Sprintf("/bucket/key-%d", worker), nil)
						if err == nil && status != http.StatusOK {
							err = fmt.Errorf("GET answered %d", status)
						}

						return err
					})
					require.NoError(b, err)
				}
			})
		}
	}
}

func BenchmarkPutObjectSSEKMS(b *testing.B) {
	for _, option := range kmsBenchOptions {
		for _, payload := range kmsBenchPayloads {
			for _, workers := range benchConcurrency {
				name := fmt.Sprintf("sse=%s/payload=%s/concurrency=%d", option.name, payload.name, workers)

				b.Run(name, func(b *testing.B) {
					skipLeftOut(b)

					s := newKMSBenchServer(b, option.validate)
					body := bytes.Repeat([]byte("x"), payload.size)

					b.SetBytes(int64(payload.size * workers))
					b.ReportAllocs()

					for b.Loop() {
						err := fanOut(workers, func(worker int) error {
							return s.putWith(b.Context(), fmt.Sprintf("/bucket/key-%d", worker), body, option.header)
						})
						require.NoError(b, err)
					}
				})
			}
		}
	}
}
