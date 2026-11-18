package s3

import (
	"cmp"
	"encoding/base64"
	"encoding/binary"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
)

// snapshotVersion is the version of the snapshot envelope: {"v":1,"data":...}.
// Restore refuses any other.
const snapshotVersion = 1

// rawPrefix starts a string the snapshot holds in base64: one that is not
// valid UTF-8, which json would mangle, or one that starts with rawPrefix
// itself. An S3 key or a header value can be any bytes, and two keys that json
// mangled the same way would be one object after a restore.
const rawPrefix = "\x00b64:"

// snapshotOptions are the json settings of a snapshot, on both sides of it. A
// map is written in key order, so the same state gives the same bytes, and
// every string goes through as the bytes it is.
var snapshotOptions = json.JoinOptions(
	json.Deterministic(true),
	json.WithMarshalers(json.JoinMarshalers(
		json.MarshalToFunc(marshalString),
		json.MarshalToFunc(marshalKeys[string]),
		json.MarshalToFunc(marshalKeys[*s3api.InventoryConfiguration]),
		json.MarshalToFunc(marshalKeys[*s3api.AnalyticsConfiguration]),
		json.MarshalToFunc(marshalKeys[*s3api.MetricsConfiguration]),
		json.MarshalToFunc(marshalKeys[*s3api.IntelligentTieringConfiguration]),
	)),
	json.WithUnmarshalers(json.JoinUnmarshalers(
		json.UnmarshalFromFunc(unmarshalString),
		json.UnmarshalFromFunc(unmarshalKeys[string]),
		json.UnmarshalFromFunc(unmarshalKeys[*s3api.InventoryConfiguration]),
		json.UnmarshalFromFunc(unmarshalKeys[*s3api.AnalyticsConfiguration]),
		json.UnmarshalFromFunc(unmarshalKeys[*s3api.MetricsConfiguration]),
		json.UnmarshalFromFunc(unmarshalKeys[*s3api.IntelligentTieringConfiguration]),
	)),
)

// encodeRaw is s as the snapshot holds it.
func encodeRaw(s string) string {
	if utf8.ValidString(s) && !strings.HasPrefix(s, rawPrefix) {
		return s
	}

	return rawPrefix + base64.StdEncoding.EncodeToString([]byte(s))
}

// decodeRaw is the string the snapshot holds as s.
func decodeRaw(s string) (string, error) {
	encoded, raw := strings.CutPrefix(s, rawPrefix)
	if !raw {
		return s, nil
	}

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("a raw string: %w", err)
	}

	return string(decoded), nil
}

func marshalString(enc *jsontext.Encoder, s string) error {
	if encoded := encodeRaw(s); encoded != s {
		return enc.WriteToken(jsontext.String(encoded)) //nolint:wrapcheck // json wraps it
	}

	return errors.ErrUnsupported // json's own encoding
}

func unmarshalString(dec *jsontext.Decoder, s *string) error {
	token, err := dec.ReadToken()
	if err != nil {
		return err //nolint:wrapcheck // json wraps it
	}

	if token.Kind() != '"' {
		return fmt.Errorf("want a string, have %s", token.Kind())
	}

	*s, err = decodeRaw(token.String())

	return err
}

// keyedMap is a map whose keys are encoded with encodeRaw. It is its own type,
// so the json calls in marshalKeys and unmarshalKeys do not come back to them.
type keyedMap[V any] map[string]V

// marshalKeys writes a map with its keys kept as the bytes they are: json
// passes no map key to marshalString. A metadata name or a configuration id
// can be any bytes too.
func marshalKeys[V any](enc *jsontext.Encoder, m map[string]V) error {
	encoded := make(keyedMap[V], len(m))
	for key, value := range m {
		encoded[encodeRaw(key)] = value
	}

	return json.MarshalEncode(enc, encoded) //nolint:wrapcheck // json wraps it
}

func unmarshalKeys[V any](dec *jsontext.Decoder, m *map[string]V) error {
	var encoded keyedMap[V]

	err := json.UnmarshalDecode(dec, &encoded)
	if err != nil {
		return err //nolint:wrapcheck // json wraps it
	}

	decoded := make(map[string]V, len(encoded))

	for key, value := range encoded {
		plain, err := decodeRaw(key)
		if err != nil {
			return err
		}

		decoded[plain] = value
	}

	*m = decoded

	return nil
}

// snapshotFile is the envelope of a snapshot.
type snapshotFile struct {
	Version int          `json:"v"`
	Data    snapshotData `json:"data"`
}

// snapshotData is the S3 state of every account: its buckets, each naming its
// account. The bodies are not in it; their blob ids are.
type snapshotData struct {
	Buckets []bucketData `json:"buckets"`
}

