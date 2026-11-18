package rest

import (
	"fmt"
	"time"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/query"
)

// Timestamp formats, named as in the botocore timestampFormat trait, plus the
// S3 form.
const (
	// TimeISO8601 is "2006-01-02T15:04:05Z" with six fractional digits when the
	// time has a microsecond part.
	TimeISO8601 = query.TimeISO8601
	// TimeUnix is epoch seconds.
	TimeUnix = query.TimeUnix
	// TimeRFC822 is the HTTP date, "Mon, 02 Jan 2006 15:04:05 GMT".
	TimeRFC822 = query.TimeRFC822
	// TimeS3 is the iso8601 form LocalStack's S3 serializer writes: whole
	// seconds and a literal ".000Z". It reads as TimeISO8601.
	TimeS3 = "s3iso8601"
)

const s3Layout = "2006-01-02T15:04:05.000Z"

// FormatTime returns t in format, or in def when format is empty.
func FormatTime(t *protocol.Time, format, def string) string {
	if format == "" {
		format = def
	}

	if format == TimeS3 {
		return t.UTC().Truncate(time.Second).Format(s3Layout)
	}

	return query.FormatTime(t, format)
}

// ParseTime reads a timestamp written in format, or in def when format is
// empty.
func ParseTime(s, format, def string) (*protocol.Time, error) {
	if format == "" {
		format = def
	}

	if format == TimeS3 {
		format = TimeISO8601
	}

	t, err := query.ParseTime(s, format)
	if err != nil {
		return nil, fmt.Errorf("rest timestamp: %w", err)
	}

	return t, nil
}
