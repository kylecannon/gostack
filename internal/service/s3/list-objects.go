package s3

import (
	"bytes"
	"context"
	"encoding/base64"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/protocol"
)

const (
	// maxKeysLimit is the most keys a listing returns; a larger MaxKeys is
	// served as this.
	maxKeysLimit = 1000
	encodingURL  = "url"

	argContinuationToken = "continuation-token"
	hexDigits            = "0123456789ABCDEF"
)

var (
	// tokenEncoding is how LocalStack encodes a continuation token: standard
	// base64 with "." and "_" for "+" and "/", padded.
	tokenEncoding = base64.NewEncoding(
		"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._")
	// versionIDEncoding is the same alphabet without padding, which a version id
	// is written in.
	versionIDEncoding = tokenEncoding.WithPadding(base64.NoPadding)
)

// listed is one key a listing walks: the key, its current version, and its
// whole chain, oldest first, which is valid only while the bucket lock the walk
// runs under is held. A page keeps the versions, which never change, and not the
// chain.
type listed struct {
	key      string
	current  *Version
	versions []*Version
}

// listPage is what the walk of ListObjects and ListObjectsV2 emitted and where
// it stopped.
type listPage struct {
	// keys are the emitted objects, in key order.
	keys []listed
	// prefixes are the emitted common prefixes, in key order.
	prefixes  []string
	truncated bool
	// last is the key or common prefix the walk emitted last; the NextMarker of
	// a truncated ListObjects.
	last string
	// resume is the first key the walk did not emit; the continuation token of
	// a truncated ListObjectsV2.
	resume string
}

// listRequest is what ListObjects, ListObjectsV2 and ListObjectVersions share
// once their arguments are checked.
type listRequest struct {
	bucket            *Bucket
	name              *string
	prefix, delimiter string
	limit             int
	enc               encoding
}

// listRequestOf finds the bucket and checks the arguments the three listings
// have in common.
func (p *Provider) listRequestOf(
	ctx context.Context, bucket, prefix, delimiter, encodingType *string, maxKeys *int32,
) (listRequest, error) {
	rc, err := callOf(ctx)
	if err != nil {
		return listRequest{}, err
	}

	b, err := p.bucket(rc.AccountID, deref(bucket))
	if err != nil {
		return listRequest{}, err
	}

	enc, err := encodingOf(encodingType)
	if err != nil {
		return listRequest{}, err
	}

	limit, err := maxKeysOf(maxKeys)
	if err != nil {
		return listRequest{}, err
	}

	return listRequest{
		bucket: b, name: bucket, prefix: deref(prefix), delimiter: deref(delimiter), limit: limit, enc: enc,
	}, nil
}

func (r listRequest) walk() walk {
	return walk{prefix: r.prefix, delimiter: r.delimiter, limit: r.limit}
}

// delimiterOut is the Delimiter a listing reports: none when there was none.
func (r listRequest) delimiterOut() *string {
	if r.delimiter == "" {
		return nil
	}

	return new(r.enc.apply(r.delimiter))
}

// ListObjects lists the keys of a bucket in byte order, as LocalStack's
// list_objects does. A key under the delimiter after the prefix folds into one
// common prefix, and a marker inside a common prefix skips all of it.
func (p *Provider) ListObjects(ctx context.Context, in *s3api.ListObjectsInput) (*s3api.ListObjectsOutput, error) {
	r, err := p.listRequestOf(ctx, in.Bucket, in.Prefix, in.Delimiter, in.EncodingType, in.MaxKeys)
	if err != nil {
		return nil, err
	}

	marker := deref(in.Marker)
	w := r.walk()
	w.marker, w.from = marker, marker

	pg := r.bucket.listKeys(w)

	out := &s3api.ListObjectsOutput{
		IsTruncated:    &pg.truncated,
		Name:           r.name,
		MaxKeys:        int32Of(r.limit),
		Prefix:         new(r.enc.apply(r.prefix)),
		Marker:         new(r.enc.apply(marker)),
		Contents:       objectsOf(pg.keys, r.enc, &s3api.Owner{ID: new(ownerID)}),
		CommonPrefixes: commonPrefixesOf(pg.prefixes, r.enc),
		EncodingType:   r.enc.name(),
		Delimiter:      r.delimiterOut(),
		BucketRegion:   bucketRegionHeader(r.bucket),
	}

	// Without a delimiter the client continues from the last key it got.
	if r.delimiter != "" && pg.truncated {
		out.NextMarker = new(r.enc.apply(pg.last))
	}

	return out, nil
}

