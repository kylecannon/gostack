package sigv4

import (
	"cmp"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
	"strings"
)

// Algorithm is the signing algorithm SigV4 names in X-Amz-Algorithm and in the
// first line of the string to sign.
const Algorithm = "AWS4-HMAC-SHA256"

// UnsignedPayload is the payload hash of a request that does not sign its body.
const UnsignedPayload = "UNSIGNED-PAYLOAD"

// terminator ends a credential scope.
const terminator = "aws4_request"

// Pair is one query parameter, decoded.
type Pair struct {
	Key   string
	Value string
}

// Header is one header line. Name may be in any case.
type Header struct {
	Name  string
	Value string
}

// Escape percent-encodes s the way botocore's quote(s, safe='-_.~') does:
// every byte other than an ASCII letter, a digit, '-', '_', '.' and '~' becomes
// %XX in upper-case hex. It encodes a query key or value.
func Escape(s string) string {
	return percentEncode(s, unreserved)
}

// EscapePath is [Escape] that leaves '/' as it is, for a URI path.
func EscapePath(s string) string {
	return percentEncode(s, unreservedInPath)
}

// CanonicalQuery encodes each key and value with [Escape], sorts the pairs by
// encoded key and then encoded value, and joins them as k=v with '&'.
func CanonicalQuery(params []Pair) string {
	encoded := make([]Pair, 0, len(params))
	for _, p := range params {
		encoded = append(encoded, Pair{Key: Escape(p.Key), Value: Escape(p.Value)})
	}

	slices.SortFunc(encoded, func(a, b Pair) int {
		return cmp.Or(strings.Compare(a.Key, b.Key), strings.Compare(a.Value, b.Value))
	})

	var b strings.Builder
	for i, p := range encoded {
		if i > 0 {
			b.WriteByte('&')
		}

		b.WriteString(p.Key)
		b.WriteByte('=')
		b.WriteString(p.Value)
	}

	return b.String()
}

// CanonicalHeaders returns the canonical header block and the signed headers
// list of headers. Names are lower-cased and sorted; the values of one name
// are trimmed, their inner whitespace collapsed to one space, and joined with
// ',' in the order given. Each line ends with '\n', so the block ends with one.
func CanonicalHeaders(headers []Header) (block, signed string) {
	values := make(map[string][]string, len(headers))
	for _, h := range headers {
		name := strings.ToLower(h.Name)
		values[name] = append(values[name], strings.Join(strings.Fields(h.Value), " "))
	}

	names := slices.Sorted(maps.Keys(values))

	var b strings.Builder
	for _, name := range names {
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(strings.Join(values[name], ","))
		b.WriteByte('\n')
	}

	return b.String(), strings.Join(names, ";")
}

// CanonicalRequest joins the parts of a canonical request. path is already
// encoded, query is a [CanonicalQuery], and headers and signed come from
// [CanonicalHeaders].
func CanonicalRequest(method, path, query, headers, signed, payloadHash string) string {
	return strings.Join([]string{strings.ToUpper(method), path, query, headers, signed, payloadHash}, "\n")
}

// Scope returns the credential scope date/region/service/aws4_request. date is
// the YYYYMMDD day of the request.
func Scope(date, region, service string) string {
	return date + "/" + region + "/" + service + "/" + terminator
}

// StringToSign returns the string to sign of a canonical request. amzDate is
// the request's X-Amz-Date and scope its [Scope].
func StringToSign(amzDate, scope, canonicalRequest string) string {
	sum := sha256.Sum256([]byte(canonicalRequest))

	return Algorithm + "\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
}

// SigningKey derives the key that signs requests for one day, region and
// service from a secret access key.
func SigningKey(secret, date, region, service string) []byte {
	key := mac([]byte("AWS4"+secret), date)
	key = mac(key, region)
	key = mac(key, service)

	return mac(key, terminator)
}

// Signature returns the hex signature of stringToSign under key.
func Signature(key []byte, stringToSign string) string {
	return hex.EncodeToString(mac(key, stringToSign))
}

// Equal reports whether two signatures are the same, in constant time.
func Equal(a, b string) bool {
	return hmac.Equal([]byte(a), []byte(b))
}

func mac(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))

	return h.Sum(nil)
}

// percentEncode encodes every byte of s that safe rejects as %XX.
func percentEncode(s string, safe func(byte) bool) string {
	n := 0
	for i := range len(s) {
		if !safe(s[i]) {
			n++
		}
	}
	if n == 0 {
		return s
	}

	const hexDigits = "0123456789ABCDEF"

	var b strings.Builder
	b.Grow(len(s) + 2*n)

	for i := range len(s) {
		c := s[i]
		if safe(c) {
			b.WriteByte(c)

			continue
		}

		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0xf])
	}

	return b.String()
}

func unreserved(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	default:
		return c == '-' || c == '_' || c == '.' || c == '~'
	}
}

func unreservedInPath(c byte) bool {
	return c == '/' || unreserved(c)
}
