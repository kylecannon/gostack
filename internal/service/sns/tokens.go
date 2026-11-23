package sns

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"unicode/utf8"

	api "github.com/kylecannon/gostack/internal/api/sns"
)

const (
	// tokenLength is the number of hex digits of a confirmation token.
	tokenLength = 64
	// tokenRegionEnd ends the region at the start of a token: "/" in hex.
	tokenRegionEnd = "2f"

	// The version and variant bits of a version 4 UUID, RFC 9562.
	uuidVersionKeep = 0x0f
	uuidVersion4    = 0x40
	uuidVariantKeep = 0x3f
	uuidVariant     = 0x80
)

// newToken is LocalStack's encode_subscription_token_with_region: 64 hex
// digits that start with the region and a "/", in hex, and go on at random.
func newToken(region string) string {
	var random [tokenLength / 2]byte

	_, _ = rand.Read(random[:])

	token := hex.EncodeToString([]byte(region+"/")) + hex.EncodeToString(random[:])

	return token[:tokenLength]
}

// regionOfToken is LocalStack's get_region_from_subscription_token: the hex
// before the first "2f", decoded. A token of hex digits that does not decode
// is "Invalid parameter: Token". A token with anything but hex digits is
// "Invalid token", as AWS answers "randomtoken" in the aws.validated snapshot
// of test_sns_confirm_subscription_wrong_token; LocalStack reads it as the
// first.
func regionOfToken(token string) (string, error) {
	if strings.ContainsFunc(token, func(r rune) bool { return !isHexDigit(r) }) {
		return "", api.NewInvalidParameterException("Invalid token")
	}

	head, _, _ := strings.Cut(token, tokenRegionEnd)

	region, err := hex.DecodeString(head)
	if err != nil || !utf8.Valid(region) {
		return "", api.NewInvalidParameterException("Invalid parameter: Token")
	}

	return string(region), nil
}

// newUUID is a random version 4 UUID, as Python's uuid4 makes the ID at the
// end of a subscription ARN.
func newUUID() string {
	var id [16]byte

	// crypto/rand.Read never returns an error.
	_, _ = rand.Read(id[:])

	id[6] = id[6]&uuidVersionKeep | uuidVersion4
	id[8] = id[8]&uuidVariantKeep | uuidVariant

	h := hex.EncodeToString(id[:])

	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func isHexDigit(r rune) bool {
	return '0' <= r && r <= '9' || 'a' <= r && r <= 'f' || 'A' <= r && r <= 'F'
}
