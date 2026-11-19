package s3_test

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkPostObject measures a browser upload form through the real
// gateway over HTTP, with and without a policy, across the payload axis:
//
//	BenchmarkPostObject/policy=on/payload=64KiB
//
// The 1 MiB file spills to disk, the smaller ones stay in memory.
func BenchmarkPostObject(b *testing.B) {
	s := newObjectBenchServer(b)

	for _, withPolicy := range []bool{false, true} {
		for _, payload := range []benchPayload{{"1KiB", 1 << 10}, {"64KiB", 64 << 10}, {"1MiB", 1 << 20}} {
			name := "policy=off/payload=" + payload.name
			if withPolicy {
				name = "policy=on/payload=" + payload.name
			}

			b.Run(name, func(b *testing.B) {
				fields := [][2]string{{fieldKey, "post"}}
				if withPolicy {
					fields = signedForm(b, "post", []any{"starts-with", conditionKey, "p"},
						[]any{conditionLength, 0, payload.size})
				}

				body, contentType := benchForm(b, fields, payload.size)

				b.SetBytes(int64(payload.size))
				b.ReportAllocs()

				for b.Loop() {
					status, err := s.post(b, body, contentType)
					require.NoError(b, err)
					require.Equal(b, http.StatusNoContent, status)
				}
			})
		}
	}
}

func benchForm(b *testing.B, fields [][2]string, size int) ([]byte, string) {
	b.Helper()

	var body bytes.Buffer

	mw := multipart.NewWriter(&body)
	for _, field := range fields {
		require.NoError(b, mw.WriteField(field[0], field[1]))
	}

	part, err := mw.CreateFormFile("file", "f.bin")
	require.NoError(b, err)

	_, err = part.Write(bytes.Repeat([]byte("x"), size))
	require.NoError(b, err)
	require.NoError(b, mw.Close())

	return body.Bytes(), mw.FormDataContentType()
}

// post sends a form to the bench bucket on its virtual host.
func (s *objectBenchServer) post(b *testing.B, body []byte, contentType string) (int, error) {
	b.Helper()

	req, err := http.NewRequestWithContext(b.Context(), http.MethodPost, s.url+"/", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}

	req.Host = "bucket.s3." + localstackHost
	req.Header.Set("Content-Type", contentType)

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("send request: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	_, err = io.Copy(io.Discard, resp.Body)
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	return resp.StatusCode, nil
}
