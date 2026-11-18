package s3

import (
	"slices"
	"sync"
	"time"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
)

// versionNull is the version id of an object in a bucket that never had
// versioning, as S3 reports it.
const versionNull = "null"

// State is the S3 state of one account, held in a store.Bundle under the
// account and an empty region: buckets are global to their account and carry
// their own region, as LocalStack's CrossRegionAttribute buckets are. The lock
// covers the map only; a bucket has its own.
type State struct {
	mu      sync.RWMutex
	buckets map[string]*Bucket
}

func newState(string, string) *State {
	return &State{buckets: make(map[string]*Bucket)}
}

// emptyState is the Bundle's emptyFn. The provider's Reset releases the blobs
// and names of the buckets itself and never calls Bundle.Reset; this only keeps
// the Bundle's contract.
func emptyState(s *State) {
	s.mu.Lock()
	clear(s.buckets)
	s.mu.Unlock()
}

func (s *State) get(name string) *Bucket {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.buckets[name]
}

// sorted returns the account's buckets in name order, byte by byte, which is
// how S3 lists them.
func (s *State) sorted() []*Bucket {
	s.mu.RLock()

	buckets := make([]*Bucket, 0, len(s.buckets))
	for _, b := range s.buckets {
		buckets = append(buckets, b)
	}

	s.mu.RUnlock()

	slices.SortFunc(buckets, func(a, b *Bucket) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		default:
			return 0
		}
	})

	return buckets
}

// take removes the buckets of region, or every bucket when region is empty, and
// returns them.
func (s *State) take(region string) []*Bucket {
	s.mu.Lock()
	defer s.mu.Unlock()

	var taken []*Bucket

	for name, b := range s.buckets {
		if region == "" || b.Region == region {
			taken = append(taken, b)
			delete(s.buckets, name)
		}
	}

	return taken
}

// Bucket is one bucket: its identity, its objects and multipart uploads, and
// every configuration subresource S3 has. The subresources are declared here
// once, typed with the generated shapes; the operations that read and write
// them live in their own files. A nil subresource is one never set.
//
// mu guards everything that changes after creation. There is no lock above it
// that an object operation takes.
type Bucket struct {
	mu sync.RWMutex
	// gone is set once the bucket has left its State, by DeleteBucket or a
	// reset. A call that found the bucket before treats it as missing.
	gone bool

	Name    string
	Account string
	// Region is where the bucket lives: its LocationConstraint, us-east-1 when
	// it has none, eu-west-1 for EU.
	Region string
	// LocationConstraint is the constraint it was created with, "" for none.
	LocationConstraint string
	CreationDate       time.Time

	// Objects maps a key to its versions.
	Objects map[string]*Object
	// Uploads maps an upload id to the multipart upload in progress.
	Uploads map[string]*MultipartUpload

	// keys and uploadOrder are sorted indexes of Objects and Uploads that
	// listings page through; see keyindex.go. They are not state: a bucket built
	// by filling the maps calls reindex.
	keys        []string
	uploadOrder []*MultipartUpload

	Versioning        *s3api.VersioningConfiguration
	ACL               *s3api.AccessControlPolicy
	Policy            *string
	PublicAccessBlock *s3api.PublicAccessBlockConfiguration
	OwnershipControls *s3api.OwnershipControls
	CORS              *s3api.CORSConfiguration
	Website           *s3api.WebsiteConfiguration
	Lifecycle         *s3api.BucketLifecycleConfiguration
	Logging           *s3api.BucketLoggingStatus
	Notification      *s3api.GetBucketNotificationConfigurationOutput
	Replication       *s3api.ReplicationConfiguration
	Encryption        *s3api.ServerSideEncryptionConfiguration
	// EncryptionConfigured is set once a PutBucketEncryption has replaced the
	// SSE-S3 default every bucket starts with, even by an identical rule.
	// LocalStack tells the two apart by the identity of the rule, which a saved
	// bucket does not keep; CopyObject reads it to decide an in-place copy.
	EncryptionConfigured bool
	Tags                 []s3api.Tag
	ObjectLock           *s3api.ObjectLockConfiguration
	Accelerate           *s3api.AccelerateConfiguration
	RequestPayment       *s3api.RequestPaymentConfiguration
	Inventory            map[string]*s3api.InventoryConfiguration
	Analytics            map[string]*s3api.AnalyticsConfiguration
	Metrics              map[string]*s3api.MetricsConfiguration
	IntelligentTiering   map[string]*s3api.IntelligentTieringConfiguration

	// LifecycleMinObjectSize is the TransitionDefaultMinimumObjectSize the
	// lifecycle configuration was put with; empty for the default.
	LifecycleMinObjectSize string
}

