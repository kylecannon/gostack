package s3

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/blob"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fillDepth = 8

// fill sets every exported field reachable from v to a value that is not zero:
// strings that are not valid UTF-8, so the snapshot has to keep their bytes,
// one element in each slice and map, and a time with a zone and nanoseconds.
func fill(v reflect.Value, depth int) {
	if depth > fillDepth {
		return
	}

	switch v.Kind() {
	case reflect.String:
		v.SetString("v\xff\x00" + v.Type().Name())
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(7)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1.5)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem(), depth+1)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fill(v.Index(0), depth+1)
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))

		key := reflect.New(v.Type().Key()).Elem()
		fill(key, depth+1)

		value := reflect.New(v.Type().Elem()).Elem()
		fill(value, depth+1)
		v.SetMapIndex(key, value)
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[time.Time]() {
			v.Set(reflect.ValueOf(time.Date(2026, 11, 18, 10, 11, 12, 13, time.FixedZone("x", 3600))))

			return
		}

		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i), depth+1)
			}
		}
	default:
	}
}

// filledBucket is a bucket with every field set, an object with a version and
// a delete marker, and an upload with a part.
func filledBucket(account, name string) *Bucket {
	b := &Bucket{}
	fill(reflect.ValueOf(b).Elem(), 0)

	b.Name, b.Account = name, account

	version := &Version{}
	fill(reflect.ValueOf(version).Elem(), 0)

	marker := &Version{ID: "null", DeleteMarker: true, LastModified: version.LastModified}

	upload := &MultipartUpload{}
	fill(reflect.ValueOf(upload).Elem(), 0)

	part := &Part{}
	fill(reflect.ValueOf(part).Elem(), 0)
	part.Number = 3
	upload.Parts = map[int32]*Part{3: part}

	b.Objects = map[string]*Object{"key\xfe": {Versions: []*Version{version, marker}}}
	b.Uploads = map[string]*MultipartUpload{upload.ID: upload}

	return b
}

func newSnapshotProvider(t testing.TB) *Provider {
	t.Helper()

	p := New(func() Settings { return Settings{} }, blob.NewEphemeral(t.TempDir()))
	t.Cleanup(func() { p.Close(context.WithoutCancel(t.Context())) })

	return p
}

func snapshotOf(t *testing.T, p *Provider) []byte {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, p.Snapshot(&buf))

	return buf.Bytes()
}

// Test_Snapshot_RoundTripsEveryField restores a snapshot of buckets whose every
// field is set, in two accounts, and snapshots the restored state again: the
// two snapshots are the same bytes, so no field is lost on the way.
func Test_Snapshot_RoundTripsEveryField(t *testing.T) {
	first := newSnapshotProvider(t)
	_, claimed := first.addBucket(filledBucket("111111111111", "alpha"))
	require.True(t, claimed)
	_, claimed = first.addBucket(filledBucket("222222222222", "beta"))
	require.True(t, claimed)

	saved := snapshotOf(t, first)

	second := newSnapshotProvider(t)
	require.NoError(t, second.Restore(bytes.NewReader(saved)))

	assert.Equal(t, string(saved), string(snapshotOf(t, second)))

	restored := second.state("111111111111").get("alpha")
	require.NotNil(t, restored)
	assert.Equal(t, first.state("111111111111").get("alpha").Objects["key\xfe"].Versions[0].Metadata,
		restored.Objects["key\xfe"].Versions[0].Metadata, "bytes that are not UTF-8 come back as they were")

	owner, ok := second.names.owner("beta")
	assert.True(t, ok)
	assert.Equal(t, "222222222222", owner, "the name index is rebuilt")
	assert.Nil(t, second.state("111111111111").get("beta"), "a bucket stays in its own account")
}

// Test_bucketData_HasEveryBucketField fails when Bucket gains a field the
// snapshot does not carry.
func Test_bucketData_HasEveryBucketField(t *testing.T) {
	data := reflect.TypeFor[bucketData]()

	bucket := reflect.TypeFor[Bucket]()

	for field := range bucket.Fields() {
		if !field.IsExported() {
			continue
		}

		saved, ok := data.FieldByName(field.Name)
		if !assert.True(t, ok, "bucketData has no %s", field.Name) {
			continue
		}

		if field.Name != "Objects" && field.Name != "Uploads" {
			assert.Equal(t, field.Type, saved.Type, field.Name)
		}
	}
}

