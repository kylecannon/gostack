package account

import (
	"encoding/base32"
	"strconv"
	"strings"
)

const (
	// Default is the account of every call that names no other.
	Default = "000000000000"

	accountDigits = 12
	// minKeyLength is the shortest access key ID that carries an encoded
	// account.
	minKeyLength = 20
	// encodedStart and encodedEnd bound the base32 account part of an access
	// key ID; the character after it selects the account's lowest bit.
	encodedStart = 4
	encodedEnd   = 12
	// accountOffset is int.from_bytes(base32.b32decode(b"QAAAAAAA"), "big"), the
	// encoding of account 000000000000.
	accountOffset = 549755813888
	// maxAccount is the largest 12-digit account.
	maxAccount = 999999999999
	// base32Alphabet is the RFC 4648 alphabet, in code order.
	base32Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	// oddAccountFrom is the index in base32Alphabet from which the character
	// after the account part marks an odd account.
	oddAccountFrom = 16
	// accountsPerStep is how many accounts one step of the encoded part covers;
	// the character after the part picks one of the two.
	accountsPerStep = 2
	bitsPerByte     = 8
)

// FromAccessKey returns the account that an access key ID stands for.
//
// A key of exactly 12 digits is the account itself. LocalStack tests that with
// re.match(r"\d{12}", key), which also accepts "123456789012abc" and returns
// the whole key as the account. No test relies on that, so a key with
// anything after the 12 digits falls through to the other rules here.
//
// A key of at least 20 characters starting with LSIA or LKIA always decodes to
// its account. ASIA and AKIA keys decode only when parity is set, which is
// PARITY_AWS_ACCESS_KEY_ID: otherwise they are production credentials and the
// default account applies. Every other key gets the default account.
//
//nolint:revive // flag-parameter: parity mirrors the PARITY_AWS_ACCESS_KEY_ID setting, it does not steer control flow.
func FromAccessKey(accessKeyID string, parity bool) string {
	if isAccountID(accessKeyID) {
		return accessKeyID
	}

	if len(accessKeyID) < minKeyLength {
		return Default
	}

	prefix := accessKeyID[:encodedStart]

	local := prefix == "LSIA" || prefix == "LKIA"
	production := prefix == "ASIA" || prefix == "AKIA"

	if local && !parity || production && parity {
		return decode(accessKeyID)
	}

	return Default
}

func isAccountID(s string) bool {
	if len(s) != accountDigits {
		return false
	}

	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}

	return true
}

// decode extracts the account from an access key ID of at least 20 characters,
// or returns Default when the key does not decode to one.
func decode(accessKeyID string) string {
	part, err := base32.StdEncoding.DecodeString(accessKeyID[encodedStart:encodedEnd])
	if err != nil {
		return Default
	}

	var encoded int64
	for _, b := range part {
		encoded = encoded<<bitsPerByte | int64(b)
	}

	account := accountsPerStep * (encoded - accountOffset)

	index := strings.IndexByte(base32Alphabet, accessKeyID[encodedEnd])
	if index < 0 {
		return Default
	}

	if index >= oddAccountFrom {
		account++
	}

	if account < 0 || account > maxAccount {
		return Default
	}

	return format(account)
}

// format writes account as 12 digits, zero padded.
func format(account int64) string {
	digits := strconv.FormatInt(account, 10)

	return strings.Repeat("0", accountDigits-len(digits)) + digits
}
