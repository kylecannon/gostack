package sqs

import (
	"crypto/md5" //nolint:gosec // G501: MD5OfBody and MD5OfMessageAttributes are MD5 checksums by the SQS API, not security.
	"encoding/binary"
	"encoding/hex"
	"maps"
	"slices"
	"strconv"
	"time"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/request"
)

// Names of the system attributes of a message, from botocore's
// MessageSystemAttributeName enum.
const (
	sysSenderID                         = "SenderId"
	sysSentTimestamp                    = "SentTimestamp"
	sysApproximateReceiveCount          = "ApproximateReceiveCount"
	sysApproximateFirstReceiveTimestamp = "ApproximateFirstReceiveTimestamp"
	sysAWSTraceHeader                   = "AWSTraceHeader"
	sysMessageGroupID                   = "MessageGroupId"

	// headerTraceID is the header X-Ray puts the trace on. A message sent with
	// it carries the AWSTraceHeader attribute.
	headerTraceID = "X-Amzn-Trace-Id"

	// The field index that follows the data type in the MD5 of an attribute:
	// 1 for a string value, 2 for a binary one.
	typeFieldString = 1
	typeFieldBinary = 2
)

// message is a message as a queue holds it. Everything in it is fixed when the
// message is sent: what changes while the message is in the queue (its
// receive count, its deadline) is the contents' own bookkeeping.
type message struct {
	id      string
	body    string
	bodyMD5 string
	// attributes are the user attributes of the message and attributesMD5 the
	// checksum over all of them, "" when there are none.
	attributes    map[string]sqsapi.MessageAttributeValue
	attributesMD5 string
	// system holds the system attributes that the receiver can ask for, apart
	// from the two that change with each receive.
	system map[string]string
	sent   time.Time
}

// delivery is one message handed to a receiver: the message, the receipt handle
// of this receive, and what the contents counted for it.
type delivery struct {
	msg           *message
	handle        string
	receiveCount  int
	firstReceived time.Time
}

// newMessage builds the message of a send request that passed validation.
func newMessage(rc *request.Context, req *sendRequest, now time.Time) *message {
	m := &message{
		id:      newRequestID(),
		body:    deref(req.body),
		sent:    now,
		system:  systemAttributes(rc, req, now),
		bodyMD5: md5Hex([]byte(deref(req.body))),
	}

	if req.attributes != nil {
		m.attributes = req.attributes
		m.attributesMD5 = attributesMD5(req.attributes)
	}

	return m
}

// systemAttributes returns the system attributes a message starts with: who
// sent it and when, the trace header when the request has one, the attributes
// the sender passed and the message group.
func systemAttributes(rc *request.Context, req *sendRequest, now time.Time) map[string]string {
	attrs := map[string]string{
		sysSenderID:      rc.AccountID,
		sysSentTimestamp: strconv.FormatInt(now.UnixMilli(), 10),
	}

	if rc.Request != nil {
		if trace := rc.Request.Header.Get(headerTraceID); trace != "" {
			attrs[sysAWSTraceHeader] = trace
		}
	}

	for name, value := range req.system {
		attrs[name] = deref(value.StringValue)
	}

	if req.groupID != nil {
		attrs[sysMessageGroupID] = *req.groupID
	}

	return attrs
}

// attributesMD5 is the MD5OfMessageAttributes checksum: for each attribute in
// name order, the name, the data type and the value, each as a four-byte
// big-endian length and the bytes, with a byte before the value that says
// whether it is a string (1) or binary (2). An attribute with neither
// contributes its name and type only, as in LocalStack.
func attributesMD5(attrs map[string]sqsapi.MessageAttributeValue) string {
	sum := md5.New() //nolint:gosec // G401: the checksum is part of the SQS API.

	var buf []byte

	for _, name := range slices.Sorted(maps.Keys(attrs)) {
		value := attrs[name]

		buf = appendLengthPrefixed(buf[:0], []byte(name))
		buf = appendLengthPrefixed(buf, []byte(deref(value.DataType)))

		switch {
		case deref(value.StringValue) != "":
			buf = append(buf, typeFieldString)
			buf = appendLengthPrefixed(buf, []byte(*value.StringValue))
		case len(value.BinaryValue) > 0:
			buf = append(buf, typeFieldBinary)
			buf = appendLengthPrefixed(buf, value.BinaryValue)
		default:
			// A value of another kind is not part of the checksum.
		}

		sum.Write(buf)
	}

	return hex.EncodeToString(sum.Sum(nil))
}

// systemAttributesMD5 is MD5OfMessageSystemAttributes: the same checksum over
// the system attributes the sender passed.
func systemAttributesMD5(attrs map[string]sqsapi.MessageSystemAttributeValue) string {
	converted := make(map[string]sqsapi.MessageAttributeValue, len(attrs))
	for name, value := range attrs {
		converted[name] = sqsapi.MessageAttributeValue(value)
	}

	return attributesMD5(converted)
}

func appendLengthPrefixed(buf, value []byte) []byte {
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(value))) //nolint:gosec // G115: a message is far below 4 GiB.

	return append(buf, value...)
}

func md5Hex(data []byte) string {
	sum := md5.Sum(data) //nolint:gosec // G401: MD5OfBody is part of the SQS API.

	return hex.EncodeToString(sum[:])
}

// deref returns what p points to, or "" for nil.
func deref(p *string) string {
	if p == nil {
		return ""
	}

	return *p
}