// ListObjectsV2 lists the keys of a bucket as ListObjects does, paged by a
// continuation token, which is the first key of the next page. StartAfter is
// ignored when a token is given.
func (p *Provider) ListObjectsV2(
	ctx context.Context, in *s3api.ListObjectsV2Input,
) (*s3api.ListObjectsV2Output, error) {
	r, err := p.listRequestOf(ctx, in.Bucket, in.Prefix, in.Delimiter, in.EncodingType, in.MaxKeys)
	if err != nil {
		return nil, err
	}

	w := r.walk()
	w.from = deref(in.StartAfter)

	if in.ContinuationToken != nil {
		// An empty token is as wrong as one that does not decode.
		w.from, err = decodeToken(*in.ContinuationToken)
		if err != nil {
			return nil, err
		}

		w.inclusive = true
	}

	pg := r.bucket.listKeys(w)

	var owner *s3api.Owner
	if in.FetchOwner != nil && *in.FetchOwner {
		owner = &s3api.Owner{ID: new(ownerID)}
	}

	out := &s3api.ListObjectsV2Output{
		IsTruncated:    &pg.truncated,
		Name:           r.name,
		MaxKeys:        int32Of(r.limit),
		Prefix:         new(r.enc.apply(r.prefix)),
		KeyCount:       int32Of(len(pg.keys) + len(pg.prefixes)),
		Contents:       objectsOf(pg.keys, r.enc, owner),
		CommonPrefixes: commonPrefixesOf(pg.prefixes, r.enc),
		EncodingType:   r.enc.name(),
		Delimiter:      r.delimiterOut(),
		BucketRegion:   bucketRegionHeader(r.bucket),
	}

	if pg.truncated {
		out.NextContinuationToken = new(tokenEncoding.EncodeToString([]byte(pg.resume)))
	}

	if in.ContinuationToken != nil {
		out.ContinuationToken = in.ContinuationToken
	} else if startAfter := deref(in.StartAfter); startAfter != "" {
		out.StartAfter = new(r.enc.apply(startAfter))
	}

	return out, nil
}

// ListObjectVersions lists every version and delete marker of a bucket: keys in
// byte order, the newest version of a key first. The page is continued from a
// key and a version id.
func (p *Provider) ListObjectVersions(
	ctx context.Context, in *s3api.ListObjectVersionsInput,
) (*s3api.ListObjectVersionsOutput, error) {
	keyMarker, versionMarker := deref(in.KeyMarker), deref(in.VersionIdMarker)
	if versionMarker != "" && keyMarker == "" {
		return nil, invalidArgument("A version-id marker cannot be specified without a key marker.",
			"version-id-marker", &versionMarker)
	}

	r, err := p.listRequestOf(ctx, in.Bucket, in.Prefix, in.Delimiter, in.EncodingType, in.MaxKeys)
	if err != nil {
		return nil, err
	}

	w := r.walk()
	w.marker, w.versionMarker = keyMarker, versionMarker
	w.from, w.inclusive = keyMarker, true

	pg := r.bucket.listVersions(w)

	out := &s3api.ListObjectVersionsOutput{
		IsTruncated:     &pg.truncated,
		Name:            r.name,
		MaxKeys:         int32Of(r.limit),
		Prefix:          new(r.enc.apply(r.prefix)),
		KeyMarker:       new(r.enc.apply(keyMarker)),
		VersionIdMarker: new(versionMarker),
		CommonPrefixes:  commonPrefixesOf(pg.prefixes, r.enc),
		EncodingType:    r.enc.name(),
		Delimiter:       r.delimiterOut(),
	}

	out.Versions, out.DeleteMarkers = versionsOf(pg.entries, r.enc)

	if pg.truncated {
		out.NextKeyMarker = new(r.enc.apply(pg.last.key))
		out.NextVersionIdMarker = nonEmpty(pg.last.versionID)
	}

	return out, nil
}