// bucketData is a Bucket: every exported field of it, with the objects and the
// uploads as lists. A field added to Bucket has to be added here, and
// Test_bucketData_HasEveryBucketField says so.
type bucketData struct {
	Name               string
	Account            string
	Region             string
	LocationConstraint string
	CreationDate       time.Time

	Objects []objectData
	Uploads []*MultipartUpload

	Versioning             *s3api.VersioningConfiguration
	ACL                    *s3api.AccessControlPolicy
	Policy                 *string
	PublicAccessBlock      *s3api.PublicAccessBlockConfiguration
	OwnershipControls      *s3api.OwnershipControls
	CORS                   *s3api.CORSConfiguration
	Website                *s3api.WebsiteConfiguration
	Lifecycle              *s3api.BucketLifecycleConfiguration
	Logging                *s3api.BucketLoggingStatus
	Notification           *s3api.GetBucketNotificationConfigurationOutput
	Replication            *s3api.ReplicationConfiguration
	Encryption             *s3api.ServerSideEncryptionConfiguration
	EncryptionConfigured   bool
	Tags                   []s3api.Tag
	ObjectLock             *s3api.ObjectLockConfiguration
	Accelerate             *s3api.AccelerateConfiguration
	RequestPayment         *s3api.RequestPaymentConfiguration
	Inventory              map[string]*s3api.InventoryConfiguration
	Analytics              map[string]*s3api.AnalyticsConfiguration
	Metrics                map[string]*s3api.MetricsConfiguration
	IntelligentTiering     map[string]*s3api.IntelligentTieringConfiguration
	LifecycleMinObjectSize string
}

// objectData is one key and its version chain, oldest first.
type objectData struct {
	Key      string
	Versions []*Version
}

// Snapshot writes the state of every account to w as json. It implements
// persist.Snapshotter.
//
// It copies first and encodes after. The copy holds the transfer lock, so no
// bucket is being created, deleted or restored, and each bucket's lock only
// for its own copy. A Version never changes once committed and a configuration
// is replaced, never changed in place, so the copy shares them; the maps and
// slices a call goes on to change are cloned. Then, with every lock released,
// it sorts the copy and encodes it straight to w.
func (p *Provider) Snapshot(w io.Writer) error {
	file := p.capture()

	slices.SortFunc(file.Data.Buckets, func(a, b bucketData) int {
		return cmp.Or(strings.Compare(a.Account, b.Account), strings.Compare(a.Name, b.Name))
	})

	for i := range file.Data.Buckets {
		file.Data.Buckets[i].sort()
	}

	err := json.MarshalEncode(jsontext.NewEncoder(w, snapshotOptions), &file, snapshotOptions)
	if err != nil {
		return fmt.Errorf("write s3 snapshot: %w", err)
	}

	return nil
}

// capture copies every bucket of every account.
func (p *Provider) capture() snapshotFile {
	file := snapshotFile{Version: snapshotVersion, Data: snapshotData{Buckets: []bucketData{}}}

	p.transfer.Lock()
	defer p.transfer.Unlock()

	p.states.Range(func(_, _ string, s *State) bool {
		for _, b := range s.sorted() {
			if saved, ok := b.save(); ok {
				file.Data.Buckets = append(file.Data.Buckets, saved)
			}
		}

		return true
	})

	return file
}

// save copies the bucket under its lock. It reports false for a bucket that is
// gone.
func (b *Bucket) save() (bucketData, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.gone {
		return bucketData{}, false
	}

	saved := bucketData{
		Name: b.Name, Account: b.Account, Region: b.Region, LocationConstraint: b.LocationConstraint,
		CreationDate: b.CreationDate,

		Objects: make([]objectData, 0, len(b.Objects)),
		Uploads: make([]*MultipartUpload, 0, len(b.Uploads)),

		Versioning: b.Versioning, ACL: b.ACL, Policy: b.Policy, PublicAccessBlock: b.PublicAccessBlock,
		OwnershipControls: b.OwnershipControls, CORS: b.CORS, Website: b.Website, Lifecycle: b.Lifecycle,
		Logging: b.Logging, Notification: b.Notification, Replication: b.Replication, Encryption: b.Encryption,
		EncryptionConfigured: b.EncryptionConfigured, Tags: b.Tags, ObjectLock: b.ObjectLock,
		Accelerate: b.Accelerate, RequestPayment: b.RequestPayment,
		Inventory: maps.Clone(b.Inventory), Analytics: maps.Clone(b.Analytics), Metrics: maps.Clone(b.Metrics),
		IntelligentTiering: maps.Clone(b.IntelligentTiering), LifecycleMinObjectSize: b.LifecycleMinObjectSize,
	}

	for key, object := range b.Objects {
		saved.Objects = append(saved.Objects, objectData{Key: key, Versions: slices.Clone(object.Versions)})
	}

	for _, upload := range b.Uploads {
		copied := *upload
		copied.Parts = maps.Clone(upload.Parts)
		saved.Uploads = append(saved.Uploads, &copied)
	}

	return saved, true
}

