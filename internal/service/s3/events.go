package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/kylecannon/gostack/internal/account"
	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/inproc"
	"github.com/kylecannon/gostack/internal/request"
)

// The event names S3 sends, as LocalStack's EVENT_OPERATION_MAP names them.
const (
	eventPut             = "s3:ObjectCreated:Put"
	eventPost            = "s3:ObjectCreated:Post"
	eventCopy            = "s3:ObjectCreated:Copy"
	eventComplete        = "s3:ObjectCreated:CompleteMultipartUpload"
	eventTaggingPut      = "s3:ObjectTagging:Put"
	eventTaggingDelete   = "s3:ObjectTagging:Delete"
	eventRemoved         = "s3:ObjectRemoved:Delete"
	eventMarkerCreated   = "s3:ObjectRemoved:DeleteMarkerCreated"
	eventACLPut          = "s3:ObjectAcl:Put"
	eventRestorePost     = "s3:ObjectRestore:Post"
	eventRestoreComplete = "s3:ObjectRestore:Completed"
)

const (
	// The values LocalStack puts in every record: the account and request
	// identities are placeholders, and so are the sequencer and the host ids.
	recordVersion         = "2.1"
	recordVersionTagACL   = "2.3"
	recordSource          = "aws:s3"
	recordSchemaVersion   = "1.0"
	recordPrincipal       = "AIDAJDPLRKLG7UEXAMPLE"
	recordPrincipalGlacir = "AmazonCustomer:A3NL1KOZZKExample"
	recordOwnerPrincipal  = "A3NL1KOZZKExample"
	recordSourceIP        = "127.0.0.1"
	recordHostID          = "eftixk72aD6Ap51TnqcoF8eFidJG9Z/2"
	recordSequencer       = "0055AED6DCD90281E5"

	// emptyETag is the ETag AWS gives a delete marker: that of an empty body.
	emptyETag = "d41d8cd98f00b204e9800998ecf8427e"

	// eventTimeFormat is timestamp_millis: milliseconds and a Z.
	eventTimeFormat = "2006-01-02T15:04:05.000Z"

	// restoreCompletedDelay is how far after the Post record the Completed one
	// is dated, so that the two sort in that order although both are instant.
	restoreCompletedDelay = 500 * time.Millisecond

	headerXRay = "X-Amzn-Trace-Id"

	// sqsSystemAttributeTrace is the SQS system attribute the X-Ray header is
	// handed on in.
	sqsSystemAttributeTrace = "AWSTraceHeader"
)

// eventNames maps the operation a request ran to the event it raises.
var eventNames = map[string]string{
	"PutObject":               eventPut,
	"PostObject":              eventPost,
	"CopyObject":              eventCopy,
	"CompleteMultipartUpload": eventComplete,
	"PutObjectTagging":        eventTaggingPut,
	"DeleteObjectTagging":     eventTaggingDelete,
	"DeleteObject":            eventRemoved,
	"DeleteObjects":           eventRemoved,
	"PutObjectAcl":            eventACLPut,
	"RestoreObject":           eventRestorePost,
}

// Bind gives S3 the handler its notifications are delivered through, normally
// the gateway's. Until it is bound, a notification target is not validated and
// no event is delivered.
func (p *Provider) Bind(h http.Handler) {
	p.client.Bind(h)
}

// event is one thing that happened to an object, with what a record of it
// reports.
type event struct {
	name   string
	time   time.Time
	region string // the region of the request: it names the partition
	xray   string

	bucket       string
	account      string // the bucket's owner
	bucketRegion string
	key          string // as the record spells it: percent-encoded
	versionID    string
	etag         string
	size         int64
	storageClass string
	expires      time.Time
}

// notify raises the event the running operation stands for on the object
// that was written or removed. v is nil for an object that was not found, as
// DeleteObjects reports a missing key. The caller holds no lock of b.
func (p *Provider) notify(ctx context.Context, b *Bucket, key string, v *Version) {
	p.raise(ctx, b, key, v, "")
}

// notifyDeletion raises the event a delete makes: DeleteMarkerCreated when it
// added a delete marker, Delete otherwise. A removed version or marker is
// named by its id; its ETag and size are gone with it, so the record carries
// the empty ETag, as it does for a key that was not there.
func (p *Provider) notifyDeletion(ctx context.Context, b *Bucket, key string, d deletion) {
	if d.versionID == "" {
		p.notify(ctx, b, key, nil)

		return
	}

	gone := &Version{ID: d.versionID, DeleteMarker: true}
	if d.added {
		p.raise(ctx, b, key, gone, eventMarkerCreated)

		return
	}

	p.raise(ctx, b, key, gone, "")
}

// notifyRestore raises the pair of events a restore does at once: it was
// initiated, and it is complete.
func (p *Provider) notifyRestore(ctx context.Context, b *Bucket, key string, v *Version) {
	p.raise(ctx, b, key, v, eventRestorePost)
	p.raise(ctx, b, key, v, eventRestoreComplete)
}