// versionsOf splits the emitted entries into versions and delete markers.
func versionsOf(entries []versionEntry, enc encoding) ([]s3api.ObjectVersion, []s3api.DeleteMarkerEntry) {
	var (
		versions []s3api.ObjectVersion
		markers  []s3api.DeleteMarkerEntry
	)

	owner := &s3api.Owner{ID: new(ownerID)}

	for _, e := range entries {
		if e.version.DeleteMarker {
			markers = append(markers, s3api.DeleteMarkerEntry{
				Key:          new(enc.apply(e.key)),
				Owner:        owner,
				VersionId:    new(e.version.ID),
				IsLatest:     new(e.latest),
				LastModified: lastModified(e.version),
			})

			continue
		}

		versions = append(versions, s3api.ObjectVersion{
			Key:               new(enc.apply(e.key)),
			ETag:              new(quoted(e.version.ETag)),
			Owner:             owner,
			Size:              new(e.version.Size),
			VersionId:         new(e.version.ID),
			LastModified:      lastModified(e.version),
			IsLatest:          new(e.latest),
			StorageClass:      new(storageClassDefault),
			ChecksumAlgorithm: checksumAlgorithmsOf(e.version),
			ChecksumType:      nonEmpty(checksumTypeOf(e.version)),
		})
	}

	return versions, markers
}

// walk is how a listing walks its keys.
type walk struct {
	prefix, delimiter string
	limit             int
	// from is the key the walk starts at: after it, or at it when inclusive. A
	// key under prefix is all a walk lists, so it starts at the later of the two.
	from      string
	inclusive bool
	// marker is the ListObjects marker or the ListObjectVersions key marker.
	// A key inside a common prefix that begins with the marker is skipped with
	// the rest of that prefix.
	marker string
	// versionMarker is the ListObjectVersions version id marker.
	versionMarker string
}

// start is the position of the first key the walk may list.
func (w walk) start(src keySource) int {
	return src.seek(max(w.prefix, w.from))
}

// before reports whether key is a key the walk starts after rather than at.
func (w walk) before(key string) bool {
	return !w.inclusive && key == w.from
}

// walkKeysFrom is the walk of ListObjects and ListObjectsV2 over the keys of
// src, in key order, whose current version is not a delete marker. A common
// prefix is emitted once and the walk then steps past all of its keys with one
// search, so a page costs the page and not the bucket.
func walkKeysFrom(src keySource, w walk) listPage {
	var pg listPage

	if w.limit == 0 {
		return pg
	}

	for i := w.start(src); i < src.len(); {
		k := src.at(i)
		if !strings.HasPrefix(k.key, w.prefix) {
			break
		}

		i++

		if k.current == nil || k.current.DeleteMarker || w.before(k.key) {
			continue
		}

		cp := w.commonPrefix(k.key)
		if cp != "" && w.marker != "" && strings.HasPrefix(w.marker, cp) {
			i = prefixEnd(src, cp)

			continue
		}

		if len(pg.keys)+len(pg.prefixes) >= w.limit {
			pg.truncated = true
			pg.resume = k.key

			break
		}

		if cp != "" {
			pg.prefixes = append(pg.prefixes, cp)
			pg.last = cp
			i = prefixEnd(src, cp)

			continue
		}

		// The chain is only valid under the lock; the version it ends in is not.
		pg.keys = append(pg.keys, listed{key: k.key, current: k.current})
		pg.last = k.key
	}

	return pg
}

// versionEntry is one version or delete marker a ListObjectVersions emits.
type versionEntry struct {
	key     string
	version *Version
	latest  bool
}

// versionMark is where a ListObjectVersions stopped: the key or common prefix
// it emitted last, and the version id of it when it is a version.
type versionMark struct {
	key, versionID string
}

