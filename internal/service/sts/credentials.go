package sts

import (
	"crypto/rand"
	"encoding/base32"
	"strconv"
	"time"

	api "github.com/kylecannon/gostack/internal/api/sts"
	"github.com/kylecannon/gostack/internal/protocol"
)

const (
	// The shape of an access key ID that internal/account decodes: a 4
	// character prefix, 8 base32 characters that hold half the account, one
	// character that holds its lowest bit, and a random tail.
	accountHalfOffset = 549755813888 // int.from_bytes(b32decode("QAAAAAAA"), "big")
	accountsPerHalf   = 2
	accountHalfBytes  = 5
	base32Alphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	oddFrom           = 16 // a char at or after this index of the alphabet marks an odd account
	keyTailLength     = 7
	byteBits          = 8

	// prefixLocal is the prefix of a temporary access key under LocalStack's
	// default; prefixProduction the one of a real AWS key, which is what
	// PARITY_AWS_ACCESS_KEY_ID asks for.
	prefixLocal      = "LSIA"
	prefixProduction = "ASIA"

	roleIDPrefix = "AROA"
	roleIDLength = 17

	secretKeyLength    = 40
	sessionTokenLength = 356
	// sessionTokenStart is the start of every session token moto issues.
	sessionTokenStart = "FQoGZXIvYXdzE" //nolint:gosec // a public prefix, not a credential.

	// base64Alphabet has 64 characters, so a random byte maps onto it with no bias.
	base64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789/+"
	base64Mask     = 63

	// The default lifetimes of AssumeRole and of GetSessionToken, in seconds.
	defaultAssumeDuration = 3600
	defaultTokenDuration  = 43200
)

// newAccessKeyID returns an access key ID for accountID, which must be 12
// digits. internal/account.FromAccessKey decodes it back to accountID when its
// parity flag equals parity.
//
//nolint:revive // flag-parameter: parity mirrors the PARITY_AWS_ACCESS_KEY_ID setting, it does not steer control flow.
func newAccessKeyID(accountID string, parity bool) string {
	prefix := prefixLocal
	if parity {
		prefix = prefixProduction
	}

	number, err := strconv.ParseInt(accountID, 10, 64)
	if err != nil || number < 0 {
		number = 0
	}

	half := uint64(number/accountsPerHalf) + accountHalfOffset //nolint:gosec // number is a non-negative account.

	var raw [accountHalfBytes]byte
	for i := range raw {
		raw[accountHalfBytes-1-i] = byte(half >> (byteBits * i)) //nolint:gosec // takes the low byte on purpose.
	}

	// The character after the account part is random within the half of the
	// alphabet that matches the lowest bit of the account.
	var pick [1]byte

	_, _ = rand.Read(pick[:]) // crypto/rand.Read never returns an error.

	index := int(pick[0]) % oddFrom
	if number%accountsPerHalf == 1 {
		index += oddFrom
	}

	return prefix + base32.StdEncoding.EncodeToString(raw[:]) + string(base32Alphabet[index]) +
		rand.Text()[:keyTailLength]
}

// newRoleID returns the ID of a role: AROA and 17 more characters.
func newRoleID() string {
	return roleIDPrefix + rand.Text()[:roleIDLength]
}

// newSecretKey returns a secret access key of 40 characters.
func newSecretKey() string {
	return randomBase64(secretKeyLength)
}

// newSessionToken returns a session token in the shape moto issues.
func newSessionToken() string {
	return sessionTokenStart + randomBase64(sessionTokenLength) + "="
}

func randomBase64(length int) string {
	raw := make([]byte, length)
	_, _ = rand.Read(raw) // crypto/rand.Read never returns an error.

	for i, b := range raw {
		raw[i] = base64Alphabet[b&base64Mask]
	}

	return string(raw)
}

// newCredentials issues credentials for accountID that expire after duration
// seconds, or after defaultSeconds when duration is nil.
func (p *provider) newCredentials(accountID string, duration *int32, defaultSeconds int) *api.Credentials {
	seconds := defaultSeconds
	if duration != nil {
		seconds = int(*duration)
	}

	return &api.Credentials{
		AccessKeyId:     new(newAccessKeyID(accountID, p.parity())),
		SecretAccessKey: new(newSecretKey()),
		SessionToken:    new(newSessionToken()),
		Expiration:      &protocol.Time{Time: p.now().Add(time.Duration(seconds) * time.Second).UTC()},
	}
}
