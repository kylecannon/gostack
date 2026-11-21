package s3

import (
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Records document is checked as bytes, not as parsed JSON: a client that
// compares the text sees the members in the order LocalStack's
// _get_event_payload writes them, and a Go map encodes sorted.

const (
	orderBucket   = "orders"
	orderConfigID = "cfg-1"
	orderETag     = "e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1"
	orderSeq      = `"sequencer":"0055AED6DCD90281E5"`
	orderHost     = `"x-amz-id-2":"eftixk72aD6Ap51TnqcoF8eFidJG9Z/2"`
	orderPrinc    = "AIDAJDPLRKLG7UEXAMPLE"
	orderGlacier  = "AmazonCustomer:A3NL1KOZZKExample"
)

var requestIDMember = regexp.MustCompile(`"x-amz-request-id":"([0-9a-f]{8})"`)

// orderDocument is the document LocalStack writes: the record's members in its
// order, with the object's and the Completed record's parts as given.
func orderDocument(version, name, at, principal, requestID, object, glacier string) string {
	return `{"Records":[{"eventVersion":"` + version + `","eventSource":"aws:s3","awsRegion":"eu-west-1",` +
		`"eventTime":"` + at + `","eventName":"` + name + `","userIdentity":{"principalId":"` + principal + `"},` +
		`"requestParameters":{"sourceIPAddress":"127.0.0.1"},` +
		`"responseElements":{"x-amz-request-id":"` + requestID + `",` + orderHost + `},` +
		`"s3":{"s3SchemaVersion":"1.0","configurationId":"` + orderConfigID + `","bucket":{"name":"` + orderBucket +
		`","ownerIdentity":{"principalId":"A3NL1KOZZKExample"},"arn":"arn:aws:s3:::` + orderBucket + `"},` +
		`"object":` + object + `}` + glacier + `}]}`
}

func Test_event_record_MembersAreInLocalStacksOrder(t *testing.T) {
	const (
		created  = "2025-01-02T03:04:05.678Z"
		restored = "2025-01-02T03:04:06.178Z" // 500 ms after the Post record
		key      = `"key":"a%40b",`
		versionP = `"versionId":"v1",`
		etagP    = `"eTag":"` + orderETag + `"`
		emptyP   = `"eTag":"` + emptyETag + `"`
	)

	expires := time.Date(2025, time.March, 4, 5, 6, 7, 8000000, time.UTC)
	glacier := `,"glacierEventData":{"restoreEventData":{"lifecycleRestorationExpiryTime":"2025-03-04T05:06:07.008Z",` +
		`"lifecycleRestoreStorageClass":"GLACIER"}}`

	put := `{` + key + orderSeq + `,` + etagP + `,"size":3}`
	putVersioned := `{` + key + orderSeq + `,` + versionP + etagP + `,"size":3}`
	tagged := `{` + key + etagP + `}`
	taggedVersioned := `{` + key + versionP + etagP + `}`

	tests := []struct {
		name          string
		give          event
		wantVersion   string
		wantEventName string
		wantPrincipal string
		wantAt        string
		wantObject    string
		wantGlacier   string
	}{
		{
			name: "put", give: event{name: eventPut, etag: orderETag, size: 3},
			wantObject: put,
		},
		{
			name: "put in a versioned bucket", give: event{name: eventPut, etag: orderETag, size: 3, versionID: "v1"},
			wantObject: putVersioned,
		},
		{
			name:       "put in a bucket that never versioned",
			give:       event{name: eventPut, etag: orderETag, size: 3, versionID: versionNull},
			wantObject: put,
		},
		{
			name: "post", give: event{name: eventPost, etag: orderETag, size: 3, versionID: "v1"},
			wantObject: putVersioned,
		},
		{
			name: "copy", give: event{name: eventCopy, etag: orderETag, size: 3, versionID: "v1"},
			wantObject: putVersioned,
		},
		{
			name: "complete multipart upload", give: event{name: eventComplete, etag: orderETag, size: 3, versionID: "v1"},
			wantObject: putVersioned,
		},
		{
			name: "delete of a key", give: event{name: eventRemoved, etag: emptyETag},
			wantObject: `{` + key + orderSeq + `}`,
		},
		{
			name: "delete of a version", give: event{name: eventRemoved, etag: emptyETag, versionID: "v1"},
			wantObject: `{` + key + orderSeq + `,"versionId":"v1"}`,
		},
		{
			name: "delete marker created", give: event{name: eventMarkerCreated, etag: emptyETag, versionID: "v1"},
			wantObject: `{` + key + orderSeq + `,` + versionP + emptyP + `}`,
		},
		{
			name: "restore initiated", give: event{name: eventRestorePost, etag: orderETag, size: 3, versionID: "v1"},
			wantObject: putVersioned,
		},
		{
			name: "restore completed",
			give: event{
				name: eventRestoreComplete, etag: orderETag, size: 3, versionID: "v1", storageClass: "GLACIER", expires: expires,
			},
			wantObject: putVersioned, wantPrincipal: orderGlacier, wantAt: restored, wantGlacier: glacier,
		},
		{
			name: "tagging put", give: event{name: eventTaggingPut, etag: orderETag, size: 3},
			wantVersion: recordVersionTagACL, wantObject: tagged,
		},
		{
			name:        "tagging delete of a version",
			give:        event{name: eventTaggingDelete, etag: orderETag, size: 3, versionID: "v1"},
			wantVersion: recordVersionTagACL, wantObject: taggedVersioned,
		},
		{
			name: "acl put", give: event{name: eventACLPut, etag: orderETag, size: 3, versionID: "v1"},
			wantVersion: recordVersionTagACL, wantObject: taggedVersioned,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := tt.give
			ev.time = time.Date(2025, time.January, 2, 3, 4, 5, 678000000, time.UTC)
			ev.region = regionEUWest1
			ev.bucketRegion = regionEUWest1
			ev.bucket = orderBucket
			ev.key = "a%40b"

			wantVersion := tt.wantVersion
			if wantVersion == "" {
				wantVersion = recordVersion
			}

			wantPrincipal := tt.wantPrincipal
			if wantPrincipal == "" {
				wantPrincipal = orderPrinc
			}

			wantAt := tt.wantAt
			if wantAt == "" {
				wantAt = created
			}

			got, err := encodeJSON(notification{Records: []eventRecord{ev.record(orderConfigID)}})
			require.NoError(t, err)

			match := requestIDMember.FindSubmatch(got)
			require.NotNil(t, match, "the record carries a request id: %s", got)

			want := orderDocument(wantVersion, ev.name[len("s3:"):], wantAt, wantPrincipal, string(match[1]),
				tt.wantObject, tt.wantGlacier)
			assert.Equal(t, want, string(got))
		})
	}
}

func Test_testEvent_MembersAreInLocalStacksOrder(t *testing.T) {
	b := &Bucket{Name: orderBucket}

	got, err := testEvent(t.Context(), b)
	require.NoError(t, err)

	// A context with no request has the empty request id.
	assert.Regexp(t, `^\{"Service":"Amazon S3","Event":"s3:TestEvent","Time":"[^"]+","Bucket":"`+orderBucket+
		`","RequestId":"","HostId":"eftixk72aD6Ap51TnqcoF8eFidJG9Z/2"\}$`, string(got))
}