type versionPage struct {
	entries   []versionEntry
	prefixes  []string
	truncated bool
	last      versionMark
}

// walkVersionsFrom is the walk of ListObjectVersions over the keys of src:
// every version of a key, the newest first, until the limit. Like walkKeysFrom
// it steps past a common prefix with one search.
func walkVersionsFrom(src keySource, w walk) versionPage {
	var pg versionPage

	if w.limit == 0 {
		return pg
	}

	var markerFound bool

	for i := w.start(src); i < src.len(); {
		k := src.at(i)
		if !strings.HasPrefix(k.key, w.prefix) {
			break
		}

		i++

		if k.current == nil || w.before(k.key) {
			continue
		}

		if cp := w.commonPrefix(k.key); cp != "" {
			if w.marker == "" || !strings.HasPrefix(w.marker, cp) {
				if len(pg.entries)+len(pg.prefixes) >= w.limit {
					pg.truncated = true

					return pg
				}

				pg.prefixes = append(pg.prefixes, cp)
				pg.last = versionMark{key: cp}
			}

			i = prefixEnd(src, cp)

			continue
		}

		for j, v := range slices.Backward(k.versions) {
			if w.marker != "" && k.key == w.marker {
				skip, found := w.skipVersion(v, markerFound)
				markerFound = found

				if skip {
					continue
				}
			}

			if len(pg.entries)+len(pg.prefixes) >= w.limit {
				pg.truncated = true

				return pg
			}

			pg.entries = append(pg.entries, versionEntry{key: k.key, version: v, latest: j == len(k.versions)-1})
			pg.last = versionMark{key: k.key, versionID: v.ID}
		}
	}

	return pg
}

// skipVersion reports whether a version of the key marker is skipped, and
// whether the version id marker has been passed by it. A key listed from a
// version id marker resumes after that version. When the version of the marker
// is gone, the first version older than it resumes the list.
func (w walk) skipVersion(v *Version, markerFound bool) (skip, found bool) {
	switch {
	case w.versionMarker == "":
		return true, markerFound
	case v.ID == w.versionMarker:
		return true, true
	case versionOlder(v.ID, w.versionMarker):
		return false, true
	default:
		return !markerFound, markerFound
	}
}

// commonPrefix is the common prefix a key falls into: the key up to and
// including the first delimiter after the prefix. It is empty when the walk has
// no delimiter or the key has none after the prefix.
func (w walk) commonPrefix(key string) string {
	if w.delimiter == "" {
		return ""
	}

	rest := strings.TrimPrefix(key, w.prefix)

	i := strings.Index(rest, w.delimiter)
	if i < 0 {
		return ""
	}

	return key[:len(w.prefix)+i+len(w.delimiter)]
}

// objectsOf is the Contents of a listing. owner is set on each object, nil for
// none.
func objectsOf(keys []listed, enc encoding, owner *s3api.Owner) []s3api.Object {
	if len(keys) == 0 {
		return nil
	}

	objects := make([]s3api.Object, len(keys))

	for i, k := range keys {
		v := k.current

		objects[i] = s3api.Object{
			Key:               new(enc.apply(k.key)),
			ETag:              new(quoted(v.ETag)),
			Size:              new(v.Size),
			LastModified:      lastModified(v),
			StorageClass:      new(storageClassName(v)),
			ChecksumAlgorithm: checksumAlgorithmsOf(v),
			ChecksumType:      nonEmpty(checksumTypeOf(v)),
			Owner:             owner,
		}
	}

	return objects
}

func commonPrefixesOf(prefixes []string, enc encoding) []s3api.CommonPrefix {
	if len(prefixes) == 0 {
		return nil
	}

	out := make([]s3api.CommonPrefix, len(prefixes))
	for i, prefix := range prefixes {
		out[i] = s3api.CommonPrefix{Prefix: new(enc.apply(prefix))}
	}

	return out
}

// lastModified is the time S3 lists: whole seconds.
func lastModified(v *Version) *protocol.Time {
	return &protocol.Time{Time: v.LastModified.Truncate(time.Second)}
}