// sort puts a copied bucket's objects and uploads in the order the file keeps
// them, outside the bucket's lock.
func (d *bucketData) sort() {
	slices.SortFunc(d.Objects, func(a, b objectData) int { return strings.Compare(a.Key, b.Key) })
	slices.SortFunc(d.Uploads, func(a, b *MultipartUpload) int { return strings.Compare(a.ID, b.ID) })
}

// References returns the blob id of every body the state names: each version's
// and each part's of an upload in progress. It implements persist.Referencer.
// The ids are collected bucket by bucket under each bucket's lock, and yielded
// once every lock is released.
func (p *Provider) References() iter.Seq[string] {
	var ids []string

	p.states.Range(func(_, _ string, s *State) bool {
		for _, b := range s.sorted() {
			b.mu.RLock()

			if !b.gone {
				ids = append(ids, b.blobIDs()...)
			}

			b.mu.RUnlock()
		}

		return true
	})

	return slices.Values(ids)
}

// Restore replaces the state of every account with what r holds. It
// implements persist.Snapshotter. The snapshot is read and checked whole before
// anything is replaced, and one that is not valid leaves S3 empty, as the
// Snapshotter contract says.
//
// It holds the transfer lock, so no bucket is created or deleted meanwhile.
// Each account's buckets go from the old map to the restored one in one step,
// so a call that looks a bucket up finds the old one or the restored one,
// never none. Every bucket replaced is then marked gone, as a reset marks it:
// a write that found it before the swap fails, and drops the body it wrote,
// instead of linking that body into a bucket no state holds. The bodies the
// replaced buckets named that the restored state does not name are removed.
//
// After a snapshot that does not load, nothing is removed: the engine sets the
// file aside, and it may name those bodies.
func (p *Provider) Restore(r io.Reader) error {
	buckets, err := readSnapshot(r)

	p.transfer.Lock()
	defer p.transfer.Unlock()

	replaced := p.install(buckets)

	keep := make(map[string]struct{})
	for _, b := range buckets {
		for _, id := range b.blobIDs() {
			keep[id] = struct{}{}
		}
	}

	for _, b := range replaced {
		for _, id := range b.drop() {
			if _, named := keep[id]; !named && err == nil {
				_ = p.blobs.Remove(id)
			}
		}
	}

	if err != nil {
		return err
	}

	for _, b := range buckets {
		for _, object := range b.Objects {
			for _, v := range object.Versions {
				observeVersionID(v.ID)
			}
		}
	}

	return nil
}

// install puts the buckets in their accounts' States, in place of every bucket
// there was, and gives the name index their names. It returns the buckets it
// replaced. The caller holds the transfer lock.
func (p *Provider) install(buckets []*Bucket) []*Bucket {
	byAccount := make(map[string]map[string]*Bucket)
	owners := make(map[string]string, len(buckets))

	for _, b := range buckets {
		if byAccount[b.Account] == nil {
			byAccount[b.Account] = make(map[string]*Bucket)
			// Range is sure to walk only the States that exist when it starts.
			p.state(b.Account)
		}

		byAccount[b.Account][b.Name] = b
		owners[b.Name] = b.Account
	}

	var replaced []*Bucket

	p.states.Range(func(account, _ string, s *State) bool {
		restored := byAccount[account]
		if restored == nil {
			restored = make(map[string]*Bucket)
		}

		s.mu.Lock()
		replaced = slices.AppendSeq(replaced, maps.Values(s.buckets))
		s.buckets = restored
		s.mu.Unlock()

		return true
	})

	p.names.mu.Lock()
	p.names.owners = owners
	p.names.mu.Unlock()

	return replaced
}

// drop marks a bucket that left its State as gone and empties it, and returns
// the blob ids it named, for the caller to remove the ones no state names.
func (b *Bucket) drop() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.gone = true
	ids := b.blobIDs()
	clear(b.Objects)
	clear(b.Uploads)

	return ids
}

