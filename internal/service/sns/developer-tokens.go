package sns

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

const (
	// subscriptionTokensPath is LocalStack's SUBSCRIPTION_TOKENS_ENDPOINT,
	// relative to the developer prefix. The subscription ARN follows it.
	subscriptionTokensPath = "/subscription-tokens"
	// keySubscriptionARN names the ARN in every reply of the endpoint.
	keySubscriptionARN = "subscription_arn"
)

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
