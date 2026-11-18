package rest_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol/rest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const bucketLabel, keyLabel = "Bucket", "Key"

func Test_Pattern(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		givePath   string
		wantLabels rest.Labels
		wantMatch  bool
	}{
		{
			name:     "greedy label keeps escaped slash, double slash and dot-dot",
			givePath: "/b/a%2Fb//c/../d", wantMatch: true,
			wantLabels: rest.Labels{{Name: bucketLabel, Value: "b"}, {Name: keyLabel, Value: "a/b//c/../d"}},
		},
		{
			name:     "greedy label keeps a trailing slash",
			givePath: "/b/dir/", wantMatch: true,
			wantLabels: rest.Labels{{Name: bucketLabel, Value: "b"}, {Name: keyLabel, Value: "dir/"}},
		},
		{
			name:     "a label is unescaped exactly once",
			givePath: "/b/%252F%20x", wantMatch: true,
			wantLabels: rest.Labels{{Name: bucketLabel, Value: "b"}, {Name: keyLabel, Value: "%2F x"}},
		},
		{name: "greedy label never matches nothing", givePath: "/b/", wantMatch: false},
		{name: "a bad escape matches nothing", givePath: "/b/%zz", wantMatch: false},
		{name: "a missing slash matches nothing", givePath: "b/k", wantMatch: false},
	}

	p := rest.MustParsePattern("/{Bucket}/{Key+}?x-id=GetObject")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			labels, ok := p.Match(tt.givePath)
			require.Equal(t, tt.wantMatch, ok)
			assert.Equal(t, tt.wantLabels, labels)
		})
	}
}

func Test_Pattern_Segments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		giveURI    string
		givePath   string
		wantLabels rest.Labels
		wantMatch  bool
	}{
		{name: "root", giveURI: "/", givePath: "/", wantMatch: true},
		{
			name: "trailing slashes are ignored", giveURI: "/{Bucket}?uploads", givePath: "/b//", wantMatch: true,
			wantLabels: rest.Labels{{Name: bucketLabel, Value: "b"}},
		},
		{
			name: "an escaped slash stays inside a plain label", giveURI: "/{Bucket}", givePath: "/b%2Fc", wantMatch: true,
			wantLabels: rest.Labels{{Name: bucketLabel, Value: "b/c"}},
		},
		{name: "a plain label is one segment", giveURI: "/{Bucket}", givePath: "/b/c", wantMatch: false},
		{name: "a literal must match", giveURI: "/{a}/lit", givePath: "/x/other", wantMatch: false},
		{
			name: "a literal may hold regexp characters", giveURI: "/ReDos/{s}/(a+)+", givePath: "/ReDos/x/(a+)+",
			wantMatch: true, wantLabels: rest.Labels{{Name: "s", Value: "x"}},
		},
		{name: "an empty segment is no label", giveURI: "/{a}/{b}", givePath: "/x//y", wantMatch: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			labels, ok := rest.MustParsePattern(tt.giveURI).Match(tt.givePath)
			require.Equal(t, tt.wantMatch, ok)
			assert.Equal(t, tt.wantLabels, labels)
		})
	}
}

func Test_ParsePattern_Errors(t *testing.T) {
	t.Parallel()

	for _, uri := range []string{"no-slash", "/{a+}/b", "/x{a}", "/{}", "/{a}}"} {
		t.Run(uri, func(t *testing.T) {
			t.Parallel()

			_, err := rest.ParsePattern(uri)
			require.Error(t, err)
		})
	}
}

// FuzzPatternMatch checks that a bucket and a key escaped the way a client
// escapes them come back from Match exactly as they were.
func FuzzPatternMatch(f *testing.F) {
	f.Add("bucket", "a/b//c/../d")
	f.Add("b%2F", "%2F/ /+/?#")
	f.Add("x", "/")

	p := rest.MustParsePattern("/{Bucket}/{Key+}")

	f.Fuzz(func(t *testing.T, bucket, key string) {
		if bucket == "" || key == "" {
			return
		}

		parts := strings.Split(key, "/")
		for i, part := range parts {
			parts[i] = url.PathEscape(part)
		}

		labels, ok := p.Match("/" + url.PathEscape(bucket) + "/" + strings.Join(parts, "/"))
		require.True(t, ok)

		gotBucket, _ := labels.Get(bucketLabel)
		gotKey, _ := labels.Get(keyLabel)
		require.Equal(t, bucket, gotBucket)
		require.Equal(t, key, gotKey)
	})
}