// readSnapshot reads a snapshot into buckets, checked whole.
func readSnapshot(r io.Reader) ([]*Bucket, error) {
	var file snapshotFile

	dec := jsontext.NewDecoder(r, snapshotOptions)

	err := json.UnmarshalDecode(dec, &file, snapshotOptions)
	if err != nil {
		return nil, fmt.Errorf("decode s3 snapshot: %w", err)
	}

	_, err = dec.ReadToken()
	if !errors.Is(err, io.EOF) {
		return nil, errors.New("decode s3 snapshot: data after the snapshot")
	}

	if file.Version != snapshotVersion {
		return nil, fmt.Errorf("s3 snapshot is version %d, this build reads version %d", file.Version, snapshotVersion)
	}

	// Snapshot always writes the list, empty when there is no bucket.
	if file.Data.Buckets == nil {
		return nil, errors.New("s3 snapshot has no list of buckets")
	}

	names := make(map[string]bool, len(file.Data.Buckets))
	buckets := make([]*Bucket, 0, len(file.Data.Buckets))

	for i := range file.Data.Buckets {
		saved := &file.Data.Buckets[i]

		if names[saved.Name] {
			return nil, fmt.Errorf("s3 snapshot has the bucket %s twice", saved.Name)
		}

		names[saved.Name] = true

		b, err := saved.bucket()
		if err != nil {
			return nil, fmt.Errorf("s3 snapshot, bucket %s: %w", saved.Name, err)
		}

		buckets = append(buckets, b)
	}

	return buckets, nil
}

// bucket makes the Bucket d holds, checked.
func (d *bucketData) bucket() (*Bucket, error) {
	if d.Name == "" || d.Account == "" || d.Region == "" {
		return nil, errors.New("no name, account or region")
	}

	b := &Bucket{
		Name: d.Name, Account: d.Account, Region: d.Region, LocationConstraint: d.LocationConstraint,
		CreationDate: d.CreationDate,

		Objects: make(map[string]*Object, len(d.Objects)),
		Uploads: make(map[string]*MultipartUpload, len(d.Uploads)),

		Versioning: d.Versioning, ACL: d.ACL, Policy: d.Policy, PublicAccessBlock: d.PublicAccessBlock,
		OwnershipControls: d.OwnershipControls, CORS: d.CORS, Website: d.Website, Lifecycle: d.Lifecycle,
		Logging: d.Logging, Notification: d.Notification, Replication: d.Replication, Encryption: d.Encryption,
		EncryptionConfigured: d.EncryptionConfigured, Tags: d.Tags, ObjectLock: d.ObjectLock,
		Accelerate: d.Accelerate, RequestPayment: d.RequestPayment,
		Inventory: d.Inventory, Analytics: d.Analytics, Metrics: d.Metrics,
		IntelligentTiering: d.IntelligentTiering, LifecycleMinObjectSize: d.LifecycleMinObjectSize,
	}

	for _, saved := range d.Objects {
		if _, twice := b.Objects[saved.Key]; twice {
			return nil, fmt.Errorf("the key %q twice", saved.Key)
		}

		if len(saved.Versions) == 0 || slices.Contains(saved.Versions, nil) {
			return nil, fmt.Errorf("the key %q has a missing version", saved.Key)
		}

		b.Objects[saved.Key] = &Object{Versions: saved.Versions}
	}

	for _, upload := range d.Uploads {
		err := checkUpload(b, upload)
		if err != nil {
			return nil, err
		}

		b.Uploads[upload.ID] = upload
	}

	return b, nil
}

// checkUpload checks an upload read from a snapshot before it joins b, and
// gives it a part map when it has none.
func checkUpload(b *Bucket, upload *MultipartUpload) error {
	if upload == nil || upload.ID == "" {
		return errors.New("an upload without an id")
	}

	if _, twice := b.Uploads[upload.ID]; twice {
		return fmt.Errorf("the upload %s twice", upload.ID)
	}

	if upload.Parts == nil {
		upload.Parts = make(map[int32]*Part)
	}

	for number, part := range upload.Parts {
		if part == nil || part.Number != number || part.BlobID == "" {
			return fmt.Errorf("the upload %s has a broken part %d", upload.ID, number)
		}
	}

	return nil
}

// observeVersionID moves the version id sequence past the one in id, so a
// version made after a restore sorts after every restored one, however many
// versions the process before made per millisecond. An id that is not one
// generateVersionID makes, such as "null", is ignored.
func observeVersionID(id string) {
	raw, err := versionIDEncoding.DecodeString(id)
	if err != nil || len(raw) != versionIDSequenceBytes+versionIDRandomBytes {
		return
	}

	var sequence [8]byte

	copy(sequence[8-versionIDSequenceBytes:], raw[:versionIDSequenceBytes])
	seen := binary.BigEndian.Uint64(sequence[:])

	for {
		current := versionSequence.Load()
		if seen <= current || versionSequence.CompareAndSwap(current, seen) {
			return
		}
	}
}
