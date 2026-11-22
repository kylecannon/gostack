package s3_test

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// errListing is a request or a page that did not come out as it must.
var errListing = errors.New("listing went wrong")

// at is do for a spawned goroutine: it returns what went wrong instead of
// failing the test.
func (s *server) at(method, target string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(s.t.Context(), method, s.url+target, http.NoBody)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20250101/us-east-1/s3/aws4_request, "+
		"SignedHeaders=host, Signature=0")

	resp, err := testClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("send request: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read body: %w", err)
	}

	return resp.StatusCode, body, nil
}

// expect sends a request and fails when the answer is not status.
func (s *server) expect(status int, method, target string) ([]byte, error) {
	got, body, err := s.at(method, target)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, target, err)
	}

	if got != status {
		return nil, fmt.Errorf("%w: %s %s: status %d, want %d: %.200s", errListing, method, target, got, status, body)
	}

	return body, nil
}

// pageAll lists the bucket a page at a time with ListObjectsV2 and returns every
// key it was given, in the order it got them.
func (s *server) pageAll(query string) ([]string, error) {
	var (
		keys  []string
		token string
	)

	for {
		target := bucketPath + "?list-type=2&max-keys=7" + query
		if token != "" {
			target += "&continuation-token=" + url.QueryEscape(token)
		}

		body, err := s.expect(http.StatusOK, http.MethodGet, target)
		if err != nil {
			return keys, err
		}

		var page listResult

		err = xml.Unmarshal(body, &page)
		if err != nil {
			return keys, fmt.Errorf("decode list: %w", err)
		}

		keys = append(keys, page.keys()...)

		if !page.IsTruncated {
			return keys, nil
		}

		token = page.NextContinuationToken
	}
}

// churn puts and deletes keys before, between and after the stable ones.
func (s *server) churn(worker, rounds, stable int) error {
	for round := range rounds {
		for _, key := range []string{
			fmt.Sprintf("aaa/%d-%d", worker, round),
			fmt.Sprintf("keep/%03d-%d", round%stable, worker),
			fmt.Sprintf("zzz/%d-%d", worker, round),
		} {
			_, err := s.expect(http.StatusOK, http.MethodPut, bucketPath+"/"+key)
			if err != nil {
				return err
			}

			_, err = s.expect(http.StatusNoContent, http.MethodDelete, bucketPath+"/"+key)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// checkPaging pages the bucket and checks what a concurrent writer cannot
// change: the keys are in order, none is listed twice, and every key of want,
// which was there throughout, is listed unless a delimiter folded it.
func (s *server) checkPaging(query string, want []string) error {
	keys, err := s.pageAll(query)
	if err != nil {
		return err
	}

	if !slices.IsSorted(keys) {
		return fmt.Errorf("%w: query %q: keys out of order: %v", errListing, query, keys)
	}

	seen := make(map[string]bool, len(keys))

	for _, key := range keys {
		if seen[key] {
			return fmt.Errorf("%w: query %q: %s listed twice", errListing, query, key)
		}

		seen[key] = true
	}

	if strings.Contains(query, "delimiter") {
		return nil
	}

	for _, key := range want {
		if !seen[key] {
			return fmt.Errorf("%w: query %q: %s was in the bucket the whole time and was not listed",
				errListing, query, key)
		}
	}

	return nil
}

// TestListWhilePutsAndDeletesChangeTheBucket pages a bucket with ListObjectsV2
// and ListObjectVersions while other goroutines put and delete keys around the
// ones that stay. Every page must be in key order, list no key twice, and list
// every key that was there for the whole listing; under -race it also shows the
// walk and the index writes share the bucket lock.
func TestListWhilePutsAndDeletesChangeTheBucket(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	const stable, churners, rounds = 60, 4, 40

	want := make([]string, 0, stable)

	for i := range stable {
		key := fmt.Sprintf("keep/%03d", i)
		want = append(want, key)

		got := s.do(call{method: http.MethodPut, target: bucketPath + "/" + key, region: regionUSEast1})
		require.Equal(t, http.StatusOK, got.status, string(got.body))
	}

	var wg sync.WaitGroup

	run := func(fn func() error) {
		wg.Go(func() {
			err := fn()
			if err != nil {
				t.Error(err)
			}
		})
	}

	for churner := range churners {
		run(func() error { return s.churn(churner, rounds, stable) })
	}

	for _, query := range []string{"", "&delimiter=%2F", "&prefix=keep%2F"} {
		run(func() error {
			for range rounds / 4 {
				err := s.checkPaging(query, want)
				if err != nil {
					return err
				}
			}

			return nil
		})
	}

	run(func() error {
		for range rounds / 4 {
			_, err := s.expect(http.StatusOK, http.MethodGet, bucketPath+"?versions&max-keys=9&prefix=keep%2F")
			if err != nil {
				return err
			}
		}

		return nil
	})

	wg.Wait()

	// What is left is exactly the stable keys.
	keys, err := s.pageAll("")
	require.NoError(t, err)
	require.Equal(t, want, keys)
}