// Test_Restore_RefusesABrokenSnapshot leaves S3 empty, and removes no body,
// for a snapshot that does not load.
func Test_Restore_RefusesABrokenSnapshot(t *testing.T) {
	valid := `{"v":1,"data":{"buckets":[{"Name":"a","Account":"1","Region":"us-east-1"}]}}`

	for name, snapshot := range map[string]string{
		"garbled":       `{"v":1,`,
		"version":       `{"v":2,"data":{"buckets":[]}}`,
		"no list":       `{"v":1,"data":{}}`,
		"trailing data": valid + `{}`,
		"bucket twice":  strings.Replace(valid, `}]}}`, `},{"Name":"a","Account":"2","Region":"us-east-1"}]}}`, 1),
		"no account":    `{"v":1,"data":{"buckets":[{"Name":"a","Region":"us-east-1"}]}}`,
		"no versions":   strings.Replace(valid, `"Region"`, `"Objects":[{"Key":"k","Versions":[]}],"Region"`, 1),
		"part misfiled": strings.Replace(valid, `"Region"`,
			`"Uploads":[{"ID":"u","Parts":{"1":{"Number":2,"BlobID":"x"}}}],"Region"`, 1),
		"raw not base64": strings.Replace(valid, `"Name":"a"`, `"Name":"\u0000b64:%%"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			p := newSnapshotProvider(t)
			_, claimed := p.addBucket(newBucket("old", "1", regionUSEast1, "", nil))
			require.True(t, claimed)

			require.Error(t, p.Restore(strings.NewReader(snapshot)))

			assert.Empty(t, p.state("1").sorted(), "a failed restore leaves S3 empty")
			_, held := p.names.owner("old")
			assert.False(t, held, "and frees the names")
		})
	}
}

// Test_Restore_RebuildsTheKeyIndexes restores a bucket built out of key order:
// its sorted key and upload indexes, which every listing walks, come back in
// listing order, for a bucket that has keys and for one that has none.
func Test_Restore_RebuildsTheKeyIndexes(t *testing.T) {
	first := newSnapshotProvider(t)
	b := newBucket("indexed", "111111111111", regionUSEast1, "", nil)

	for _, key := range []string{"m", "a", "z", "b/c"} {
		b.putObject(key, &Object{Versions: []*Version{{ID: versionNull, Size: 1}}})
	}

	started := time.Now().UTC()
	b.addUpload(&MultipartUpload{ID: "u2", Key: "k", Initiated: stamped(started.Add(time.Second))})
	b.addUpload(&MultipartUpload{ID: "u1", Key: "k", Initiated: stamped(started)})
	b.addUpload(&MultipartUpload{ID: "u0", Key: "a", Initiated: stamped(started.Add(time.Minute))})

	_, claimed := first.addBucket(b)
	require.True(t, claimed)

	_, claimed = first.addBucket(newBucket("empty", "111111111111", regionUSEast1, "", nil))
	require.True(t, claimed)

	second := newSnapshotProvider(t)
	require.NoError(t, second.Restore(bytes.NewReader(snapshotOf(t, first))))

	restored := second.state("111111111111").get("indexed")
	require.NotNil(t, restored)

	restored.mu.RLock()
	defer restored.mu.RUnlock()

	assert.Equal(t, []string{"a", "b/c", "m", "z"}, restored.keys)

	order := make([]string, 0, len(restored.uploadOrder))
	for _, upload := range restored.uploadOrder {
		order = append(order, upload.ID)
	}

	assert.Equal(t, []string{"u0", "u1", "u2"}, order, "by key, then start time")

	empty := second.state("111111111111").get("empty")
	require.NotNil(t, empty)
	assert.Empty(t, empty.keys)
}

// Test_Restore_MovesTheVersionSequencePastRestoredIDs makes a version made
// after a restore sort after every restored one.
func Test_Restore_MovesTheVersionSequencePastRestoredIDs(t *testing.T) {
	// The id a process that made a million versions more would have made.
	start := versionSequence.Load()
	versionSequence.Store(start + 1_000_000)
	id := generateVersionID()
	versionSequence.Store(start)

	observeVersionID(id)
	observeVersionID("null")

	assert.Equal(t, start+1_000_001, versionSequence.Load())
	assert.True(t, versionOlder(id, generateVersionID()), "a new id sorts after the restored one")
}