// raise delivers the event to every queue the bucket's configuration sends it
// to. name overrides the event the operation maps to.
func (p *Provider) raise(ctx context.Context, b *Bucket, key string, v *Version, name string) {
	cfg := b.notificationConfig()
	if cfg == nil || len(cfg.QueueConfigurations) == 0 || !p.client.Bound() {
		return
	}

	rc, ok := request.From(ctx)
	if !ok {
		return
	}

	if name == "" {
		name = eventNames[rc.Operation]
	}

	if name == "" {
		return
	}

	ev := newEvent(rc, b, name, key, v)

	for i := range cfg.QueueConfigurations {
		queue := &cfg.QueueConfigurations[i]
		if !matchesEvent(queue.Events, name) || !matchesFilter(queue.Filter, ev.key) {
			continue
		}

		p.sendEvent(&ev, queue)
	}
}

// notificationConfig returns the bucket's notification configuration, nil for
// none. It is never changed once stored, so a caller may read it unlocked.
func (b *Bucket) notificationConfig() *s3api.GetBucketNotificationConfigurationOutput {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.Notification
}

// newEvent describes what happened to key. A delete marker, and an object that
// was not found, carry the ETag of an empty body and no size.
func newEvent(rc *request.Context, b *Bucket, name, key string, v *Version) event {
	ev := event{
		name:         name,
		time:         time.Now().UTC(),
		region:       rc.Region,
		xray:         rc.Request.Header.Get(headerXRay),
		bucket:       b.Name,
		account:      b.Account,
		bucketRegion: b.Region,
		key:          quote(key),
	}

	b.mu.RLock()
	versioned := b.versioned()
	b.mu.RUnlock()

	switch {
	case v == nil || v.DeleteMarker:
		ev.etag = emptyETag
	default:
		ev.etag = v.ETag
		ev.size = v.Size
		ev.storageClass = v.StorageClass
		ev.expires = parseHTTPDate(v.Expires)
	}

	if v != nil && versioned {
		ev.versionID = v.ID
	}

	return ev
}

// parseHTTPDate reads an Expires header, the zero time when it is not one.
func parseHTTPDate(value string) time.Time {
	t, err := http.ParseTime(value)
	if err != nil {
		return time.Time{}
	}

	return t.UTC()
}

// matchesEvent reports whether a configuration's events include the event, or
// its family's wildcard, as _matching_event does.
func matchesEvent(events []string, name string) bool {
	family := name[:strings.LastIndexByte(name, ':')] + ":*"

	for _, e := range events {
		if e == name || e == family {
			return true
		}
	}

	return false
}

// matchesFilter reports whether the key passes the prefix and suffix rules of
// the filter. The key is the encoded one, as LocalStack matches it.
func matchesFilter(filter *s3api.NotificationConfigurationFilter, key string) bool {
	if filter == nil || filter.Key == nil {
		return true
	}

	for _, rule := range filter.Key.FilterRules {
		value := deref(rule.Value)

		switch strings.ToLower(deref(rule.Name)) {
		case "prefix":
			if !strings.HasPrefix(key, value) {
				return false
			}
		case "suffix":
			if !strings.HasSuffix(key, value) {
				return false
			}
		default:
			// PutBucketNotificationConfiguration refuses every other rule name.
		}
	}

	return true
}

// The record is the JSON S3 sends. The members are in the order LocalStack
// writes them.
type eventRecord struct {
	EventVersion      string            `json:"eventVersion"`
	EventSource       string            `json:"eventSource"`
	AWSRegion         string            `json:"awsRegion"`
	EventTime         string            `json:"eventTime"`
	EventName         string            `json:"eventName"`
	UserIdentity      principal         `json:"userIdentity"`
	RequestParameters map[string]string `json:"requestParameters"`
	ResponseElements  map[string]string `json:"responseElements"`
	S3                eventS3           `json:"s3"`
	GlacierEventData  *glacierEvent     `json:"glacierEventData,omitempty"`
}

type principal struct {
	PrincipalID string `json:"principalId"`
}

type eventS3 struct {
	SchemaVersion   string      `json:"s3SchemaVersion"`
	ConfigurationID string      `json:"configurationId"`
	Bucket          eventBucket `json:"bucket"`
	Object          eventObject `json:"object"`
}

type eventBucket struct {
	Name          string    `json:"name"`
	OwnerIdentity principal `json:"ownerIdentity"`
	ARN           string    `json:"arn"`
}

type eventObject struct {
	Key       string  `json:"key"`
	Sequencer string  `json:"sequencer,omitempty"`
	VersionID string  `json:"versionId,omitempty"`
	ETag      *string `json:"eTag,omitempty"`
	Size      *int64  `json:"size,omitempty"`
}

type glacierEvent struct {
	RestoreEventData restoreEventData `json:"restoreEventData"`
}

type restoreEventData struct {
	LifecycleRestorationExpiryTime string `json:"lifecycleRestorationExpiryTime"`
	LifecycleRestoreStorageClass   string `json:"lifecycleRestoreStorageClass"`
}

