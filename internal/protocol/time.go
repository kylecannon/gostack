package protocol

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// iso8601 and iso8601Micro match botocore's ISO8601 and ISO8601_MICRO, which
	// LocalStack's query serializer picks between by whether the microsecond
	// part is zero.
	iso8601      = "2006-01-02T15:04:05Z"
	iso8601Micro = "2006-01-02T15:04:05.000000Z"

	nanosPerSecond = 1_000_000_000
	nanosPerMicro  = 1_000
	nanosDigits    = 9
	decimalBase    = 10
)

// Time is a timestamp. In json it is epoch seconds as a number; in query and
// xml it is ISO 8601.
type Time struct{ time.Time }

// MarshalJSON writes the time as epoch seconds: an integer when the seconds
// are whole, otherwise a decimal with the fraction kept exactly.
func (t *Time) MarshalJSON() ([]byte, error) {
	sec, nsec := t.Unix(), int64(t.Nanosecond())

	var buf []byte

	if sec < 0 {
		buf = append(buf, '-')

		// time.Unix floors, so a negative time with a fraction is the next
		// second up minus the complement of the fraction.
		if nsec > 0 {
			sec++
			nsec = nanosPerSecond - nsec
		}

		sec = -sec
	}

	buf = strconv.AppendInt(buf, sec, decimalBase)

	if nsec > 0 {
		frac := strconv.FormatInt(nsec+nanosPerSecond, decimalBase)[1:] // zero-padded to nanosDigits
		buf = append(buf, '.')
		buf = append(buf, strings.TrimRight(frac, "0")...)
	}

	return buf, nil
}

// UnmarshalJSON reads epoch seconds written as an integer or a decimal. A json
// null leaves the time unchanged.
func (t *Time) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if string(data) == "null" {
		return nil
	}

	sec, nsec, err := parseEpoch(string(data))
	if err != nil {
		return fmt.Errorf("unmarshal protocol.Time: %w", err)
	}

	t.Time = time.Unix(sec, nsec)

	return nil
}

// FormatISO8601 formats the time in UTC the way LocalStack's query serializer
// does: "2006-01-02T15:04:05Z", with six fractional digits when the time has a
// microsecond part.
func (t *Time) FormatISO8601() string {
	u := t.UTC()
	if u.Nanosecond()/nanosPerMicro > 0 {
		return u.Format(iso8601Micro)
	}

	return u.Format(iso8601)
}

// ParseISO8601 sets the time from an ISO 8601 timestamp with a zone, such as
// "2006-01-02T15:04:05Z" or "2006-01-02T15:04:05.123456+01:00".
func (t *Time) ParseISO8601(s string) error {
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return fmt.Errorf("parse protocol.Time: %w", err)
	}

	t.Time = parsed

	return nil
}

// parseEpoch splits a json number into seconds and nanoseconds. A plain
// integer or decimal is read digit by digit so the fraction survives exactly;
// anything with an exponent goes through float64.
func parseEpoch(s string) (int64, int64, error) {
	if strings.ContainsAny(s, "eE") {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("epoch seconds: %w", err)
		}

		sec := int64(f)

		return sec, int64((f - float64(sec)) * nanosPerSecond), nil
	}

	intPart, fracPart, hasFrac := strings.Cut(s, ".")

	sec, err := strconv.ParseInt(intPart, decimalBase, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("epoch seconds: %w", err)
	}

	if !hasFrac {
		return sec, 0, nil
	}

	if fracPart == "" || strings.TrimLeft(fracPart, "0123456789") != "" {
		return 0, 0, errors.New("epoch seconds: malformed fraction")
	}

	// Digits past nanoseconds are dropped; fewer are padded.
	fracPart = (fracPart + strings.Repeat("0", nanosDigits))[:nanosDigits]

	nsec, err := strconv.ParseInt(fracPart, decimalBase, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("epoch seconds: %w", err)
	}

	if nsec > 0 && strings.HasPrefix(intPart, "-") {
		// "-1.5" is -1s minus 0.5s, which time.Unix expresses as -2s plus 0.5s.
		return sec - 1, nanosPerSecond - nsec, nil
	}

	return sec, nsec, nil
}
