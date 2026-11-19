package ciphertext_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/kylecannon/gostack/internal/service/kms/ciphertext"
	"github.com/stretchr/testify/require"
)

// The vectors below were built by LocalStack's own code at 8b9a79f: utils/crypto.py
// encrypt and models.py _serialize_ciphertext_blob and
// _serialize_encryption_context, run with Python's cryptography 47, key
// 00 01 .. 1f and IV 64 65 .. 73.
const (
	vectorEmpty = "35643666316330652d386134332d346233652d396130622d3166376633623265396334316465666768696a6b6c" +
		"6d6e6f70717273ac831b3c97afdb3b5cbfd610fa226901c8a35341f40437c8a2e9bd9957c7fc75"
	vectorBlock = "35643666316330652d386134332d346233652d396130622d3166376633623265396334316465666768696a6b6c" +
		"6d6e6f707172738fdc237cf52b25de3fa22aa515fb4670e8827162d02111ef8ac0cceb24b389034a6148d08cfff01bd590047e89e717d8"
	vectorContext = "6d726b2d30313233343536373839616263646566303132333435363738396162636465666465666768696a6b6c" +
		"6d6e6f70717273d8dbe001fd53aafb0f76d546db8ef593b0d62f3d8b344cb5c1feaa8e40d0eb62"

	uuidKey = "5d6f1c0e-8a43-4b3e-9a0b-1f7f3b2e9c41"
	mrkKey  = "mrk-0123456789abcdef0123456789abcdef"
)

func material() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	return key
}

// fixedIV reads LocalStack's vector IV, 0x64 to 0x73.
type fixedIV struct{}

func (fixedIV) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(0x64 + i)
	}

	return len(p), nil
}

func Test_vectors(t *testing.T) {
	context := map[string]string{"zeta": "1", "alpha": "2", "aws-crypto-public-key": "ignored"}

	tests := []struct {
		name          string
		giveKeyID     string
		givePlaintext []byte
		giveContext   map[string]string
		wantAAD       string
		wantBlob      string
	}{
		{
			name:          "empty plaintext pads a whole block",
			giveKeyID:     uuidKey,
			givePlaintext: []byte{},
			giveContext:   nil,
			wantAAD:       "",
			wantBlob:      vectorEmpty,
		},
		{
			name:          "one block of plaintext pads another",
			giveKeyID:     uuidKey,
			givePlaintext: []byte("0123456789abcdef"),
			giveContext:   map[string]string{},
			wantAAD:       "",
			wantBlob:      vectorBlock,
		},
		{
			name:          "context sorted by key, ignored key left out, mrk id",
			giveKeyID:     mrkKey,
			givePlaintext: []byte("hello kms"),
			giveContext:   context,
			wantAAD:       "alpha2zeta1",
			wantBlob:      vectorContext,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			aad := ciphertext.Context(tt.giveContext)
			require.Equal(t, tt.wantAAD, string(aad))

			sealed, err := ciphertext.Seal(material(), tt.giveKeyID, tt.givePlaintext, aad, fixedIV{})
			require.NoError(t, err)
			require.Equal(t, tt.wantBlob, hex.EncodeToString(sealed))

			want, err := hex.DecodeString(tt.wantBlob)
			require.NoError(t, err)

			blob, err := ciphertext.Parse(want)
			require.NoError(t, err)
			require.Equal(t, tt.giveKeyID, blob.KeyID)
			require.Equal(t, want, blob.Bytes())

			plaintext, err := ciphertext.Open(material(), blob, aad)
			require.NoError(t, err)
			require.Equal(t, tt.givePlaintext, plaintext)
		})
	}
}

func Test_Open(t *testing.T) {
	sealed, err := ciphertext.Seal(material(), uuidKey, []byte("secret"), []byte("ctx"), rand.Reader)
	require.NoError(t, err)

	blob, err := ciphertext.Parse(sealed)
	require.NoError(t, err)

	t.Run("refuses other additional data", func(t *testing.T) {
		_, err := ciphertext.Open(material(), blob, []byte("other"))
		require.ErrorIs(t, err, ciphertext.ErrInvalid)
	})

	t.Run("refuses another key", func(t *testing.T) {
		_, err := ciphertext.Open(bytes.Repeat([]byte{7}, 32), blob, []byte("ctx"))
		require.ErrorIs(t, err, ciphertext.ErrInvalid)
	})

	t.Run("refuses a changed tag", func(t *testing.T) {
		changed := *blob
		changed.Tag[0] ^= 1

		_, err := ciphertext.Open(material(), &changed, []byte("ctx"))
		require.ErrorIs(t, err, ciphertext.ErrInvalid)
	})

	t.Run("refuses material that is not an AES key", func(t *testing.T) {
		_, err := ciphertext.Open([]byte("short"), blob, nil)
		require.Error(t, err)
	})
}

func Test_Parse(t *testing.T) {
	t.Run("refuses a blob shorter than the header", func(t *testing.T) {
		_, err := ciphertext.Parse(make([]byte, ciphertext.HeaderLen-1))
		require.ErrorIs(t, err, ciphertext.ErrInvalid)
	})

	t.Run("refuses a key id that is not UTF-8", func(t *testing.T) {
		data := make([]byte, ciphertext.HeaderLen)
		data[0] = 0xff

		_, err := ciphertext.Parse(data)
		require.ErrorIs(t, err, ciphertext.ErrInvalid)
	})

	t.Run("pads a short key id with zero bytes and drops them again", func(t *testing.T) {
		b := &ciphertext.Blob{KeyID: "short-id", Ciphertext: []byte{1, 2}}
		wire := b.Bytes()
		require.Len(t, wire, ciphertext.HeaderLen+2)
		require.Equal(t, byte(0), wire[len("short-id")])

		got, err := ciphertext.Parse(wire)
		require.NoError(t, err)
		require.Equal(t, b, got)
	})
}

// FuzzParse holds Parse to its contract: any input either parses into a blob
// whose wire form gives the same blob back, or fails with ErrInvalid.
func FuzzParse(f *testing.F) {
	for _, vector := range []string{vectorEmpty, vectorBlock, vectorContext} {
		data, err := hex.DecodeString(vector)
		require.NoError(f, err)
		f.Add(data)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		blob, err := ciphertext.Parse(data)
		if err != nil {
			require.ErrorIs(t, err, ciphertext.ErrInvalid)

			return
		}

		again, err := ciphertext.Parse(blob.Bytes())
		require.NoError(t, err)
		require.Equal(t, blob, again)

		_, err = ciphertext.Open(material(), blob, nil)
		if err != nil {
			require.ErrorIs(t, err, ciphertext.ErrInvalid)
		}
	})
}
