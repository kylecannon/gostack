package service

import (
	"context"
	"net/http"
)

type ownerKey struct{}

// Own marks r as belonging to the named service. A Preprocess calls it, and the
// gateway calls it for the service whose ClaimHost claimed the request, before
// that service's Preprocess runs; the gateway's later steps honor it. Route detection answers with the service
// before any other step, as LocalStack's ServiceNameParser keeps a service an
// earlier handler set (aws/handlers/service.py:38). Content decoding skips the
// request when the service's Registration sets RawBody, and CORS skips it when
// the Registration sets OwnCORS. A later Own replaces an earlier one.
//
// A host claim wins over a mark: gateway.preprocess marks a request that
// ClaimHost claimed for the claimant and hands it only to the claimant's own
// Preprocess, so another service's Own never reaches it.
func Own(r *http.Request, service string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ownerKey{}, service))
}

// OwnerOf returns the service a Preprocess marked r for, or "".
func OwnerOf(r *http.Request) string {
	owner, _ := r.Context().Value(ownerKey{}).(string)

	return owner
}
