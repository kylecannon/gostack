package ssm_test

import (
	"encoding/json"
	"strconv"
	"sync"
	"testing"

	"github.com/kylecannon/gostack/internal/service/ssm"
)

// The test of two pagers and a writer, from the second review of SSM
// (SQ-277). Two callers page through listings in a loop, sharing the cursors
// of the State, while a third deletes, overwrites, tags, labels and untags the
// parameters they page through. Each page is checked for itself: no name twice
// in it, and a token that is the offset it was read at plus its length. What
// the pages hold changes with the writes, so the race detector is the check
// of the rest, as in Test_listingsWhileWriting.

const (
	concParameters = 200
	concPrefix     = "/q/"
)

// concName is the name of parameter i, under one of three paths.
func concName(i int) string {
	return concPrefix + strconv.Itoa(i%3) + "/p" + strconv.Itoa(i)
}

// concPager pages through listings, 40 rounds of up to 500 pages each, and
// reports every page that breaks the checks above.
func concPager(t *testing.T, d direct, worker int) {
	t.Helper()

	queries := []obj{
		{kPath: "/q", kRecursive: true, fieldMaxResults: 3},
		{kPath: "/q/1", fieldMaxResults: 7},
		{kPath: "/q", kRecursive: true, fieldFilters: []obj{{fieldKey: "tag:k"}}},
	}

	for round := range 40 {
		q := queries[(worker+round)%len(queries)]
		if worker == 0 {
			q = queries[0]
		}

		op := kGetParametersByPath
		if round%4 == 3 {
			op, q = kDescribeParameters, obj{}
		}

		if !concWalk(t, d, op, q) {
			return
		}
	}
}

// concWalk pages through one listing and returns false after the first
// answer that is not a page, or is a page that fails a check.
func concWalk(t *testing.T, d direct, op string, q obj) bool {
	t.Helper()

	token := ""

	for range 500 {
		body := q
		if token != "" {
			body = with(q, fieldNextToken, token)
		}

		status, out := d.do(op, body)
		if status != 200 {
			t.Errorf("%s %d %s", op, status, out)

			return false
		}

		var resp pagedAnswer

		err := json.Unmarshal(out, &resp)
		if err != nil {
			t.Errorf("%s: %v", op, err)

			return false
		}

		seen := map[string]bool{}

		for _, p := range resp.Parameters {
			if seen[p.Name] {
				t.Errorf("duplicate in one page: %s", p.Name)
			}

			seen[p.Name] = true
		}

		if resp.NextToken == "" {
			return true
		}

		offset, _ := strconv.Atoi(token)
		if resp.NextToken != strconv.Itoa(offset+len(resp.Parameters)) {
			t.Errorf("token %s after %q and %d parameters", resp.NextToken, token, len(resp.Parameters))
		}

		token = resp.NextToken
	}

	return true
}

// concWrite is the i-th write of the writer.
func concWrite(d direct, i int) {
	name := concName(i % concParameters)
	tag := []obj{{fieldKey: "k", kValue: "v"}}

	switch i % 5 {
	case 0:
		d.do(kDeleteParameter, obj{kName: name})
	case 1:
		d.do(kPutParameter, obj{kName: name, kValue: "w", kType: kString, kOverwrite: true})
	case 2:
		d.do(kAddTagsToResource, obj{kResourceType: resourceParameter, kResourceID: name, kTags: tag})
	case 3:
		d.do(kLabelParameterVersion, obj{kName: name, fieldLabels: []string{"l"}})
	default:
		d.do(kRemoveTagsFromResource, obj{kResourceType: resourceParameter, kResourceID: name, fieldTagKeys: []string{"k"}})
	}
}

func Test_twoPagersAndAWriter(t *testing.T) {
	d := direct{tb: t, h: handlerFor(t, ssm.New(), nil)}

	for i := range concParameters {
		d.do(kPutParameter, obj{kName: concName(i), kValue: "v", kType: kString})
	}

	var pagers, writer sync.WaitGroup

	stop := make(chan struct{})

	for worker := range 2 {
		pagers.Go(func() { concPager(t, d, worker) })
	}

	writer.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				concWrite(d, i)
			}
		}
	})

	pagers.Wait()
	close(stop)
	writer.Wait()
}
