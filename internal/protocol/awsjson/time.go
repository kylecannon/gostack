package awsjson

import (
	"encoding/json/jsontext"
	"fmt"
	"net/http"

	"github.com/kylecannon/gostack/internal/protocol"
)

// protocol.Time is epoch seconds in json, which is the json protocol's default.
// A shape with a timestampFormat trait asks for another form. The generated
// code swaps the field for one of these types while it encodes or decodes the
// struct that holds it.
//
// Both types implement only the encoding/json/v2 interfaces. They embed
// time.Time, so encoding/json (v1) would use time.Time's own MarshalJSON.

// ISO8601 is a [protocol.Time] that json writes as an ISO 8601 string.
type ISO8601 protocol.Time

// RFC822 is a [protocol.Time] that json writes as an HTTP date.
type RFC822 protocol.Time

// MarshalJSONTo writes the time as a quoted ISO 8601 string.
func (t *ISO8601) MarshalJSONTo(enc *jsontext.Encoder) error {
	return writeString(enc, (*protocol.Time)(t).FormatISO8601())
}

// UnmarshalJSONFrom reads a quoted ISO 8601 string. A json null leaves the
// time unchanged.
func (t *ISO8601) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	s, null, err := readString(dec)
	if err != nil || null {
		return err
	}

	err = (*protocol.Time)(t).ParseISO8601(s)
	if err != nil {
		return fmt.Errorf("parse iso8601 timestamp: %w", err)
	}

	return nil
}

// MarshalJSONTo writes the time as a quoted HTTP date.
func (t *RFC822) MarshalJSONTo(enc *jsontext.Encoder) error {
	return writeString(enc, t.UTC().Format(http.TimeFormat))
}

// UnmarshalJSONFrom reads a quoted HTTP date. A json null leaves the time
// unchanged.
func (t *RFC822) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	s, null, err := readString(dec)
	if err != nil || null {
		return err
	}

	parsed, err := http.ParseTime(s)
	if err != nil {
		return fmt.Errorf("parse rfc822 timestamp: %w", err)
	}

	t.Time = parsed

	return nil
}

func writeString(enc *jsontext.Encoder, s string) error {
	err := enc.WriteToken(jsontext.String(s))
	if err != nil {
		return fmt.Errorf("write timestamp: %w", err)
	}

	return nil
}

func readString(dec *jsontext.Decoder) (string, bool, error) {
	tok, err := dec.ReadToken()
	if err != nil {
		return "", false, fmt.Errorf("read timestamp: %w", err)
	}

	switch tok.Kind() {
	case 'n':
		return "", true, nil
	case '"':
		return tok.String(), false, nil
	default:
		return "", false, fmt.Errorf("timestamp is not a string: json %v", tok.Kind())
	}
}
