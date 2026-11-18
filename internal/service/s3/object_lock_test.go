package s3_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	headerLockEnabled = "X-Amz-Bucket-Object-Lock-Enabled"
	headerLockMode    = "X-Amz-Object-Lock-Mode"
	headerLockUntil   = "X-Amz-Object-Lock-Retain-Until-Date"
	headerLockHold    = "X-Amz-Object-Lock-Legal-Hold"
	headerBypass      = "X-Amz-Bypass-Governance-Retention"
	farFuture         = "2099-01-01T00:00:00Z"
	laterFuture       = "2099-06-01T00:00:00Z"
	sooner            = "2098-01-01T00:00:00Z"
	lockPath          = "/bucket?object-lock"
	retentionQuery    = "?retention"
	legalHoldQuery    = "?legal-hold"
	versionIDQuery    = "&versionId="
	modeGovernance    = "GOVERNANCE"
	modeCompliance    = "COMPLIANCE"
	modeBad           = "BAD"
	copyLockedPath    = "/bucket/copy-locked"
)

func createLockedBucket(t *testing.T, s *server) {
	t.Helper()

	got := s.do(call{
		method: http.MethodPut, target: bucketPath, region: regionUSEast1,
		header: map[string]string{headerLockEnabled: valueTrue},
	})
	require.Equal(t, http.StatusOK, got.status, string(got.body))
}

