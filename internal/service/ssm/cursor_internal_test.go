package ssm

import (
	"strconv"
	"testing"
	"time"

	ssmapi "github.com/kylecannon/gostack/internal/api/ssm"
	"github.com/stretchr/testify/require"
)

// putOne sends the put a PutParameter makes to s, as PutParameter does: under
// the write lock. It returns the error the put was refused with.
func putOne(s *State, name string, overwrite bool) error {
	s.lock()
	defer s.mu.Unlock()

	_, err := s.put(name, &ssmapi.PutParameterInput{
		Name: &name, Value: new("v"), Type: new(typeString), Overwrite: &overwrite,
	}, time.Time{})

	return err
}

// namesOf is the names of the versions of a page.
func namesOf(page []*version) []string {
	names := make([]string, 0, len(page))
	for _, v := range page {
		names = append(names, v.Name)
	}

	return names
}

// Test_aPutThatCannotMoveAPageKeepsTheCursors: a page that ends full leaves a
// cursor, and a write drops it unless the write cannot have changed a page. A
// put refused, and a put that makes a parameter, which takes the last place in
// order, keep it; a put that adds a version, and a delete, drop it.
func Test_aPutThatCannotMoveAPageKeepsTheCursors(t *testing.T) {
	const (
		parameters = 25
		limit      = 10
	)

	all := func(string, *parameter) bool { return true }
	listing := listingKey{1}

	s := newState("", "")

	for i := range parameters {
		require.NoError(t, putOne(s, "p"+strconv.Itoa(i), false))
	}

	// remember leaves a cursor after the first page and returns where it is.
	remember := func() int {
		s.mu.RLock()
		defer s.mu.RUnlock()

		page, next := s.page(listing, 0, limit, all)
		require.Len(t, page, limit)
		require.NotNil(t, next)

		at, found := s.cursor(listing, limit)
		require.True(t, found)

		return at
	}

	cursor := func() (int, bool) {
		s.mu.RLock()
		defer s.mu.RUnlock()

		return s.cursor(listing, limit)
	}

	at := remember()

	require.ErrorContains(t, putOne(s, "p3", false), "already exists")

	kept, found := cursor()
	require.True(t, found, "a put that is refused kept the cursor")
	require.Equal(t, at, kept)

	require.NoError(t, putOne(s, "p"+strconv.Itoa(parameters), false))

	kept, found = cursor()
	require.True(t, found, "a put that makes a parameter kept the cursor")
	require.Equal(t, at, kept)

	// The page the cursor gives is the one a walk from the start gives: the
	// parameters after the first ten, the new one last.
	s.mu.RLock()
	fromCursor, _ := s.page(listing, limit, limit, all)
	s.mu.RUnlock()

	require.Len(t, fromCursor, limit)
	require.Equal(t, "p10", fromCursor[0].Name)

	s.mu.RLock()
	last, next := s.page(listing, 2*limit, limit, all)
	s.mu.RUnlock()

	require.Equal(t, []string{"p20", "p21", "p22", "p23", "p24", "p25"}, namesOf(last))
	require.Nil(t, next)

	// A put that adds a version can change what a filter sees.
	require.NoError(t, putOne(s, "p3", true))

	_, found = cursor()
	require.False(t, found, "a put that adds a version dropped the cursor")

	// The cache is made again, and a delete drops it.
	remember()

	s.lock()
	require.True(t, s.remove("p3"))
	s.mu.Unlock()

	_, found = cursor()
	require.False(t, found, "a delete dropped the cursor")
}

// Test_theCursorsOfAnEmptyStateWork: a State no write has dropped the cursors
// of, as the first listing of a new one is, still has somewhere to put them.
func Test_theCursorsOfAnEmptyStateWork(t *testing.T) {
	s := newState("", "")

	require.NoError(t, putOne(s, "a", false))

	s.mu.RLock()
	defer s.mu.RUnlock()

	page, next := s.page(listingKey{2}, 0, 1, func(string, *parameter) bool { return true })
	require.Equal(t, []string{"a"}, namesOf(page))
	require.NotNil(t, next)

	_, found := s.cursor(listingKey{2}, 1)
	require.True(t, found)
}

// Test_aListingKeyIsTheRequestWithoutItsPageSize: the key of a listing does
// not change with the token or the page size, and does change with anything
// else the request says, so a cursor made for one listing is never used by
// another.
func Test_aListingKeyIsTheRequestWithoutItsPageSize(t *testing.T) {
	filter := func(key, option string, values ...string) ssmapi.ParameterStringFilter {
		return ssmapi.ParameterStringFilter{Key: &key, Option: &option, Values: values}
	}

	keys := map[string]listingKey{
		"all":          byPathListing("/a/", nil, nil),
		"recursive":    byPathListing("/a/", new(true), nil),
		"another path": byPathListing("/b/", nil, nil),
		"one value":    byPathListing("/a/", nil, []ssmapi.ParameterStringFilter{filter("Type", "Equals", "String")}),
		"two values":   byPathListing("/a/", nil, []ssmapi.ParameterStringFilter{filter("Type", "Equals", "String", "x")}),
		"joined":       byPathListing("/a/", nil, []ssmapi.ParameterStringFilter{filter("Type", "Equals", "Stringx")}),
		"another key":  byPathListing("/a/", nil, []ssmapi.ParameterStringFilter{filter("Tier", "Equals", "String")}),
		"describe":     describeListing(nil, nil),
		"describe one": describeListing([]ssmapi.ParameterStringFilter{filter("Type", "Equals", "String")}, nil),
		"legacy": describeListing(nil, []ssmapi.ParametersFilter{
			{Key: new("Type"), Values: []string{"String"}},
		}),
	}

	seen := map[listingKey]string{}

	for name, key := range keys {
		other, dup := seen[key]
		require.False(t, dup, "%s and %s share a key", name, other)

		seen[key] = name
	}

	require.Equal(t, keys["all"], byPathListing("/a/", nil, nil))
	require.Equal(t, keys["one value"], byPathListing("/a/", nil, []ssmapi.ParameterStringFilter{
		filter("Type", "Equals", "String"),
	}))
}
