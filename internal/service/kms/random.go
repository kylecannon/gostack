package kms

import (
	"context"

	api "github.com/kylecannon/gostack/internal/api/kms"
)

// GenerateRandom answers NumberOfBytes random bytes, 1 to 1024 of them.
// CustomKeyStoreId and Recipient are not read, in LocalStack either.
func (p *Provider) GenerateRandom(ctx context.Context, in *api.GenerateRandomInput) (*api.GenerateRandomOutput, error) {
	if in.NumberOfBytes == nil {
		return nil, validation("NumberOfBytes is required.")
	}

	err := in.Validate()
	if err != nil {
		return nil, err //nolint:wrapcheck // the generated ValidationException is the reply
	}

	plaintext, err := randomBytes(p.random, int(*in.NumberOfBytes))
	if err != nil {
		return nil, err
	}

	return &api.GenerateRandomOutput{Plaintext: plaintext}, nil
}
