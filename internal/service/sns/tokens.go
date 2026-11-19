package sns

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	api "github.com/kylecannon/gostack/internal/api/sns"
)

const (
	// tokenLength is the number of hex digits of a confirmation token.
	tokenLength = 64
	// tokenRegionEnd ends the region at the start of a token: "/" in hex.
	tokenRegionEnd = "2f"

	// developerCanonicalPrefix is where the developer endpoints always live.
	developerCanonicalPrefix = "/_gostack/services"
	// developerCompatPrefix is LocalStack's name for them, mounted unless
	// Settings.DisableCompatPaths is set.
	developerCompatPrefix = "/_aws"
	// subscriptionTokensPath is LocalStack's SUBSCRIPTION_TOKENS_ENDPOINT
	// under either prefix. The subscription ARN follows it.
	subscriptionTokensPath = "/sns/subscription-tokens"
	// keySubscriptionARN names the ARN in every reply of the endpoint.
	keySubscriptionARN = "subscription_arn"

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

// mountDeveloperAPI adds the developer routes: always under /_gostack/services,
// and under /_aws too unless the settings turn the LocalStack names off. It is
// called when the router is built, so the setting is read once.
func (p *Provider) mountDeveloperAPI(r chi.Router) {
	prefixes := []string{developerCanonicalPrefix}
	if !p.settings().DisableCompatPaths {
		prefixes = append(prefixes, developerCompatPrefix)
	}

	for _, prefix := range prefixes {
		r.Get(prefix+subscriptionTokensPath+"/*", p.subscriptionToken)
	}
}

// subscriptionToken is LocalStack's SNSServiceSubscriptionTokenApiResource: the
// confirmation token of the subscription the rest of the path names, so a
// test can confirm a subscription whose confirmation message it cannot
// receive.
func (p *Provider) subscriptionToken(w http.ResponseWriter, r *http.Request) {
	arn := chi.URLParam(r, "*")

	parts, ok := parseARN(arn)
	if !ok {
		writeTokenReply(w, http.StatusBadRequest, map[string]string{
			"error": "The provided SubscriptionARN is invalid", keySubscriptionARN: arn,
		})

		return
	}

	st := p.stateAt(parts.account, parts.region)

	st.mu.RLock()
	sub, ok := st.subscriptions[arn]
	st.mu.RUnlock()

	if !ok {
		writeTokenReply(w, http.StatusNotFound, map[string]string{
			"error": "The provided SubscriptionARN is not found", keySubscriptionARN: arn,
		})

		return
	}

	writeTokenReply(w, http.StatusOK, map[string]string{"subscription_token": sub.Token, keySubscriptionARN: arn})
}

func writeTokenReply(w http.ResponseWriter, status int, body map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// A map of strings always encodes, so an error is a write to a client that
	// is gone, and there is no one left to tell.
	err := json.NewEncoder(w).Encode(body)
	if err != nil {
		return
	}
}