// Object is one key of a bucket and its version chain, oldest first: the last
// version is the current one.
type Object struct {
	Versions []*Version
}

// current returns the current version, or nil for an object with none.
func (o *Object) current() *Version {
	if len(o.Versions) == 0 {
		return nil
	}

	return o.Versions[len(o.Versions)-1]
}

// Version is one version of an object, or a delete marker.
type Version struct {
	// ID is the version id, versionNull outside a versioned bucket.
	ID           string
	DeleteMarker bool
	// BlobID names the body in the BlobStore. A delete marker has none.
	BlobID       string
	Size         int64
	ETag         string
	LastModified time.Time

	// Metadata is the user metadata, x-amz-meta-*, keys in lower case.
	Metadata map[string]string
	// The system metadata the client may set, as SYSTEM_METADATA_SETTABLE_HEADERS
	// lists it, and Expires.
	ContentType        string
	CacheControl       string
	ContentDisposition string
	ContentEncoding    string
	ContentLanguage    string
	Expires            string

	// WebsiteRedirectLocation is x-amz-website-redirect-location, which a
	// website redirects the object's URL to.
	WebsiteRedirectLocation string

	StorageClass string
	// The server-side encryption the object is stored with; its bytes are
	// stored plain, as LocalStack does. ServerSideEncryption is the algorithm
	// ("" for none, and for SSE-C), SSEKMSKeyID the key ARN of an aws:kms
	// object, and SSECustomerKeyMD5 the MD5 of the SSE-C key a request must
	// present. See encryption.go.
	ServerSideEncryption string
	SSEKMSKeyID          string
	BucketKeyEnabled     bool
	SSECustomerKeyMD5    string
	// Restore is the Restore header of an archived object that was restored,
	// "" for one that was not.
	Restore string

	ChecksumAlgorithm string
	ChecksumValue     string
	ChecksumType      string

	// Parts is where each part of an object completed from a multipart upload
	// lies in its body, in part order; nil for an object put whole.
	Parts []ObjectPart

	// Lock is the version's Object Lock retention and legal hold. PutObjectRetention
	// and PutObjectLegalHold replace the Version under the bucket lock.
	Lock Lock

	// ACL is the object's access control policy; nil is the private default.
	// Tags is its tag set. Neither changes in place: PutObjectAcl and
	// PutObjectTagging replace the Version under the bucket lock.
	ACL  *s3api.AccessControlPolicy
	Tags []s3api.Tag
}

// blobIDs returns the id of every body the bucket holds: object versions and
// upload parts. The caller holds b.mu.
func (b *Bucket) blobIDs() []string {
	var ids []string

	for _, object := range b.Objects {
		for _, v := range object.Versions {
			if v.BlobID != "" {
				ids = append(ids, v.BlobID)
			}
		}
	}

	for _, upload := range b.Uploads {
		for _, part := range upload.Parts {
			ids = append(ids, part.BlobID)
		}
	}

	return ids
}

// nameIndex maps every bucket name to the account that owns it: names are
// unique across accounts, as LocalStack's CrossAccountAttribute
// global_bucket_map holds them. Its lock is held for one map operation only,
// never across an S3 operation.
type nameIndex struct {
	mu     sync.RWMutex
	owners map[string]string
}

func (n *nameIndex) owner(name string) (string, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()

	account, ok := n.owners[name]

	return account, ok
}

// claim gives name to account unless another account, or account itself,
// holds it already. It returns the holder.
func (n *nameIndex) claim(name, account string) (string, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if holder, taken := n.owners[name]; taken {
		return holder, false
	}

	if n.owners == nil {
		n.owners = make(map[string]string)
	}

	n.owners[name] = account

	return account, true
}

// release frees name when account holds it.
func (n *nameIndex) release(name, account string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.owners[name] == account {
		delete(n.owners, name)
	}
}
