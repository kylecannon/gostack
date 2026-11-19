// Package sigv4 builds the pieces of an AWS Signature Version 4 signature: the
// canonical query, headers and request, the string to sign, the signing key
// and the signature. It follows botocore's SigV4Auth byte for byte, so a
// service can recompute the signature a client sent and compare.
//
// The package only canonicalizes and signs. Which headers a request signs, how
// a service normalizes its path, and what it answers on a mismatch belong to
// the service that checks the signature.
package sigv4