func checksumAlgorithmsOf(v *Version) []string {
	if v.ChecksumAlgorithm == "" {
		return nil
	}

	return []string{v.ChecksumAlgorithm}
}

func checksumTypeOf(v *Version) string {
	if v.ChecksumAlgorithm == "" {
		return ""
	}

	return v.ChecksumType
}

func storageClassName(v *Version) string {
	if v.StorageClass == "" {
		return storageClassDefault
	}

	return v.StorageClass
}

// bucketRegionHeader is the x-amz-bucket-region of a listing, which only a
// bucket outside us-east-1 carries.
func bucketRegionHeader(b *Bucket) *string {
	if b.Region == regionUSEast1 {
		return nil
	}

	return &b.Region
}

// int32Of is a count a listing reports. It is at most maxKeysLimit.
func int32Of(n int) *int32 {
	return new(int32(min(n, maxKeysLimit))) //nolint:gosec // G115: capped at maxKeysLimit.
}

// maxKeysOf is the number of keys a listing returns: 1,000 unless MaxKeys says
// fewer.
func maxKeysOf(maxKeys *int32) (int, error) {
	if maxKeys == nil {
		return maxKeysLimit, nil
	}

	if *maxKeys < 0 {
		value := strconv.Itoa(int(*maxKeys))

		return 0, invalidArgument("Argument maxKeys must be an integer between 0 and 2147483647", "maxKeys", &value)
	}

	return min(int(*maxKeys), maxKeysLimit), nil
}

// decodeToken is the key a continuation token names.
func decodeToken(token string) (string, error) {
	raw, err := tokenEncoding.DecodeString(token)
	if err != nil || len(raw) == 0 || !utf8.Valid(raw) {
		return "", invalidArgument("The continuation token provided is incorrect", argContinuationToken, nil)
	}

	return string(raw), nil
}

// versionOlder reports whether version id a was made before b. A version id
// starts with an ever-increasing sequence, so the order of the decoded ids is
// the order they were made in, as LocalStack's is_version_older_than_other
// relies on. An id that does not decode is never older.
func versionOlder(a, b string) bool {
	rawA, errA := versionIDEncoding.DecodeString(strings.TrimRight(a, "="))
	rawB, errB := versionIDEncoding.DecodeString(strings.TrimRight(b, "="))

	return errA == nil && errB == nil && bytes.Compare(rawA, rawB) < 0
}

// encoding is what a listing does to a key, a prefix or a delimiter it reports:
// nothing, or Python's urllib.parse.quote when the client asked for
// EncodingType=url.
type encoding struct {
	url bool
}

// encodingOf validates EncodingType.
func encodingOf(encodingType *string) (encoding, error) {
	if encodingType == nil {
		return encoding{}, nil
	}

	if *encodingType != encodingURL {
		return encoding{}, invalidArgument("Invalid Encoding Method specified in Request", "encoding-type", encodingType)
	}

	return encoding{url: true}, nil
}

func (e encoding) apply(s string) string {
	if e.url {
		return quote(s)
	}

	return s
}

// name is the EncodingType a listing reports.
func (e encoding) name() *string {
	if e.url {
		return new(encodingURL)
	}

	return nil
}

// quote percent-encodes s as urllib.parse.quote does: every byte but the ASCII
// letters, digits, "_.-~" and "/".
func quote(s string) string {
	clean := true

	for i := range len(s) {
		if !unreserved(s[i]) {
			clean = false

			break
		}
	}

	if clean {
		return s
	}

	var out strings.Builder

	out.Grow(len(s) + len(s)/2)

	for i := range len(s) {
		c := s[i]
		if unreserved(c) {
			out.WriteByte(c)

			continue
		}

		out.WriteByte('%')
		out.WriteByte(hexDigits[c>>4])
		out.WriteByte(hexDigits[c&0x0f])
	}

	return out.String()
}

func unreserved(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	default:
		return c == '_' || c == '.' || c == '-' || c == '~' || c == '/'
	}
}
