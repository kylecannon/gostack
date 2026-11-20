package envelope

import (
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Type is the envelope's Type field and the x-amz-sns-message-type header.
type Type string

// The three envelope types.
const (
	Notification             Type = "Notification"
	SubscriptionConfirmation Type = "SubscriptionConfirmation"
	UnsubscribeConfirmation  Type = "UnsubscribeConfirmation"
)

// TimestampLayout is the envelope's Timestamp: UTC, milliseconds, Z suffix.
const TimestampLayout = "2006-01-02T15:04:05.000Z"

// ErrUnknownType reports an envelope Type outside the three SNS defines.
var ErrUnknownType = errors.New("unknown sns envelope type")

// Attribute is one message attribute as the publisher sent it.
type Attribute struct {
	Name     string
	DataType string
	// StringValue is the value of a String or Number attribute. Nil is
	// written as null, as LocalStack writes an attribute with no StringValue.
	StringValue *string
	BinaryValue []byte
}

// Message is the input of one envelope.
type Message struct {
	// Type is the envelope type; empty means Notification.
	Type      Type
	MessageID string
	// TopicARN decides FIFO: a topic whose ARN ends in ".fifo" gets a
	// SequenceNumber and no signature, as in LocalStack.
	TopicARN string
	// SubscriptionARN builds a Notification's UnsubscribeURL.
	SubscriptionARN string
	// Token builds a confirmation's Token and SubscribeURL.
	Token   string
	Subject string
	Message string
	// Timestamp is written in UTC to the millisecond.
	Timestamp time.Time
	// Attributes are written sorted by name: the publisher's order is gone
	// before the message gets here, since the request holds them in a map. A
	// Notification carries them; none is written when the list is empty.
	Attributes []Attribute
	// SequenceNumber is written for a FIFO topic only, as null when it is
	// empty, as LocalStack writes a confirmation's missing one.
	SequenceNumber string
	// SignatureVersion is the topic's: "2" signs with SHA256, anything else
	// with SHA1. Empty is written as "1".
	SignatureVersion string
}

// BaseURL is the scheme and host every envelope URL starts with:
// https://<certHost> when SNS_CERT_URL_HOST is set, otherwise the external
// service URL without a trailing slash (publisher.py get_cert_base_url).
func BaseURL(certHost, externalURL string) string {
	if certHost != "" {
		return "https://" + certHost
	}

	return strings.TrimRight(externalURL, "/")
}

// Body returns the envelope for in as JSON, for the subscription
// in.SubscriptionARN. baseURL prefixes SigningCertURL, SubscribeURL and
// UnsubscribeURL; see BaseURL. A message delivered to more than one
// subscriber is signed once with Sign instead.
func (s *Signer) Body(in *Message, baseURL string) ([]byte, error) {
	signed, err := s.Sign(in, baseURL)
	if err != nil {
		return nil, err
	}

	return signed.Body(in.SubscriptionARN), nil
}

// Signed is a message signed once, whose envelope can be written for any
// subscriber of its topic: the signature covers no per-subscriber field, the
// UnsubscribeURL being the only one. It is safe for concurrent use.
type Signed struct {
	m    Message
	f    envelopeFields
	base string
	hint int
}

// Sign signs in for every subscriber of its topic. in.SubscriptionARN is
// ignored; Body takes each subscriber's.
//
// What is signed is exactly what a subscriber parses back: every string
// field is made valid UTF-8 first, a run of invalid bytes becoming one U+FFFD,
// so the body and the canonical string hold the same text.
func (s *Signer) Sign(in *Message, baseURL string) (*Signed, error) {
	m := valid(in)

	kind := m.Type
	if kind == "" {
		kind = Notification
	}

	if kind != Notification && kind != SubscriptionConfirmation && kind != UnsubscribeConfirmation {
		return nil, fmt.Errorf("%w: %q", ErrUnknownType, kind)
	}

	fields := envelopeFields{
		kind:      kind,
		timestamp: m.Timestamp.UTC().Format(TimestampLayout),
		fifo:      strings.HasSuffix(m.TopicARN, ".fifo"),
	}

	if kind != Notification {
		fields.subscribeURL = baseURL + "/?Action=ConfirmSubscription&TopicArn=" + m.TopicARN + "&Token=" + m.Token
	}

	if !fields.fifo {
		fields.version = m.SignatureVersion
		if fields.version == "" {
			fields.version = "1"
		}

		sig, err := s.signCanonical(canonical(&m, &fields), fields.version)
		if err != nil {
			return nil, err
		}

		fields.signature = base64.StdEncoding.EncodeToString(sig)
		fields.certURL = baseURL + CertPath
	}

	return &Signed{m: m, f: fields, base: baseURL, hint: sizeHint(&m, &fields)}, nil
}

// Body returns the envelope as JSON for the subscription subscriptionARN, which
// a Notification's UnsubscribeURL names.
func (sg *Signed) Body(subscriptionARN string) []byte {
	f := sg.f
	if f.kind == Notification {
		f.unsubscribeURL = sg.base + "/?Action=Unsubscribe&SubscriptionArn=" + validString(subscriptionARN)
	}

	return appendEnvelope(make([]byte, 0, sg.hint+len(f.unsubscribeURL)), &sg.m, &f)
}

// valid is a copy of in with every string field valid UTF-8 and the
// attributes sorted by name.
func valid(in *Message) Message {
	m := *in
	m.MessageID = validString(m.MessageID)
	m.TopicARN = validString(m.TopicARN)
	m.Token = validString(m.Token)
	m.Subject = validString(m.Subject)
	m.Message = validString(m.Message)
	m.SequenceNumber = validString(m.SequenceNumber)

	if len(in.Attributes) == 0 {
		return m
	}

	m.Attributes = make([]Attribute, len(in.Attributes))
	for i, a := range in.Attributes {
		a.Name = validString(a.Name)
		a.DataType = validString(a.DataType)

		if a.StringValue != nil {
			a.StringValue = new(validString(*a.StringValue))
		}

		m.Attributes[i] = a
	}

	slices.SortFunc(m.Attributes, func(a, b Attribute) int { return strings.Compare(a.Name, b.Name) })

	return m
}

// validString is s with each run of invalid UTF-8 bytes replaced by U+FFFD.
func validString(s string) string {
	if utf8.ValidString(s) {
		return s
	}

	return strings.ToValidUTF8(s, "�")
}

// envelopeFields are the values Body derives from a Message.
type envelopeFields struct {
	kind           Type
	timestamp      string
	fifo           bool
	unsubscribeURL string
	subscribeURL   string
	version        string
	signature      string
	certURL        string
}

// canonical is the string SNS signs: each field present in the envelope, in
// this order, as "Name\nvalue\n" (publisher.py compute_canonical_string, and
// AWS's sns-verify-signature-of-message).
func canonical(m *Message, f *envelopeFields) []byte {
	const names = 256 // the field names, newlines and the short fields

	b := make([]byte, 0, names+len(m.Message)+len(m.Subject)+len(f.subscribeURL)+len(m.Token))
	b = appendLine(b, "Message", m.Message)
	b = appendLine(b, "MessageId", m.MessageID)

	if f.kind == Notification {
		if m.Subject != "" {
			b = appendLine(b, "Subject", m.Subject)
		}

		b = appendLine(b, "Timestamp", f.timestamp)
	} else {
		b = appendLine(b, "SubscribeURL", f.subscribeURL)
		b = appendLine(b, "Timestamp", f.timestamp)
		b = appendLine(b, "Token", m.Token)
	}

	b = appendLine(b, "TopicArn", m.TopicARN)

	return appendLine(b, "Type", string(f.kind))
}

func appendLine(b []byte, name, value string) []byte {
	b = append(b, name...)
	b = append(b, '\n')
	b = append(b, value...)

	return append(b, '\n')
}

// appendEnvelope writes the envelope in AWS's field order. A Notification:
// Type, MessageId, SequenceNumber (FIFO), TopicArn, Subject, Message,
// Timestamp, the three signature fields (not FIFO), UnsubscribeURL,
// MessageAttributes. A confirmation: Type, MessageId, SequenceNumber (FIFO),
// Token, TopicArn, Subject, Message, SubscribeURL, Timestamp, the signature
// fields (not FIFO), MessageAttributes.
func appendEnvelope(b []byte, m *Message, f *envelopeFields) []byte {
	b = append(b, '{')
	b = appendMember(b, "Type", string(f.kind))
	b = appendMember(b, "MessageId", m.MessageID)

	if f.fifo {
		if m.SequenceNumber == "" {
			b = appendNull(b, "SequenceNumber")
		} else {
			b = appendMember(b, "SequenceNumber", m.SequenceNumber)
		}
	}

	if f.kind != Notification {
		b = appendMember(b, "Token", m.Token)
	}

	b = appendMember(b, "TopicArn", m.TopicARN)

	if m.Subject != "" {
		b = appendMember(b, "Subject", m.Subject)
	}

	b = appendMember(b, "Message", m.Message)

	if f.kind != Notification {
		b = appendMember(b, "SubscribeURL", f.subscribeURL)
	}

	b = appendMember(b, "Timestamp", f.timestamp)

	if !f.fifo {
		b = appendMember(b, "SignatureVersion", f.version)
		b = appendMember(b, "Signature", f.signature)
		b = appendMember(b, "SigningCertURL", f.certURL)
	}

	if f.kind == Notification {
		b = appendMember(b, "UnsubscribeURL", f.unsubscribeURL)
	}

	if len(m.Attributes) > 0 {
		b = appendAttributes(b, m.Attributes)
	}

	return append(b, '}')
}

// appendAttributes writes MessageAttributes as LocalStack's
// prepare_message_attributes does: {"<name>": {"Type": <DataType>, "Value":
// <value>}}, where a Binary value is base64 of its bytes.
func appendAttributes(b []byte, attrs []Attribute) []byte {
	b = append(b, `, "MessageAttributes": {`...)

	for i, a := range attrs {
		if i > 0 {
			b = append(b, ", "...)
		}

		b = appendString(b, a.Name)
		b = append(b, `: {"Type": `...)
		b = appendString(b, a.DataType)
		b = append(b, `, "Value": `...)

		switch {
		case strings.HasPrefix(a.DataType, "Binary"):
			b = append(b, '"')
			b = base64.StdEncoding.AppendEncode(b, a.BinaryValue)
			b = append(b, '"')
		case a.StringValue == nil:
			b = append(b, "null"...)
		default:
			b = appendString(b, *a.StringValue)
		}

		b = append(b, '}')
	}

	return append(b, '}')
}

// appendMember writes one string member with json.dumps's separators: ", "
// before every member but the first, ": " after the name.
func appendMember(b []byte, name, value string) []byte {
	if b[len(b)-1] != '{' {
		b = append(b, ", "...)
	}

	b = append(b, '"')
	b = append(b, name...)
	b = append(b, `": `...)

	return appendString(b, value)
}

// appendNull writes one member whose value is null, separated as appendMember.
func appendNull(b []byte, name string) []byte {
	if b[len(b)-1] != '{' {
		b = append(b, ", "...)
	}

	b = append(b, '"')
	b = append(b, name...)

	return append(b, `": null`...)
}

// sizeHint is the envelope's length before string escapes: the variable
// fields plus the fixed names, separators and base64 signature.
func sizeHint(m *Message, f *envelopeFields) int {
	const (
		fixed     = 512 // member names and separators
		perAttrib = 32  // an attribute's names, quotes and braces
	)

	n := fixed + len(m.MessageID) + len(m.TopicARN) + len(m.Subject) + len(m.Message) +
		len(m.Token) + len(m.SequenceNumber) + len(f.unsubscribeURL) + len(f.subscribeURL) +
		len(f.signature) + len(f.certURL)

	for _, a := range m.Attributes {
		n += perAttrib + len(a.Name) + len(a.DataType) + len(deref(a.StringValue)) +
			base64.StdEncoding.EncodedLen(len(a.BinaryValue))
	}

	return n
}

// deref is what s points to, or "" for nil.
func deref(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}
