package protocol

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
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

	// maxEpoch is the last epoch second a time.Time holds. A time.Time counts
	// seconds from year 1, 62,135,596,800 before the epoch, in an int64, so
	// time.Unix wraps a later second round to a date in the past.
	maxEpoch = math.MaxInt64 - 62_135_596_800
	// epochFloatLimit is 2^63: a float64 at or past it, either way, has no
	// int64.
	epochFloatLimit = 1 << 63
)

// Time is a timestamp. In json it is epoch seconds as a number; in query and
// xml it is ISO 8601.
type Time struct{ time.Time }

// MarshalJSON writes the time as epoch seconds: an integer when the seconds
// are whole, otherwise a decimal with the fraction kept exactly. The query
// codec uses it for its unix timestamps. encoding/json/v2 calls MarshalJSONTo
// instead, which does not build the []byte.
//
// Time embeds time.Time, so without these methods a caller would get
// time.Time's own, an RFC 3339 string, and no compile error.
//
//nolint:revive // marshal-receiver wants a value receiver, and recvcheck wants one kind of receiver on Time
func (t *Time) MarshalJSON() ([]byte, error) {
	return appendEpoch(nil, t.Time), nil
}

// MarshalJSONTo is MarshalJSON for encoding/json/v2, written straight into
// the encoder.
func (t *Time) MarshalJSONTo(enc *jsontext.Encoder) error {
	var arr [32]byte

	err := enc.WriteValue(appendEpoch(arr[:0], t.Time))
	if err != nil {
		return fmt.Errorf("marshal protocol.Time: %w", err)
	}

	return nil
}

// appendEpoch appends t as epoch seconds.
func appendEpoch(buf []byte, t time.Time) []byte {
	sec, nsec := t.Unix(), int64(t.Nanosecond())

	if sec < 0 {
		buf = append(buf, '-')

		// time.Unix floors, so a negative time with a fraction is the next
		// second up minus the complement of the fraction.
		if nsec > 0 {
			sec++
			nsec = nanosPerSecond - nsec
		}
	}

	// The magnitude as unsigned: negating math.MinInt64 as an int64 overflows.
	magnitude := uint64(sec) //nolint:gosec // the bits of an int64, negated below when it is negative
	if sec < 0 {
		magnitude = -magnitude
	}

	buf = strconv.AppendUint(buf, magnitude, decimalBase)

	if nsec > 0 {
		var frac [nanosDigits]byte // zero-padded, then trimmed of trailing zeros

		for i := nanosDigits - 1; i >= 0; i-- {
			frac[i] = byte('0' + nsec%decimalBase)
			nsec /= decimalBase
		}

		buf = append(buf, '.')
		buf = append(buf, bytes.TrimRight(frac[:], "0")...)
	}

	return buf
}

// UnmarshalJSON reads epoch seconds written as an integer or a decimal. A json
// null leaves the time unchanged.
func (t *Time) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if string(data) == "null" {
		return nil
	}

	parsed, err := epochTime(string(data))
	if err != nil {
		return err
	}

	t.Time = parsed

	return nil
}

// UnmarshalJSONFrom is UnmarshalJSON for encoding/json/v2, read straight from
// the decoder.
func (t *Time) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	if dec.PeekKind() == 'n' {
		_, err := dec.ReadToken()
		if err != nil {
			return fmt.Errorf("unmarshal protocol.Time: %w", err)
		}

		return nil
	}

	v, err := dec.ReadValue()
	if err != nil {
		return fmt.Errorf("unmarshal protocol.Time: %w", err)
	}

	parsed, err := epochTime(string(v))
	if err != nil {
		return err
	}

	t.Time = parsed

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

// The Unix seconds of the first and the last moment RFC 3339 can write, in the
// zone the time is written in: 0000-01-01T00:00:00 and 9999-12-31T23:59:59.
// A zone offset stays under secondsPerDay.
const (
	minRFC3339Local = -62167219200
	maxRFC3339Local = 253402300799
	secondsPerDay   = 24 * 60 * 60
	secondsPerMin   = 60
)

