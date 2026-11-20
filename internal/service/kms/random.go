package kms

import (
	"context"

	api "github.com/kylecannon/gostack/internal/api/kms"
)

// GenerateRandom answers NumberOfBytes random bytes, 1 to 1024 of them.
// CustomKeyStoreId is not read, in LocalStack either. A Recipient gets the
// bytes in CiphertextForRecipient, as GenerateDataKey answers it.
func (p *Provider) GenerateRandom(_ context.Context, in *api.GenerateRandomInput) (*api.GenerateRandomOutput, error) {
	if in.NumberOfBytes == nil {
		return nil, validation("NumberOfBytes is required.")
	}

	err := in.Validate()
	if err != nil {
		return nil, err
	}

	plaintext, err := randomBytes(p.random, int(*in.NumberOfBytes))
	if err != nil {
		return nil, err
	}

	sealed, bare, err := p.forRecipient(in.Recipient, plaintext)
	if err != nil {
		return nil, err
	}

	return &api.GenerateRandomOutput{Plaintext: bare, CiphertextForRecipient: sealed}, nil
}
