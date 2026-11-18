package s3

import (
	"cmp"
	"math"
	"slices"
	"strings"
)

// A bucket keeps two sorted indexes beside its maps, so a listing page costs a
// binary search and the page, not a sort of the whole bucket:
//
//   - keys holds every key of Objects in byte order;
//   - uploadOrder holds every upload of Uploads by key, then start time, then id.
//
// Each is updated in the same critical section as its map, through the methods
// below. A caller that fills Objects or Uploads in bulk, as a restore does, calls
// reindex afterwards. Every method needs b.mu held for writing.

// putObject makes object the entry of key, creating the key if it is new.
func (b *Bucket) putObject(key string, object *Object) {
	if _, ok := b.Objects[key]; !ok {
		i, _ := slices.BinarySearch(b.keys, key)
		b.keys = slices.Insert(b.keys, i, key)
	}

	b.Objects[key] = object
}

// dropObject removes key and its versions from the bucket.
func (b *Bucket) dropObject(key string) {
	if _, ok := b.Objects[key]; !ok {
		return
	}

	delete(b.Objects, key)

	if i, found := slices.BinarySearch(b.keys, key); found {
		b.keys = slices.Delete(b.keys, i, i+1)
	}
}

// clearObjects removes every key.
func (b *Bucket) clearObjects() {
	clear(b.Objects)

	b.keys = nil
}

// addUpload registers a multipart upload in progress.
func (b *Bucket) addUpload(upload *MultipartUpload) {
	if _, ok := b.Uploads[upload.ID]; ok {
		b.removeUpload(upload.ID)
	}

	i, _ := slices.BinarySearchFunc(b.uploadOrder, upload, compareUploads)
	b.uploadOrder = slices.Insert(b.uploadOrder, i, upload)
	b.Uploads[upload.ID] = upload
}

// removeUpload forgets the upload with id.
func (b *Bucket) removeUpload(id string) {
	upload, ok := b.Uploads[id]
	if !ok {
		return
	}

	delete(b.Uploads, id)

	if i, found := slices.BinarySearchFunc(b.uploadOrder, upload, compareUploads); found {
		b.uploadOrder = slices.Delete(b.uploadOrder, i, i+1)
	}
}

// clearUploads forgets every upload.
func (b *Bucket) clearUploads() {
	clear(b.Uploads)

	b.uploadOrder = nil
}

// reindex rebuilds both indexes from the maps.
func (b *Bucket) reindex() {
	b.keys = make([]string, 0, len(b.Objects))
	for key := range b.Objects {
		b.keys = append(b.keys, key)
	}

	slices.Sort(b.keys)

	b.uploadOrder = make([]*MultipartUpload, 0, len(b.Uploads))
	for _, upload := range b.Uploads {
		b.uploadOrder = append(b.uploadOrder, upload)
	}

	slices.SortFunc(b.uploadOrder, compareUploads)
}

// compareUploads is the order ListMultipartUploads lists uploads in: by key,
// then by start time. The id settles a tie, so the order is total.
func compareUploads(a, b *MultipartUpload) int {
	return cmp.Or(strings.Compare(a.Key, b.Key), a.Initiated.Compare(b.Initiated), strings.Compare(a.ID, b.ID))
}

// keySource is the keys a listing walks, in byte order, and what lies at each
// position. The walk reads a source by position, so it can step over a whole
// range of keys with one search.
type keySource interface {
	// len is how many keys there are.
	len() int
	// at is the entry at position i, which is below len.
	at(i int) listed
	// seek is the position of the first key that is not below key, or len.
	seek(key string) int
}

// bucketKeys is a bucket's keys as a keySource. The caller holds b.mu.
type bucketKeys struct{ b *Bucket }

func (s bucketKeys) len() int { return len(s.b.keys) }

func (s bucketKeys) seek(key string) int {
	i, _ := slices.BinarySearch(s.b.keys, key)

	return i
}

// at returns the key with its current version and its whole chain. The chain is
// only valid while the lock is held; the versions in it never change.
func (s bucketKeys) at(i int) listed {
	key := s.b.keys[i]

	object := s.b.Objects[key]
	if object == nil {
		return listed{key: key}
	}

	return listed{key: key, current: object.current(), versions: object.Versions}
}

// listKeys walks the keys of ListObjects and ListObjectsV2 under the bucket's
// read lock, which it holds for this one page and no longer.
func (b *Bucket) listKeys(w walk) listPage {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return walkKeysFrom(bucketKeys{b}, w)
}

// listVersions is listKeys for ListObjectVersions.
func (b *Bucket) listVersions(w walk) versionPage {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return walkVersionsFrom(bucketKeys{b}, w)
}

// prefixEnd is the first position after every key that starts with prefix: the
// smallest string above all of them, found by raising the last byte that can be.
// A prefix of 0xff bytes alone has no such string, and the answer is the end.
func prefixEnd(src keySource, prefix string) int {
	end := []byte(prefix)

	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < math.MaxUint8 {
			end[i]++

			return src.seek(string(end[:i+1]))
		}
	}

	return src.len()
}