// SnapshotTime is a time.Time that a snapshot can hold whatever it is. A time
// RFC 3339 can write is written as encoding/json/v2 writes a time.Time, so a
// snapshot reads as it did. That is a year from 0 through 9999 in the time's
// own zone and an offset of whole minutes under a day. Any other is epoch
// seconds: encoding/json/v2 refuses a year outside that range and an offset of
// a day or more, which would fail the whole save, and writes an offset with
// seconds without them, so the time reads back off by that much. Reading takes
// both forms; a time read from epoch seconds is in UTC.
//
// A snapshot field that can hold any time has this type, so only that field
// pays for the choice, and not every value the snapshot writes.
//
//nolint:recvcheck // marshal takes a value, so json can write one it cannot address, and unmarshal a pointer
type SnapshotTime struct{ time.Time }

// MarshalJSONTo writes the time as RFC 3339 when RFC 3339 can hold it, and as
// epoch seconds otherwise.
func (t SnapshotTime) MarshalJSONTo(enc *jsontext.Encoder) error {
	if !rfc3339Holds(t.Time) {
		return (&Time{Time: t.Time}).MarshalJSONTo(enc)
	}

	// The longest RFC 3339 here, with nanoseconds and an offset, is 35 bytes.
	var arr [40]byte

	buf := append(arr[:0], '"')
	buf = t.AppendFormat(buf, time.RFC3339Nano)
	buf = append(buf, '"')

	err := enc.WriteValue(buf)
	if err != nil {
		return fmt.Errorf("marshal protocol.SnapshotTime: %w", err)
	}

	return nil
}

// UnmarshalJSONFrom reads epoch seconds when the value is a number, and what
// encoding/json/v2 reads for a time.Time otherwise.
func (t *SnapshotTime) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	if dec.PeekKind() != '0' {
		return json.UnmarshalDecode(dec, &t.Time) //nolint:wrapcheck // json wraps it
	}

	var epoch Time

	err := epoch.UnmarshalJSONFrom(dec)
	if err != nil {
		return err
	}

	t.Time = epoch.UTC()

	return nil
}

// rfc3339Holds reports whether encoding/json/v2 writes t as RFC 3339 and reads
// the same instant back. It compares seconds, not t.Year(), so it holds at
// either end of the int64 range.
func rfc3339Holds(t time.Time) bool {
	_, offset := t.Zone()
	sec := t.Unix()

	// A day either side of the range keeps sec+offset from overflowing.
	return offset > -secondsPerDay && offset < secondsPerDay && offset%secondsPerMin == 0 &&
		sec >= minRFC3339Local-secondsPerDay && sec <= maxRFC3339Local+secondsPerDay &&
		sec+int64(offset) >= minRFC3339Local && sec+int64(offset) <= maxRFC3339Local
}

// epochTime reads epoch seconds written as an integer or a decimal. A second
// past the last one a time.Time holds is refused, where time.Unix would wrap
// it to a date in the past.
func epochTime(s string) (time.Time, error) {
	sec, nsec, err := parseEpoch(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("unmarshal protocol.Time: %w", err)
	}

	if sec > maxEpoch {
		return time.Time{}, errors.New("unmarshal protocol.Time: epoch seconds: past the greatest time")
	}

	return time.Unix(sec, nsec), nil
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

		// Go leaves the conversion of a float64 outside int64 to the platform:
		// arm64 saturates, amd64 gives the least int64.
		if f >= epochFloatLimit || f < -epochFloatLimit {
			return 0, 0, errors.New("epoch seconds: past int64")
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
		if sec == math.MinInt64 {
			return 0, 0, errors.New("epoch seconds: below the least time")
		}

		// "-1.5" is -1s minus 0.5s, which time.Unix expresses as -2s plus 0.5s.
		return sec - 1, nanosPerSecond - nsec, nil
	}

	return sec, nsec, nil
}
