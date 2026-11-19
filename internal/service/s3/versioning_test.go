package s3_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// lowerEnabled is "enabled": a status or mode in the wrong case, which S3
	// refuses, and a plain key or case name in a few tests.
	lowerEnabled       = "enabled"
	headerVersionID    = "X-Amz-Version-Id"
	headerDeleteMarker = "X-Amz-Delete-Marker"
	versionsPath       = "/bucket?versioning"
	listVersionsPath   = "/bucket?versions"
	codeNoSuchVersion  = "NoSuchVersion"
)

func versioningBody(status string) []byte {
	return []byte(`<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>` + status +
		`</Status></VersioningConfiguration>`)
}

func setVersioning(t *testing.T, s *server, status string) {
	t.Helper()

	got := s.do(call{method: http.MethodPut, target: versionsPath, region: regionUSEast1, body: versioningBody(status)})
	require.Equal(t, http.StatusOK, got.status, string(got.body))
}

// putVersion puts the body under the key and returns the version id the reply
// names, "" when it names none.
func putVersion(t *testing.T, s *server, body string) string {
	t.Helper()

	got := s.do(call{method: http.MethodPut, target: objectPath, region: regionUSEast1, body: []byte(body)})
	require.Equal(t, http.StatusOK, got.status, string(got.body))

	return got.header.Get(headerVersionID)
}

func TestBucketVersioningStatus(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	before := s.do(call{method: http.MethodGet, target: versionsPath, region: regionUSEast1})
	require.Equal(t, http.StatusOK, before.status)
	assert.NotContains(t, string(before.body), "<Status>", "a bucket that never had versioning has no status")

	setVersioning(t, s, "Enabled")

	enabled := s.do(call{method: http.MethodGet, target: versionsPath, region: regionUSEast1})
	assert.Contains(t, string(enabled.body), "<Status>Enabled</Status>")

	setVersioning(t, s, "Suspended")

	suspended := s.do(call{method: http.MethodGet, target: versionsPath, region: regionUSEast1})
	assert.Contains(t, string(suspended.body), "<Status>Suspended</Status>")

	mfa := s.do(call{
		method: http.MethodPut, target: versionsPath, region: regionUSEast1,
		body: []byte(`<VersioningConfiguration><Status>Enabled</Status><MfaDelete>Enabled</MfaDelete>` +
			`</VersioningConfiguration>`),
	})
	require.Equal(t, http.StatusOK, mfa.status, string(mfa.body))

	stored := s.do(call{method: http.MethodGet, target: versionsPath, region: regionUSEast1})
	assert.Contains(t, string(stored.body), "<MfaDelete>Enabled</MfaDelete>")

	for status, wantCode := range map[string]string{lowerEnabled: codeMalformedXML, "Disabled": codeMalformedXML} {
		got := s.do(call{method: http.MethodPut, target: versionsPath, region: regionUSEast1, body: versioningBody(status)})
		require.Equal(t, http.StatusBadRequest, got.status, status)
		assert.Equal(t, wantCode, got.err(t).Code, status)
	}

	empty := s.do(call{
		method: http.MethodPut, target: versionsPath, region: regionUSEast1,
		body: []byte(`<VersioningConfiguration/>`),
	})
	require.Equal(t, http.StatusBadRequest, empty.status)
	assert.Equal(t, "IllegalVersioningConfigurationException", empty.err(t).Code)

	missing := s.do(call{method: http.MethodGet, target: "/missing?versioning", region: regionUSEast1})
	assert.Equal(t, http.StatusNotFound, missing.status)
}

func TestVersionChainAndDeleteMarkers(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)
	setVersioning(t, s, "Enabled")

	first := putVersion(t, s, "one")
	second := putVersion(t, s, "two")

	require.Len(t, first, 32, "a version id is 32 characters")
	assert.NotEqual(t, first, second)

	current := get(s, http.MethodGet, objectPath, nil)
	assert.Equal(t, "two", string(current.body))
	assert.Equal(t, second, current.header.Get(headerVersionID))

	older := get(s, http.MethodGet, objectPath+"?versionId="+first, nil)
	assert.Equal(t, "one", string(older.body))
	assert.Equal(t, first, older.header.Get(headerVersionID))

	unknown := get(s, http.MethodGet, objectPath+"?versionId=nope", nil)
	require.Equal(t, http.StatusNotFound, unknown.status)
	assert.Equal(t, codeNoSuchVersion, unknown.err(t).Code)

	marked := s.do(call{method: http.MethodDelete, target: objectPath, region: regionUSEast1})
	require.Equal(t, http.StatusNoContent, marked.status)
	assert.Equal(t, valueTrue, marked.header.Get(headerDeleteMarker))

	marker := marked.header.Get(headerVersionID)
	require.NotEmpty(t, marker)

	gone := get(s, http.MethodGet, objectPath, nil)
	require.Equal(t, http.StatusNotFound, gone.status)
	assert.Equal(t, codeNoSuchKey, gone.err(t).Code)
	assert.Equal(t, valueTrue, gone.header.Get(headerDeleteMarker))
	assert.Equal(t, marker, gone.header.Get(headerVersionID))

	named := get(s, http.MethodGet, objectPath+"?versionId="+marker, nil)
	require.Equal(t, http.StatusMethodNotAllowed, named.status)
	assert.Equal(t, http.MethodDelete, named.header.Get("Allow"))
	assert.Equal(t, valueTrue, named.header.Get(headerDeleteMarker))

	listed := get(s, http.MethodGet, listVersionsPath, nil)
	assert.Contains(t, string(listed.body), "<DeleteMarker>")
	assert.Equal(t, 2, strings.Count(string(listed.body), "<Version>"))

	// A bucket with versions or delete markers is not empty.
	refused := s.do(call{method: http.MethodDelete, target: bucketPath, region: regionUSEast1})
	require.Equal(t, http.StatusConflict, refused.status)
	assert.Contains(t, string(refused.body), "You must delete all versions in the bucket.")

	removed := s.do(call{method: http.MethodDelete, target: objectPath + "?versionId=" + marker, region: regionUSEast1})
	require.Equal(t, http.StatusNoContent, removed.status)
	assert.Equal(t, valueTrue, removed.header.Get(headerDeleteMarker), "the version removed was a delete marker")
	assert.Equal(t, marker, removed.header.Get(headerVersionID))

	back := get(s, http.MethodGet, objectPath, nil)
	assert.Equal(t, "two", string(back.body), "removing the delete marker brings the object back")

	invalid := s.do(call{method: http.MethodDelete, target: objectPath + "?versionId=nope", region: regionUSEast1})
	require.Equal(t, http.StatusBadRequest, invalid.status)
	assert.Equal(t, "InvalidArgument", invalid.err(t).Code)

	for _, id := range []string{first, second} {
		removed = s.do(call{method: http.MethodDelete, target: objectPath + "?versionId=" + id, region: regionUSEast1})
		require.Equal(t, http.StatusNoContent, removed.status)
		assert.Empty(t, removed.header.Get(headerDeleteMarker))
	}

	emptied := s.do(call{method: http.MethodDelete, target: bucketPath, region: regionUSEast1})
	assert.Equal(t, http.StatusNoContent, emptied.status, "no version is left")
}

