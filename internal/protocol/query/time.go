package query

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/kylecannon/gostack/internal/protocol"
)

// Timestamp formats, named as in the botocore timestampFormat trait. An empty
// format means TimeISO8601, the query default.
const (
	// TimeISO8601 is "2006-01-02T15:04:05Z" with a fraction when there is one.
	TimeISO8601 = "iso8601"
	// TimeUnix is epoch seconds.
	TimeUnix = "unixtimestamp"
	// TimeRFC822 is the HTTP date, "Mon, 02 Jan 2006 15:04:05 GMT".
	TimeRFC822 = "rfc822"
)

// FormatTime returns t in the given timestamp format.
func FormatTime(t *protocol.Time, format string) string {
	switch strings.ToLower(format) {
	case TimeUnix:
		b, err := t.MarshalJSON()
		if err != nil {
			return ""
		}

		return string(b)
	case TimeRFC822:
		return t.UTC().Format(http.TimeFormat)
	default:
		return t.FormatISO8601()
	}
}

// ParseTime reads a timestamp written in the given format.
func ParseTime(s, format string) (*protocol.Time, error) {
	t := &protocol.Time{}

	switch strings.ToLower(format) {
	case TimeUnix:
		err := t.UnmarshalJSON([]byte(s))
		if err != nil {
			return nil, fmt.Errorf("parse unix timestamp: %w", err)
		}
	case TimeRFC822:
		parsed, err := http.ParseTime(s)
		if err != nil {
			return nil, fmt.Errorf("parse rfc822 timestamp: %w", err)
		}

		t.Time = parsed
	default:
		if s == "" {
			return nil, errors.New("parse iso8601 timestamp: empty")
		}

		err := t.ParseISO8601(s)
		if err != nil {
			return nil, fmt.Errorf("parse iso8601 timestamp: %w", err)
		}
	}

	return t, nil
}
