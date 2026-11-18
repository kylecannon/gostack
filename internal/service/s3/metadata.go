package s3

import (
	"encoding/base64"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/awserr"
)

const (
	// maxUserMetadataBytes is the limit on the user-defined metadata of one
	// object, the sum of every key and value in bytes of UTF-8.
	maxUserMetadataBytes = 2048

	// rfc2047SafeChars is what AWS leaves unencoded in a metadata value besides
	// the space, the underscore, "=" and "?" (headers.py _SAFE_HEADERS_CHARS).
	rfc2047SafeChars = "!\"#$%&'()*+,-./0123456789:;<>@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\\]^`abcdefghijklmnopqrstuvwxyz{|}~\t"

	// encodedWordFrame is the length of "=?UTF-8?B?" and "?=" together, plus the
	// one LocalStack counts when it replaces a badly encoded value.
	encodedWordFrame = 13
)

// storageClasses are the classes a write may name: the spec's StorageClass
// enum without OUTPOSTS, which only an Outposts bucket takes. LocalStack checks
// PutObject, CreateMultipartUpload and PostObject against the same set
// (provider.py:739, 2317, 4546); its VALID_STORAGE_CLASSES is not used there.
var storageClasses = map[string]bool{
	"STANDARD": true, "REDUCED_REDUNDANCY": true, "STANDARD_IA": true, "ONEZONE_IA": true,
	"INTELLIGENT_TIERING": true, "GLACIER": true, "DEEP_ARCHIVE": true, "GLACIER_IR": true,
	"SNOW": true, "EXPRESS_ONEZONE": true, "FSX_OPENZFS": true, "FSX_ONTAP": true,
}

// isArchived reports whether a class holds its objects out of reach until they
// are restored (ARCHIVES_STORAGE_CLASSES).
func isArchived(class string) bool {
	return class == "GLACIER" || class == "DEEP_ARCHIVE"
}

// decodeUserMetadata lower-cases the keys of user metadata and decodes values
// that arrive RFC 2047 encoded, as S3 stores them.
func decodeUserMetadata(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}

	out := make(map[string]string, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = decodeHeaderRFC2047(v)
	}

	return out
}

// encodeUserMetadata encodes values that HTTP headers cannot carry.
func encodeUserMetadata(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}

	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = encodeHeaderRFC2047(v)
	}

	return out
}

// decodeHeaderRFC2047 decodes the encoded words of a header value. A UTF-8 base64
// word that does not decode gives one replacement character per character, as
// LocalStack does where AWS returns undecodable data.
func decodeHeaderRFC2047(header string) string {
	if !strings.Contains(header, "=?") {
		return header
	}

	if lower := strings.ToLower(header); strings.HasPrefix(lower, "=?utf-8?b?") && strings.HasSuffix(header, "?=") {
		payload := header[len("=?utf-8?b?") : len(header)-2]

		_, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return strings.Repeat("�", max(len(header)-encodedWordFrame, 0))
		}
	}

	decoded, err := new(mime.WordDecoder).DecodeHeader(header)
	if err != nil {
		return header
	}

	return decoded
}

// encodeHeaderRFC2047 encodes a metadata value the way S3 sends it: as it is
// when every byte is safe, else as one UTF-8 word, quoted-printable unless the
// value holds control or replacement characters, which need base64.
func encodeHeaderRFC2047(value string) string {
	plain := true

	for i := range len(value) {
		if c := value[i]; !strings.ContainsRune(rfc2047SafeChars, rune(c)) && c != ' ' && c != '_' && c != '=' && c != '?' {
			plain = false

			break
		}
	}

	if plain {
		return value
	}

	if needsBase64(value) {
		return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(value)) + "?="
	}

	var sb strings.Builder

	sb.WriteString("=?UTF-8?Q?")

	for i := range len(value) {
		switch c := value[i]; {
		case c == ' ':
			sb.WriteByte('_')
		case c < utf8.RuneSelf && strings.ContainsRune(rfc2047SafeChars, rune(c)):
			sb.WriteByte(c)
		default:
			sb.WriteByte('=')
			sb.WriteString(strings.ToUpper(strconv.FormatInt(int64(c)+0x100, 16)[1:]))
		}
	}

	sb.WriteString("?=")

	return sb.String()
}

// needsBase64 reports whether value holds a replacement character or one in a
// Unicode "C" category (control, format, unassigned, private use).
func needsBase64(value string) bool {
	for _, r := range value {
		if r == utf8.RuneError || unicode.In(r, unicode.C) {
			return true
		}
	}

	return false
}

// checkMetadataSize refuses user metadata over S3's 2 KB limit.
func checkMetadataSize(m map[string]string) error {
	size := 0
	for k, v := range m {
		size += len(k) + len(v)
	}

	if size <= maxUserMetadataBytes {
		return nil
	}

	return &awserr.Error{
		Code: "MetadataTooLarge",
		Message: "Your metadata headers exceed the maximum allowed metadata size. Maximum is " +
			strconv.Itoa(maxUserMetadataBytes) + " bytes, proposed " + strconv.Itoa(size) + " bytes.",
		StatusCode: http.StatusBadRequest,
		Fault:      awserr.FaultReceiver,
	}
}

// checkWebsiteRedirect holds x-amz-website-redirect-location to the forms S3
// accepts: a path from the bucket root or an http(s) URL.
func checkWebsiteRedirect(location string) error {
	if location == "" || strings.HasPrefix(location, "/") ||
		strings.HasPrefix(location, "http://") || strings.HasPrefix(location, "https://") {
		return nil
	}

	return invalidArgument(
		"The website redirect location must have a prefix of 'http://' or 'https://' or '/'.",
		"x-amz-website-redirect-location", &location)
}

// checkStorageClass refuses a class S3 does not know.
func checkStorageClass(class *string) error {
	if class == nil || storageClasses[*class] {
		return nil
	}

	return (&s3api.InvalidStorageClass{StorageClassRequested: class}).Err("The storage class you specified is not valid")
}

// checkLatin1 refuses a header value that cannot go out in ISO-8859-1.
func checkLatin1(argument, value string) error {
	for _, r := range value {
		if r > unicode.MaxLatin1 {
			return invalidArgument("Header value cannot be represented using ISO-8859-1.", argument, &value)
		}
	}

	return nil
}