// notification is the message body: a list of one record.
type notification struct {
	Records []eventRecord `json:"Records"`
}

// record builds the record of the event for the configuration with this id, as
// _get_event_payload does.
func (ev *event) record(configID string) eventRecord {
	name := strings.ToLower(ev.name)
	at := ev.time

	rec := eventRecord{
		EventVersion:      recordVersion,
		EventSource:       recordSource,
		AWSRegion:         ev.bucketRegion,
		EventName:         strings.TrimPrefix(ev.name, "s3:"),
		UserIdentity:      principal{PrincipalID: recordPrincipal},
		RequestParameters: map[string]string{"sourceIPAddress": recordSourceIP},
		ResponseElements:  map[string]string{"x-amz-request-id": shortID(), "x-amz-id-2": recordHostID},
		S3: eventS3{
			SchemaVersion:   recordSchemaVersion,
			ConfigurationID: configID,
			Bucket: eventBucket{
				Name:          ev.bucket,
				OwnerIdentity: principal{PrincipalID: recordOwnerPrincipal},
				ARN:           "arn:" + account.Partition(ev.region) + ":s3:::" + ev.bucket,
			},
			Object: eventObject{Key: ev.key, Sequencer: recordSequencer},
		},
	}

	if ev.versionID != "" && ev.versionID != versionNull {
		rec.S3.Object.VersionID = ev.versionID
	}

	if strings.Contains(name, "created") || strings.Contains(name, "restore") {
		etag := ev.etag
		rec.S3.Object.ETag = &etag

		if !strings.Contains(name, "deletemarker") {
			size := ev.size
			rec.S3.Object.Size = &size
		}
	}

	if strings.Contains(ev.name, "ObjectTagging") || strings.Contains(ev.name, "ObjectAcl") {
		etag := ev.etag
		rec.EventVersion = recordVersionTagACL
		rec.S3.Object.ETag = &etag
		rec.S3.Object.Sequencer = ""
	}

	if strings.Contains(name, "objectrestore:completed") {
		rec.GlacierEventData = ev.glacierData()
		rec.UserIdentity.PrincipalID = recordPrincipalGlacir
		// The restore is instant, so the record is dated a little after the Post
		// one: they are sent in that order, and a consumer sorts by time.
		at = at.Add(restoreCompletedDelay)
	}

	rec.EventTime = at.Format(eventTimeFormat)

	return rec
}

// glacierData is what the Completed record of a restore adds: when the restored
// copy expires, which is the object's Expires header and else now, as LocalStack
// writes it, and the class it was restored from.
func (ev *event) glacierData() *glacierEvent {
	expires := ev.expires
	if expires.IsZero() {
		expires = time.Now().UTC()
	}

	return &glacierEvent{RestoreEventData: restoreEventData{
		LifecycleRestorationExpiryTime: expires.Format(eventTimeFormat),
		LifecycleRestoreStorageClass:   ev.storageClass,
	}}
}

// encodeJSON is json.dumps for a value that needs no HTML escaping.
func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	err := enc.Encode(v)
	if err != nil {
		return nil, fmt.Errorf("encode JSON: %w", err)
	}

	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// sendEvent queues the event's message for one queue configuration. The
// delivery is asynchronous: a queue that is gone, or an unbound client, costs
// a log line and nothing else, as it does in LocalStack. The queue's ARN is the
// call's lane, so the events for one queue arrive in the order they were
// raised, and a queue that is slow to take them holds up no other.
func (p *Provider) sendEvent(ev *event, queue *s3api.QueueConfiguration) {
	queueARN := deref(queue.QueueArn)

	arn := parseARN(queueARN)
	if arn.err != nil {
		slog.Error("S3 notification skipped: unparseable queue ARN", "bucket", ev.bucket, "arn", queueARN)

		return
	}

	body, err := encodeJSON(notification{Records: []eventRecord{ev.record(deref(queue.Id))}})
	if err != nil {
		slog.Error("S3 notification skipped: cannot encode the event", "bucket", ev.bucket, "error", err)

		return
	}

	call, err := sendMessageCall(arn, ev.account, string(body), ev.xray)
	if err != nil {
		slog.Error("S3 notification skipped: cannot encode the call", "bucket", ev.bucket, "error", err)

		return
	}

	call.Lane = queueARN

	queued := p.client.Enqueue(call, func(resp inproc.Response, err error) {
		logDelivery(ev.bucket, arn.resource, resp, err)
	})
	if !queued {
		slog.Warn("S3 notification dropped: the delivery queue is full or closed", "bucket", ev.bucket, "queue", arn.resource)
	}
}

// logDelivery records a notification that SQS did not take.
func logDelivery(bucket, queue string, resp inproc.Response, err error) {
	if err != nil {
		slog.Error("Unable to send notification for S3 bucket", "bucket", bucket, "queue", queue, "error", err)

		return
	}

	if !resp.OK() {
		slog.Error("Unable to send notification for S3 bucket", "bucket", bucket, "queue", queue, "status", resp.Status)
	}
}
