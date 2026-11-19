package envelope

import (
	"encoding/base64"
	"errors"
	"fmt"
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
	Name        string
	DataType    string
	StringValue string
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
	// Attributes keep the publisher's order. A Notification carries them;
	// none is written when the list is empty.
	Attributes []Attribute
	// SequenceNumber is written for a FIFO topic only.
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

// Body returns the envelope for in as JSON. baseURL prefixes SigningCertURL,
// SubscribeURL and UnsubscribeURL; see BaseURL.
func (s *Signer) Body(in *Message, baseURL string) ([]byte, error) {
	// The signed text must be the text a subscriber parses back, so invalid
	// UTF-8 in the publisher's message or subject becomes U+FFFD in both.
	m := in
	if !utf8.ValidString(in.Message) || !utf8.ValidString(in.Subject) {
		valid := *in
		valid.Message = strings.ToValidUTF8(in.Message, "�")
		valid.Subject = strings.ToValidUTF8(in.Subject, "�")
		m = &valid
	}

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

	if kind == Notification {
		fields.unsubscribeURL = baseURL + "/?Action=Unsubscribe&SubscriptionArn=" + m.SubscriptionARN
	} else {
		fields.subscribeURL = baseURL + "/?Action=ConfirmSubscription&TopicArn=" + m.TopicARN + "&Token=" + m.Token
	}

	if !fields.fifo {
		fields.version = m.SignatureVersion
		if fields.version == "" {
			fields.version = "1"
		}

		sig, err := s.sign(canonical(m, &fields), fields.version)
		if err != nil {
			return nil, err
		}

		fields.signature = base64.StdEncoding.EncodeToString(sig)
		fields.certURL = baseURL + CertPath
	}

	return appendEnvelope(make([]byte, 0, sizeHint(m, &fields)), m, &fields), nil
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
		b = appendMember(b, "SequenceNumber", m.SequenceNumber)
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

		if strings.HasPrefix(a.DataType, "Binary") {
			b = append(b, '"')
			b = base64.StdEncoding.AppendEncode(b, a.BinaryValue)
			b = append(b, '"')
		} else {
			b = appendString(b, a.StringValue)
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
		n += perAttrib + len(a.Name) + len(a.DataType) + len(a.StringValue) +
			base64.StdEncoding.EncodedLen(len(a.BinaryValue))
	}

	return n
}
