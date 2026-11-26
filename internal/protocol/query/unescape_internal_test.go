package query

import (
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bufferedUnescape is unescape as it was before it decoded as it read: each
// run of ASCII characters decoded into a slice first, then that slice checked
// for UTF-8. unescape must give what it gives for every input.
func bufferedUnescape(s string) string {
	if !strings.ContainsAny(s, "%+") {
		return s
	}

	var out strings.Builder

	for len(s) > 0 {
		run := 0
		for run < len(s) && s[run] < asciiLimit {
			run++
		}

		if run == 0 {
			_, size := utf8.DecodeRuneInString(s)
			out.WriteString(s[:size])
			s = s[size:]

			continue
		}

		decoded := make([]byte, 0, run)

		for i := 0; i < run; i++ {
			switch c := s[i]; {
			case c == '+':
				decoded = append(decoded, ' ')
			case c == '%' && i+2 < run && isHex(s[i+1]) && isHex(s[i+2]):
				decoded = append(decoded, unhex(s[i+1])<<4|unhex(s[i+2]))
				i += 2
			default:
				decoded = append(decoded, c)
			}
		}

		for len(decoded) > 0 {
			r, size := utf8.DecodeRune(decoded)
			if r == utf8.RuneError && size == 1 {
				out.WriteByte('%')
				out.WriteByte(hexUpper[decoded[0]>>4])
				out.WriteByte(hexUpper[decoded[0]&0x0f])
			} else {
				out.Write(decoded[:size])
			}

			decoded = decoded[size:]
		}

		s = s[run:]
	}

	return out.String()
}

var unescapeSeeds = []string{
	"", "a", "+", "a+b", "%41", "%4", "%", "%%41", "%zz", "%e2%82%ac", "%E2%82",
	"%e2%82%ac%e2", "%ff", "%c3%a9+%c3", "%f0%9f%98%80", "%f0%9f%98", "é%41é",
	"%e2é%82%ac", "%ed%a0%80", "%c0%80", "%f4%90%80%80", "a%e2+%82%ac", "%41%",
	"%e2%82%ac%f0%9f%98%80%ff%41", "+%c3%a9%c3%a9+",
}

func Test_unescape_matchesTheBufferedDecoding(t *testing.T) {
	for _, s := range unescapeSeeds {
		assert.Equal(t, bufferedUnescape(s), unescape(s), "%q", s)
	}
}

func Fuzz_unescape_matchesTheBufferedDecoding(f *testing.F) {
	for _, s := range unescapeSeeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		if !utf8.ValidString(s) {
			return // a form's text is checked for UTF-8 before it is parsed
		}

		require.Equal(t, bufferedUnescape(s), unescape(s), "%q", s)
	})
}

// Test_unescape_OneCopy: a value that is all escapes allocates one buffer of
// its length, the copy the form's plan charges, and nothing more.
func Test_unescape_OneCopy(t *testing.T) {
	const size = 4 << 20

	values := []string{strings.Repeat("+", size), strings.Repeat("%41", size/3), strings.Repeat("%C3%A9", size/6)}

	for _, value := range values {
		var before, after runtime.MemStats

		runtime.ReadMemStats(&before)

		got := unescape(value)

		runtime.ReadMemStats(&after)

		require.NotEmpty(t, got)
		assert.LessOrEqual(t, after.TotalAlloc-before.TotalAlloc, uint64(len(value))+64<<10, "%.6q", value)
	}
}