func retentionBody(mode, until string) []byte {
	var b strings.Builder

	b.WriteString(`<Retention xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)

	if mode != "" {
		b.WriteString("<Mode>" + mode + "</Mode>")
	}

	if until != "" {
		b.WriteString("<RetainUntilDate>" + until + "</RetainUntilDate>")
	}

	b.WriteString("</Retention>")

	return []byte(b.String())
}

func legalHoldBody(status string) []byte {
	return []byte(`<LegalHold xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>` + status + `</Status></LegalHold>`)
}

func lockConfigBody(inner string) []byte {
	return []byte(`<ObjectLockConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` + inner +
		`</ObjectLockConfiguration>`)
}

func lockedPut(t *testing.T, s *server, mode string) string {
	t.Helper()

	got := s.do(call{
		method: http.MethodPut, target: objectPath, region: regionUSEast1, body: []byte("locked"),
		header: map[string]string{headerLockMode: mode, headerLockUntil: farFuture},
	})
	require.Equal(t, http.StatusOK, got.status, string(got.body))

	return got.header.Get(headerVersionID)
}

func deleteVersion(s *server, version string, header map[string]string) reply {
	return s.do(call{
		method: http.MethodDelete, target: objectPath + "?versionId=" + version, region: regionUSEast1, header: header,
	})
}

func TestObjectLockBucketConfiguration(t *testing.T) {
	s := newServer(t)

	plain := s.do(call{method: http.MethodPut, target: "/plain", region: regionUSEast1})
	require.Equal(t, http.StatusOK, plain.status)

	missing := s.do(call{method: http.MethodGet, target: "/plain?object-lock", region: regionUSEast1})
	require.Equal(t, http.StatusNotFound, missing.status)
	assert.Equal(t, "ObjectLockConfigurationNotFoundError", missing.err(t).Code)

	refused := s.do(call{
		method: http.MethodPut, target: "/plain?object-lock", region: regionUSEast1,
		body: lockConfigBody(`<ObjectLockEnabled>Enabled</ObjectLockEnabled>`),
	})
	require.Equal(t, http.StatusConflict, refused.status, "versioning must be Enabled first")
	assert.Equal(t, "InvalidBucketState", refused.err(t).Code)

	createLockedBucket(t, s)

	versioning := s.do(call{method: http.MethodGet, target: versionsPath, region: regionUSEast1})
	assert.Contains(t, string(versioning.body), "<Status>Enabled</Status>", "Object Lock turns versioning on")

	suspend := s.do(call{
		method: http.MethodPut, target: versionsPath, region: regionUSEast1, body: versioningBody("Suspended"),
	})
	require.Equal(t, http.StatusConflict, suspend.status)
	assert.Equal(t, "InvalidBucketState", suspend.err(t).Code)

	enabled := s.do(call{method: http.MethodGet, target: lockPath, region: regionUSEast1})
	require.Equal(t, http.StatusOK, enabled.status)
	assert.Contains(t, string(enabled.body), "<ObjectLockEnabled>Enabled</ObjectLockEnabled>")
	assert.NotContains(t, string(enabled.body), "<Rule>")

	put := s.do(call{
		method: http.MethodPut, target: lockPath, region: regionUSEast1,
		body: lockConfigBody(`<ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention>` +
			`<Mode>GOVERNANCE</Mode><Days>1</Days></DefaultRetention></Rule>`),
	})
	require.Equal(t, http.StatusOK, put.status, string(put.body))

	read := s.do(call{method: http.MethodGet, target: lockPath, region: regionUSEast1})
	assert.Contains(t, string(read.body), "<Mode>GOVERNANCE</Mode>")
	assert.Contains(t, string(read.body), "<Days>1</Days>")

	for name, inner := range map[string]string{
		"no enabled": `<Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>1</Days></DefaultRetention></Rule>`,
		"empty rule": `<ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule/>`,
		"no period": `<ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention>` +
			`<Mode>GOVERNANCE</Mode></DefaultRetention></Rule>`,
		"both periods": `<ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention>` +
			`<Mode>GOVERNANCE</Mode><Days>1</Days><Years>1</Years></DefaultRetention></Rule>`,
		"bad default mode": `<ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention>` +
			`<Mode>BAD</Mode><Days>1</Days></DefaultRetention></Rule>`,
	} {
		got := s.do(call{method: http.MethodPut, target: lockPath, region: regionUSEast1, body: lockConfigBody(inner)})
		require.Equal(t, http.StatusBadRequest, got.status, name)
		assert.Equal(t, codeMalformedXML, got.err(t).Code, name)
	}

	cleared := s.do(call{
		method: http.MethodPut, target: lockPath, region: regionUSEast1,
		body: lockConfigBody(`<ObjectLockEnabled>Enabled</ObjectLockEnabled>`),
	})
	require.Equal(t, http.StatusOK, cleared.status)

	read = s.do(call{method: http.MethodGet, target: lockPath, region: regionUSEast1})
	assert.NotContains(t, string(read.body), "<Rule>", "a configuration with no rule clears the default retention")
}

func TestObjectLockCanBeTurnedOnForAVersionedBucket(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)
	setVersioning(t, s, "Enabled")

	got := s.do(call{
		method: http.MethodPut, target: lockPath, region: regionUSEast1,
		body: lockConfigBody(`<ObjectLockEnabled>Enabled</ObjectLockEnabled>`),
	})
	require.Equal(t, http.StatusOK, got.status, string(got.body))

	read := s.do(call{method: http.MethodGet, target: lockPath, region: regionUSEast1})
	assert.Contains(t, string(read.body), "<ObjectLockEnabled>Enabled</ObjectLockEnabled>")
}

func TestRetentionKeepsAVersionFromBeingDeleted(t *testing.T) {
	s := newServer(t)
	createLockedBucket(t, s)

	version := lockedPut(t, s, modeGovernance)

	head := get(s, http.MethodHead, objectPath, nil)
	assert.Equal(t, modeGovernance, head.header.Get(headerLockMode))
	assert.Contains(t, head.header.Get(headerLockUntil), "2099-01-01T00:00:00")

	refused := deleteVersion(s, version, nil)
	require.Equal(t, http.StatusForbidden, refused.status)
	assert.Equal(t, codeAccessDenied, refused.err(t).Code)

	multi := s.do(call{
		method: http.MethodPost, target: "/bucket?delete", region: regionUSEast1,
		body: []byte(`<Delete><Object><Key>key</Key><VersionId>` + version + `</VersionId></Object></Delete>`),
	})
	require.Equal(t, http.StatusOK, multi.status)
	assert.Contains(t, string(multi.body), "<Code>"+codeAccessDenied+"</Code>")

	marker := s.do(call{method: http.MethodDelete, target: objectPath, region: regionUSEast1})
	assert.Equal(t, http.StatusNoContent, marker.status, "a delete marker needs no bypass: the version stays")
	assert.Equal(t, valueTrue, marker.header.Get(headerDeleteMarker))

	kept := get(s, http.MethodGet, objectPath+"?versionId="+version, nil)
	assert.Equal(t, "locked", string(kept.body))

	bypassed := deleteVersion(s, version, map[string]string{headerBypass: valueTrue})
	assert.Equal(t, http.StatusNoContent, bypassed.status)

	compliance := lockedPut(t, s, modeCompliance)

	refused = deleteVersion(s, compliance, map[string]string{headerBypass: valueTrue})
	require.Equal(t, http.StatusForbidden, refused.status, "COMPLIANCE cannot be bypassed")

	unlocked := s.do(call{method: http.MethodPut, target: "/other", region: regionUSEast1})
	require.Equal(t, http.StatusOK, unlocked.status)

	bypassNoLock := s.do(call{
		method: http.MethodDelete, target: "/other/key", region: regionUSEast1,
		header: map[string]string{headerBypass: valueTrue},
	})
	require.Equal(t, http.StatusBadRequest, bypassNoLock.status)
	assert.Equal(t, "InvalidArgument", bypassNoLock.err(t).Code)
}

func TestPutObjectRetention(t *testing.T) {
	s := newServer(t)
	createLockedBucket(t, s)

	version := lockedPut(t, s, modeGovernance)
	target := objectPath + retentionQuery + versionIDQuery + version

	put := func(body []byte, header map[string]string) reply {
		return s.do(call{method: http.MethodPut, target: target, region: regionUSEast1, body: body, header: header})
	}

	read := s.do(call{method: http.MethodGet, target: target, region: regionUSEast1})
	require.Equal(t, http.StatusOK, read.status, string(read.body))
	assert.Contains(t, string(read.body), "<Mode>GOVERNANCE</Mode>")
	assert.Contains(t, string(read.body), "<RetainUntilDate>2099-01-01T00:00:00")

	shortened := put(retentionBody(modeGovernance, sooner), nil)
	require.Equal(t, http.StatusForbidden, shortened.status)
	assert.Equal(t, codeAccessDenied, shortened.err(t).Code)

	bypassed := put(retentionBody(modeGovernance, sooner), map[string]string{headerBypass: valueTrue})
	assert.Equal(t, http.StatusOK, bypassed.status, "bypass lifts a GOVERNANCE retention")

	extended := put(retentionBody(modeGovernance, laterFuture), nil)
	assert.Equal(t, http.StatusOK, extended.status, "extending a retention needs no bypass")

	compliance := put(retentionBody(modeCompliance, laterFuture), nil)
	assert.Equal(t, http.StatusOK, compliance.status, "GOVERNANCE can become COMPLIANCE")

	weaker := put(retentionBody(modeGovernance, laterFuture), map[string]string{headerBypass: valueTrue})
	assert.Equal(t, http.StatusForbidden, weaker.status, "COMPLIANCE cannot become GOVERNANCE, even with a bypass")

	past := put(retentionBody(modeCompliance, "2020-01-01T00:00:00Z"), nil)
	require.Equal(t, http.StatusBadRequest, past.status)
	assert.Contains(t, string(past.body), "The retain until date must be in the future!")
	assert.Contains(t, string(past.body), "Tue Dec 31 16:00:00 PST 2019")

	for name, body := range map[string][]byte{
		"no date":            retentionBody(modeGovernance, ""),
		"no mode":            retentionBody("", farFuture),
		"bad retention mode": retentionBody(modeBad, farFuture),
	} {
		got := put(body, nil)
		require.Equal(t, http.StatusBadRequest, got.status, name)
		assert.Equal(t, codeMalformedXML, got.err(t).Code, name)
	}

	missing := s.do(call{
		method: http.MethodPut, target: "/bucket/absent" + retentionQuery, region: regionUSEast1,
		body: retentionBody(modeGovernance, farFuture),
	})
	require.Equal(t, http.StatusNotFound, missing.status)
	assert.Equal(t, codeNoSuchKey, missing.err(t).Code)
}

func TestClearingARetention(t *testing.T) {
	s := newServer(t)
	createLockedBucket(t, s)

	version := lockedPut(t, s, modeGovernance)
	target := objectPath + retentionQuery + versionIDQuery + version

	refused := s.do(call{method: http.MethodPut, target: target, region: regionUSEast1, body: retentionBody("", "")})
	require.Equal(t, http.StatusForbidden, refused.status, "an empty retention removes it, which needs a bypass")

	cleared := s.do(call{
		method: http.MethodPut, target: target, region: regionUSEast1, body: retentionBody("", ""),
		header: map[string]string{headerBypass: valueTrue},
	})
	require.Equal(t, http.StatusOK, cleared.status, string(cleared.body))

	read := s.do(call{method: http.MethodGet, target: target, region: regionUSEast1})
	require.Equal(t, http.StatusNotFound, read.status)
	assert.Equal(t, "NoSuchObjectLockConfiguration", read.err(t).Code)

	assert.Equal(t, http.StatusNoContent, deleteVersion(s, version, nil).status)
}

// TestRetentionChangeAfterTheDateHasPassed: each row is a request SQ-143 sent to
// LocalStack 8b9a79f against an object whose retain-until date had passed. It
// never looks at the clock, so a weaker retention is refused as before.
func TestRetentionChangeAfterTheDateHasPassed(t *testing.T) {
	const passed = "2020-01-01T00:00:00Z"

	deny, allow := http.StatusForbidden, http.StatusOK

	s := newServer(t)
	createLockedBucket(t, s)

	for _, row := range []struct {
		name   string
		mode   string
		body   []byte
		bypass bool
		status int
	}{
		{"COMPLIANCE replaced by GOVERNANCE", modeCompliance, retentionBody(modeGovernance, farFuture), false, deny},
		{"COMPLIANCE cleared", modeCompliance, retentionBody("", ""), false, deny},
		{"COMPLIANCE cleared with a bypass", modeCompliance, retentionBody("", ""), true, deny},
		{"COMPLIANCE extended", modeCompliance, retentionBody(modeCompliance, farFuture), false, allow},
		{"GOVERNANCE cleared", modeGovernance, retentionBody("", ""), false, deny},
		{"GOVERNANCE cleared with a bypass", modeGovernance, retentionBody("", ""), true, allow},
		{"GOVERNANCE extended", modeGovernance, retentionBody(modeGovernance, farFuture), false, allow},
		{"GOVERNANCE replaced by COMPLIANCE", modeGovernance, retentionBody(modeCompliance, farFuture), false, allow},
	} {
		key := "/bucket/" + strings.ReplaceAll(row.name, " ", "-")

		held := s.do(call{
			method: http.MethodPut, target: key, region: regionUSEast1,
			header: map[string]string{headerLockMode: row.mode, headerLockUntil: passed},
		})
		require.Equal(t, http.StatusOK, held.status, row.name+": "+string(held.body))

		header := map[string]string{}
		if row.bypass {
			header[headerBypass] = valueTrue
		}

		got := s.do(call{
			method: http.MethodPut, target: key + retentionQuery, region: regionUSEast1, body: row.body, header: header,
		})
		require.Equal(t, row.status, got.status, row.name+": "+string(got.body))
	}
}

// TestDefaultRetentionOfZeroOrLess: LocalStack takes a default retention of any
// number of days or years. A negative one puts the date of a new object in the
// past. A zero one fails the PutObject with a 500 there, where gostack ends the
// retention at once.
func TestDefaultRetentionOfZeroOrLess(t *testing.T) {
	s := newServer(t)
	createLockedBucket(t, s)

	for _, row := range []struct{ name, mode, period string }{
		{"days -1", modeGovernance, "<Days>-1</Days>"},
		{"days 0", modeGovernance, "<Days>0</Days>"},
		{"years -1", modeCompliance, "<Years>-1</Years>"},
		{"years 0", modeCompliance, "<Years>0</Years>"},
	} {
		config := s.do(call{
			method: http.MethodPut, target: lockPath, region: regionUSEast1,
			body: lockConfigBody(`<ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention>` +
				`<Mode>` + row.mode + `</Mode>` + row.period + `</DefaultRetention></Rule>`),
		})
		require.Equal(t, http.StatusOK, config.status, row.name+": "+string(config.body))

		read := s.do(call{method: http.MethodGet, target: lockPath, region: regionUSEast1})
		assert.Contains(t, string(read.body), row.period, row.name)

		key := "/bucket/" + strings.ReplaceAll(row.name, " ", "-")
		put(t, s, key, "body", nil)

		head := get(s, http.MethodHead, key, nil)
		assert.Equal(t, row.mode, head.header.Get(headerLockMode), row.name)

		until, err := time.Parse(time.RFC3339, head.header.Get(headerLockUntil))
		require.NoError(t, err, row.name)
		assert.False(t, until.After(time.Now()), row.name+": the retention has ended")
	}
}

func TestObjectLockOperationsNeedAnObjectLockBucket(t *testing.T) {
	s := newServer(t)
	seed(t, s, nil)

	for _, target := range []string{objectPath + retentionQuery, objectPath + legalHoldQuery} {
		got := s.do(call{method: http.MethodGet, target: target, region: regionUSEast1})
		require.Equal(t, http.StatusBadRequest, got.status, target)
		assert.Equal(t, codeInvalidRequest, got.err(t).Code, target)
	}

	put := s.do(call{
		method: http.MethodPut, target: objectPath + retentionQuery, region: regionUSEast1,
		body: retentionBody(modeGovernance, farFuture),
	})
	require.Equal(t, http.StatusBadRequest, put.status)
	assert.Equal(t, codeInvalidRequest, put.err(t).Code)

	empty := s.do(call{method: http.MethodPut, target: objectPath + legalHoldQuery, region: regionUSEast1})
	require.Equal(t, http.StatusBadRequest, empty.status)
	assert.Equal(t, codeMalformedXML, empty.err(t).Code, "a missing legal hold is malformed before the bucket is checked")
}

// TestLockHeadersOnABucketWithoutObjectLock: each row is a request SQ-143 sent
// to LocalStack 8b9a79f, which keeps the lock a request names whether or not
// the bucket has Object Lock. AWS's answer is only inferred (SQ-138).
func TestLockHeadersOnABucketWithoutObjectLock(t *testing.T) {
	s := newServer(t)
	seed(t, s, nil)

	lock := map[string]string{headerLockMode: modeGovernance, headerLockUntil: farFuture}

	put(t, s, "/bucket/put-locked", "a", lock)
	put(t, s, "/bucket/put-held", "a", map[string]string{headerLockHold: "ON"})

	copied := s.do(call{
		method: http.MethodPut, target: copyLockedPath, region: regionUSEast1,
		header: map[string]string{headerCopySource: sourceRef, headerLockMode: modeCompliance, headerLockUntil: farFuture},
	})
	require.Equal(t, http.StatusOK, copied.status, string(copied.body))

	id := startUpload(t, s, multipartKey, lock)
	etag := putPart(t, s, id, 1, []byte("part"), nil)
	require.Equal(t, http.StatusOK, complete(s, id, completeBody([]string{etag}), nil).status)

	for _, row := range []struct{ target, mode, hold string }{
		{"/bucket/put-locked", modeGovernance, ""},
		{"/bucket/put-held", "", "ON"},
		{copyLockedPath, modeCompliance, ""},
		{multipartKey, modeGovernance, ""},
	} {
		head := get(s, http.MethodHead, row.target, nil)
		require.Equal(t, http.StatusOK, head.status, row.target)
		assert.Equal(t, row.mode, head.header.Get(headerLockMode), row.target)
		assert.Equal(t, row.hold, head.header.Get(headerLockHold), row.target)

		if row.mode != "" {
			assert.Contains(t, head.header.Get(headerLockUntil), "2099-01-01T00:00:00", row.target)
		}
	}

	// The checks that do not depend on the bucket still run.
	for name, header := range map[string]map[string]string{
		"mode only":    {headerLockMode: modeGovernance},
		"date only":    {headerLockUntil: farFuture},
		"bad put mode": {headerLockMode: modeBad, headerLockUntil: farFuture},
	} {
		got := s.do(call{method: http.MethodPut, target: "/bucket/refused", region: regionUSEast1, header: header})
		require.Equal(t, http.StatusBadRequest, got.status, name)
		assert.Equal(t, cfgInvalidArgument, got.err(t).Code, name)
	}

	// Retention and legal hold operations still need Object Lock on the bucket.
	retention := get(s, http.MethodGet, "/bucket/put-locked"+retentionQuery, nil)
	require.Equal(t, http.StatusBadRequest, retention.status)
	assert.Equal(t, codeInvalidRequest, retention.err(t).Code)
}

func TestLegalHold(t *testing.T) {
	s := newServer(t)
	createLockedBucket(t, s)

	version := putVersion(t, s, "held")
	target := objectPath + legalHoldQuery + versionIDQuery + version

	unset := s.do(call{method: http.MethodGet, target: target, region: regionUSEast1})
	require.Equal(t, http.StatusNotFound, unset.status)
	assert.Equal(t, "NoSuchObjectLockConfiguration", unset.err(t).Code)

	invalid := s.do(call{method: http.MethodPut, target: target, region: regionUSEast1, body: legalHoldBody("MAYBE")})
	require.Equal(t, http.StatusBadRequest, invalid.status)
	assert.Equal(t, codeMalformedXML, invalid.err(t).Code)

	on := s.do(call{method: http.MethodPut, target: target, region: regionUSEast1, body: legalHoldBody("ON")})
	require.Equal(t, http.StatusOK, on.status, string(on.body))

	read := s.do(call{method: http.MethodGet, target: target, region: regionUSEast1})
	assert.Contains(t, string(read.body), "<Status>ON</Status>")

	head := get(s, http.MethodHead, objectPath, nil)
	assert.Equal(t, "ON", head.header.Get(headerLockHold))

	refused := deleteVersion(s, version, map[string]string{headerBypass: valueTrue})
	require.Equal(t, http.StatusForbidden, refused.status, "no bypass lifts a legal hold")
	assert.Equal(t, codeAccessDenied, refused.err(t).Code)

	off := s.do(call{method: http.MethodPut, target: target, region: regionUSEast1, body: legalHoldBody("OFF")})
	require.Equal(t, http.StatusOK, off.status)

	assert.Equal(t, http.StatusNoContent, deleteVersion(s, version, nil).status)
}

func TestObjectLockRetentionOfADeleteMarkerIsNotAllowed(t *testing.T) {
	s := newServer(t)
	createLockedBucket(t, s)

	putVersion(t, s, "body")

	marker := s.do(call{method: http.MethodDelete, target: objectPath, region: regionUSEast1}).header.Get(headerVersionID)

	got := s.do(call{
		method: http.MethodPut, target: objectPath + retentionQuery + versionIDQuery + marker, region: regionUSEast1,
		body: retentionBody(modeGovernance, farFuture),
	})
	require.Equal(t, http.StatusMethodNotAllowed, got.status)
	assert.Equal(t, "MethodNotAllowed", got.err(t).Code)

	head := get(s, http.MethodHead, objectPath+"?versionId="+marker, nil)
	assert.Equal(t, http.StatusMethodNotAllowed, head.status)
}

func TestLockParametersAreCheckedAndDefaulted(t *testing.T) {
	s := newServer(t)
	createLockedBucket(t, s)

	for name, header := range map[string]map[string]string{
		"mode only":    {headerLockMode: modeGovernance},
		"date only":    {headerLockUntil: farFuture},
		"bad put mode": {headerLockMode: modeBad, headerLockUntil: farFuture},
	} {
		got := s.do(call{method: http.MethodPut, target: objectPath, region: regionUSEast1, header: header})
		require.Equal(t, http.StatusBadRequest, got.status, name)
		assert.Equal(t, "InvalidArgument", got.err(t).Code, name)
	}

	upload := s.do(call{
		method: http.MethodPost, target: multipartKey + "?uploads", region: regionUSEast1,
		header: map[string]string{headerLockMode: modeBad, headerLockUntil: farFuture},
	})
	assert.Equal(t, http.StatusBadRequest, upload.status, "an upload checks its lock when it starts")

	config := s.do(call{
		method: http.MethodPut, target: lockPath, region: regionUSEast1,
		body: lockConfigBody(`<ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention>` +
			`<Mode>COMPLIANCE</Mode><Years>1</Years></DefaultRetention></Rule>`),
	})
	require.Equal(t, http.StatusOK, config.status, string(config.body))

	putVersion(t, s, "defaulted")

	head := get(s, http.MethodHead, objectPath, nil)
	assert.Equal(t, modeCompliance, head.header.Get(headerLockMode), "the bucket's default retention applies")
	assert.NotEmpty(t, head.header.Get(headerLockUntil))

	override := lockedPut(t, s, modeGovernance)
	assert.NotEmpty(t, override)

	head = get(s, http.MethodHead, objectPath, nil)
	assert.Equal(t, modeGovernance, head.header.Get(headerLockMode), "a lock the request names wins")
}

func TestCopyAndMultipartTakeTheirOwnLock(t *testing.T) {
	s := newServer(t)
	createLockedBucket(t, s)
	lockedPut(t, s, modeGovernance)

	copied := s.do(call{
		method: http.MethodPut, target: destPath, region: regionUSEast1,
		header: map[string]string{headerCopySource: objectPath},
	})
	require.Equal(t, http.StatusOK, copied.status, string(copied.body))
	assert.NotEmpty(t, copied.header.Get(headerVersionID))

	head := get(s, http.MethodHead, destPath, nil)
	assert.Empty(t, head.header.Get(headerLockMode), "a copy keeps no lock from its source")

	locked := s.do(call{
		method: http.MethodPut, target: copyLockedPath, region: regionUSEast1,
		header: map[string]string{
			headerCopySource: objectPath, headerLockMode: modeCompliance, headerLockUntil: farFuture,
			headerLockHold: "ON",
		},
	})
	require.Equal(t, http.StatusOK, locked.status, string(locked.body))

	head = get(s, http.MethodHead, copyLockedPath, nil)
	assert.Equal(t, modeCompliance, head.header.Get(headerLockMode))
	assert.Equal(t, "ON", head.header.Get(headerLockHold))

	id := startUpload(t, s, multipartKey, map[string]string{headerLockMode: modeGovernance, headerLockUntil: farFuture})
	etag := putPart(t, s, id, 1, []byte("part"), nil)

	done := complete(s, id, completeBody([]string{etag}), nil)
	require.Equal(t, http.StatusOK, done.status, string(done.body))
	assert.NotEmpty(t, done.header.Get(headerVersionID))

	head = get(s, http.MethodHead, multipartKey, nil)
	assert.Equal(t, modeGovernance, head.header.Get(headerLockMode), "the upload's lock lands on the object it completes")
}

func TestCopyingADeleteMarkerByVersionIsRefused(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)
	setVersioning(t, s, "Enabled")

	version := putVersion(t, s, "body")
	marker := s.do(call{method: http.MethodDelete, target: objectPath, region: regionUSEast1}).header.Get(headerVersionID)

	named := s.do(call{
		method: http.MethodPut, target: destPath, region: regionUSEast1,
		header: map[string]string{headerCopySource: objectPath + "?versionId=" + marker},
	})
	require.Equal(t, http.StatusBadRequest, named.status)
	assert.Equal(t, codeInvalidRequest, named.err(t).Code)

	current := s.do(call{
		method: http.MethodPut, target: destPath, region: regionUSEast1,
		header: map[string]string{headerCopySource: objectPath},
	})
	assert.Equal(t, http.StatusNotFound, current.status, "a source whose current version is a delete marker is missing")

	ok := s.do(call{
		method: http.MethodPut, target: destPath, region: regionUSEast1,
		header: map[string]string{headerCopySource: objectPath + "?versionId=" + version},
	})
	require.Equal(t, http.StatusOK, ok.status, string(ok.body))
	assert.Equal(t, version, ok.header.Get("X-Amz-Copy-Source-Version-Id"))
	assert.NotEmpty(t, ok.header.Get(headerVersionID))
}
