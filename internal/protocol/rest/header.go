package rest

import (
	"encoding/base64"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/kylecannon/gostack/internal/protocol"
)

// The Set functions write a response member that lives in a header. A nil
// value, or an empty list or map, writes nothing. Named headers go in by
// Header.Set, so code that reads them back by any spelling finds them.

// SetString writes a string header.
func SetString(h http.Header, name string, v *string) {
	if v != nil {
		h.Set(name, *v)
	}
}

// SetInt32 writes an integer header.
func SetInt32(h http.Header, name string, v *int32) {
	if v != nil {
		h.Set(name, strconv.FormatInt(int64(*v), decimalBase))
	}
}

// SetInt64 writes a long header.
func SetInt64(h http.Header, name string, v *int64) {
	if v != nil {
		h.Set(name, strconv.FormatInt(*v, decimalBase))
	}
}

// SetFloat64 writes a floating-point header. NaN and the infinities are
// written as "NaN", "Infinity" and "-Infinity".
func SetFloat64(h http.Header, name string, v *float64) {
	if v != nil {
		h.Set(name, formatFloat(*v))
	}
}

// SetBool writes "true" or "false".
func SetBool(h http.Header, name string, v *bool) {
	if v != nil {
		h.Set(name, strconv.FormatBool(*v))
	}
}

// SetTime writes a timestamp header in format, rfc822 when empty.
func SetTime(h http.Header, name string, v *protocol.Time, format string) {
	if v != nil {
		h.Set(name, FormatTime(v, format, TimeRFC822))
	}
}

// SetBlob writes a blob header in base64.
func SetBlob(h http.Header, name string, v []byte) {
	if v != nil {
		h.Set(name, base64.StdEncoding.EncodeToString(v))
	}
}

// SetJSONValue writes the json text of a jsonvalue member in base64.
func SetJSONValue(h http.Header, name string, v *string) {
	if v != nil {
		h.Set(name, base64.StdEncoding.EncodeToString([]byte(*v)))
	}
}

// SetStrings writes a list header, its items joined by commas. An item that
// holds a comma or a double quote is quoted, its quotes and backslashes
// escaped, so HeaderStrings reads the same items back.
func SetStrings(h http.Header, name string, v []string) {
	setList(h, name, v, func(b []byte, s string) []byte {
		if !strings.ContainsAny(s, `,"`) {
			return append(b, s...)
		}

		b = append(b, '"')

		for i := range len(s) {
			if s[i] == '"' || s[i] == '\\' {
				b = append(b, '\\')
			}

			b = append(b, s[i])
		}

		return append(b, '"')
	})
}

// SetInt32s writes a list header of integers.
func SetInt32s(h http.Header, name string, v []int32) {
	setList(h, name, v, func(b []byte, n int32) []byte { return strconv.AppendInt(b, int64(n), decimalBase) })
}

// SetInt64s writes a list header of longs.
func SetInt64s(h http.Header, name string, v []int64) {
	setList(h, name, v, func(b []byte, n int64) []byte { return strconv.AppendInt(b, n, decimalBase) })
}

// SetFloat64s writes a list header of floating-point numbers.
func SetFloat64s(h http.Header, name string, v []float64) {
	setList(h, name, v, func(b []byte, f float64) []byte { return append(b, formatFloat(f)...) })
}

// SetBools writes a list header of bools.
func SetBools(h http.Header, name string, v []bool) {
	setList(h, name, v, strconv.AppendBool)
}

// SetTimes writes a list header of timestamps in format, rfc822 when empty.
func SetTimes(h http.Header, name string, v []*protocol.Time, format string) {
	setList(h, name, v, func(b []byte, t *protocol.Time) []byte {
		return append(b, FormatTime(t, format, TimeRFC822)...)
	})
}

// SetMap writes one header per entry, named prefix plus the key. The name is
// assigned to the header map as it is, not canonicalized, so the key keeps
// the case it was stored in, as S3 user metadata does.
func SetMap(h http.Header, prefix string, m map[string]string) {
	for k, v := range m {
		h[prefix+k] = []string{v}
	}
}

// Status returns the status a statusCode member asks for, or def when the
// member is nil.
func Status(v *int32, def int) int {
	if v == nil {
		return def
	}

	return int(*v)
}

func setList[T any](h http.Header, name string, v []T, appendItem func([]byte, T) []byte) {
	if len(v) == 0 {
		return
	}

	var b []byte

	for i, item := range v {
		if i > 0 {
			b = append(b, ", "...)
		}

		b = appendItem(b, item)
	}

	h.Set(name, string(b))
}

func formatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	default:
		return strconv.FormatFloat(f, 'g', -1, int64Bits)
	}
}
