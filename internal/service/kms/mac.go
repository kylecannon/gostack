package kms

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"hash"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/protocol/constraint"
	"github.com/kylecannon/gostack/internal/request"
)

// maxMessageBytes is the largest Message GenerateMac, VerifyMac, Sign and
// Verify take (PlaintextType).
const maxMessageBytes = 4096

// macHashes are the digests of the MAC algorithms, one per HMAC key spec.
var macHashes = map[string]func() hash.Hash{
	"HMAC_SHA_224": sha256.New224,
	"HMAC_SHA_256": sha256.New,
	"HMAC_SHA_384": sha512.New384,
	"HMAC_SHA_512": sha512.New,
}

// GenerateMac answers the HMAC of Message under an HMAC key. Its checks run in
// LocalStack's order: the message's size, the key, the key's usage, the
// algorithm's name, then its match with the key's spec. DryRun and GrantTokens
// are not read, in LocalStack either.
func (p *Provider) GenerateMac(ctx context.Context, in *api.GenerateMacInput) (*api.GenerateMacOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	k, algorithm, err := p.macKey(rc, in.Message, in.KeyId, in.MacAlgorithm, "GenerateMac")
	if err != nil {
		return nil, err
	}

	return &api.GenerateMacOutput{
		Mac:          computeMAC(k.Material, algorithm, in.Message),
		MacAlgorithm: in.MacAlgorithm,
		KeyId:        new(k.ARN),
	}, nil
}

// VerifyMac checks Mac against the HMAC of Message. A Mac that does not match
// is KMSInvalidMacException, as in LocalStack and AWS; a match answers
// MacValid true, never false.
func (p *Provider) VerifyMac(ctx context.Context, in *api.VerifyMacInput) (*api.VerifyMacOutput, error) {
	rc, err := call(ctx)
	if err != nil {
		return nil, err
	}

	k, algorithm, err := p.macKey(rc, in.Message, in.KeyId, in.MacAlgorithm, "VerifyMac")
	if err != nil {
		return nil, err
	}

	// LocalStack passes the missing Mac on and answers 500.
	if in.Mac == nil {
		var r constraint.Report

		r.Null(nil, "mac")

		return nil, validation(r.Message())
	}

	if !hmac.Equal(in.Mac, computeMAC(k.Material, algorithm, in.Message)) {
		return nil, api.NewKMSInvalidMacException("")
	}

	return &api.VerifyMacOutput{KeyId: new(k.ARN), MacValid: new(true), MacAlgorithm: in.MacAlgorithm}, nil
}

// macKey is what GenerateMac and VerifyMac share before the MAC: the checks of
// the message, the key and the algorithm, in LocalStack's order. It returns the
// key and the algorithm's name. A missing Message or MacAlgorithm is a
// validation error, where LocalStack takes its length or looks it up and
// answers 500.
func (p *Provider) macKey(rc *request.Context, message []byte, keyID, algorithm *string, operation string,
) (*Key, string, error) {
	var r constraint.Report

	switch {
	case message == nil:
		r.Null(nil, "message")
	case len(message) > maxMessageBytes:
		r.Hidden(nil, "message", "Member must have length less than or equal to 4096")
	default:
	}

	if r.Failed() {
		return nil, "", validation(r.Message())
	}

	k, err := p.dataKey(rc, keyID)
	if err != nil {
		return nil, "", err
	}

	err = k.usable(usageMAC, operation)
	if err != nil {
		return nil, "", err
	}

	if algorithm == nil {
		r.Null(nil, "macAlgorithm")

		return nil, "", validation(r.Message())
	}

	// Validate reads only MacAlgorithm, the same enum in both inputs.
	err = (&api.GenerateMacInput{MacAlgorithm: algorithm}).Validate()
	if err != nil {
		return nil, "", err
	}

	// An algorithm HMAC_SHA_n belongs to the spec HMAC_n.
	if "HMAC_"+strings.TrimPrefix(*algorithm, "HMAC_SHA_") != k.Spec {
		return nil, "", api.NewInvalidKeyUsageException(
			"Algorithm " + *algorithm + " is incompatible with key spec " + k.Spec + ".")
	}

	return k, *algorithm, nil
}

// computeMAC is the HMAC of message under key with the digest of algorithm,
// which macKey has checked.
func computeMAC(key []byte, algorithm string, message []byte) []byte {
	h := hmac.New(macHashes[algorithm], key)
	h.Write(message)

	return h.Sum(nil)
}
