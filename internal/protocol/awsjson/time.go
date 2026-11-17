package awsjson

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"

	"github.com/kylecannon/gostack/internal/protocol"
)

// protocol.Time is epoch seconds in json, which is the json protocol's default.
// A shape with a timestampFormat trait asks for another form. The generated
// code swaps the field for one of these types while it encodes or decodes the
// struct that holds it.

// ISO8601 is a [protocol.Time] that json writes as an ISO 8601 string.
type ISO8601 protocol.Time

// RFC822 is a [protocol.Time] that json writes as an HTTP date.
type RFC822 protocol.Time

// MarshalJSON writes the time as a quoted ISO 8601 string.
func (t *ISO8601) MarshalJSON() ([]byte, error) {
	return strconv.AppendQuote(nil, (*protocol.Time)(t).FormatISO8601()), nil
}

// UnmarshalJSON reads a quoted ISO 8601 string. A json null leaves the time
// unchanged.
func (t *ISO8601) UnmarshalJSON(data []byte) error {
	s, null, err := unquote(data)
	if err != nil || null {
		return err
	}

	err = (*protocol.Time)(t).ParseISO8601(s)
	if err != nil {
		return fmt.Errorf("parse iso8601 timestamp: %w", err)
	}

	return nil
}

// MarshalJSON writes the time as a quoted HTTP date.
func (t *RFC822) MarshalJSON() ([]byte, error) {
	return strconv.AppendQuote(nil, t.UTC().Format(http.TimeFormat)), nil
}

// UnmarshalJSON reads a quoted HTTP date. A json null leaves the time unchanged.
func (t *RFC822) UnmarshalJSON(data []byte) error {
	s, null, err := unquote(data)
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

func unquote(data []byte) (string, bool, error) {
	data = bytes.TrimSpace(data)
	if string(data) == "null" {
		return "", true, nil
	}

	s, err := strconv.Unquote(string(data))
	if err != nil {
		return "", false, fmt.Errorf("timestamp is not a string: %w", err)
	}

	return s, false, nil
}