func TestSuspendedVersioningKeepsOneNullVersion(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	assert.Empty(t, putVersion(t, s, "before"))

	unversioned := get(s, http.MethodGet, objectPath, nil)
	assert.Empty(t, unversioned.header.Get(headerVersionID), "a bucket that never had versioning names no version")

	setVersioning(t, s, "Enabled")

	pre := get(s, http.MethodGet, objectPath, nil)
	assert.Equal(t, "null", pre.header.Get(headerVersionID), "an object from before versioning is the null version")

	enabled := putVersion(t, s, lowerEnabled)
	require.NotEmpty(t, enabled)

	setVersioning(t, s, "Suspended")

	assert.Empty(t, putVersion(t, s, "suspended-1"), "a suspended bucket's PUT names no version")
	assert.Empty(t, putVersion(t, s, "suspended-2"))

	current := get(s, http.MethodGet, objectPath, nil)
	assert.Equal(t, "suspended-2", string(current.body))
	assert.Equal(t, "null", current.header.Get(headerVersionID))

	listed := get(s, http.MethodGet, listVersionsPath, nil)
	assert.Equal(t, 2, strings.Count(string(listed.body), "<Version>"), "the null version was replaced, not added")

	marked := s.do(call{method: http.MethodDelete, target: objectPath, region: regionUSEast1})
	assert.Equal(t, "null", marked.header.Get(headerVersionID), "a suspended bucket's delete marker is null")
	assert.Equal(t, valueTrue, marked.header.Get(headerDeleteMarker))

	listed = get(s, http.MethodGet, listVersionsPath, nil)
	assert.Equal(t, 1, strings.Count(string(listed.body), "<Version>"), "the delete marker replaced the null version")
	assert.Equal(t, 1, strings.Count(string(listed.body), "<DeleteMarker>"))

	kept := get(s, http.MethodGet, objectPath+"?versionId="+enabled, nil)
	assert.Equal(t, lowerEnabled, string(kept.body))
}

func TestDeleteObjectsInAVersionedBucket(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)
	setVersioning(t, s, "Enabled")

	version := putVersion(t, s, "body")

	multi := func(objects string) reply {
		return s.do(call{
			method: http.MethodPost, target: "/bucket?delete", region: regionUSEast1,
			body: []byte(`<Delete>` + objects + `</Delete>`),
		})
	}

	marked := multi(`<Object><Key>key</Key></Object><Object><Key>other</Key></Object>`)
	require.Equal(t, http.StatusOK, marked.status, string(marked.body))
	assert.Equal(t, 2, strings.Count(string(marked.body), "<DeleteMarkerVersionId>"))
	assert.Equal(t, 2, strings.Count(string(marked.body), "<DeleteMarker>true</DeleteMarker>"))

	unknown := multi(`<Object><Key>key</Key><VersionId>nope</VersionId></Object>` +
		`<Object><Key>absent</Key><VersionId>nope</VersionId></Object>`)
	require.Equal(t, http.StatusOK, unknown.status, string(unknown.body))
	assert.Equal(t, 2, strings.Count(string(unknown.body), "<Code>"+codeNoSuchVersion+"</Code>"))

	removed := multi(`<Object><Key>key</Key><VersionId>` + version + `</VersionId></Object>`)
	assert.Contains(t, string(removed.body), "<VersionId>"+version+"</VersionId>")
	assert.NotContains(t, string(removed.body), "<DeleteMarker>")

	gone := get(s, http.MethodGet, objectPath+"?versionId="+version, nil)
	assert.Equal(t, http.StatusNotFound, gone.status)
}
