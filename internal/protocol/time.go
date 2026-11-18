package protocol

import (
	"bytes"
	"encoding/json/jsontext"
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

		sec = -sec
	}

	buf = strconv.AppendInt(buf, sec, decimalBase)

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

// epochTime reads epoch seconds written as an integer or a decimal.
func epochTime(s string) (time.Time, error) {
	sec, nsec, err := parseEpoch(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("unmarshal protocol.Time: %w", err)
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
