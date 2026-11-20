package kms

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"testing"

	"github.com/stretchr/testify/require"
)

func mustBlock(t *testing.T, key []byte) cipher.Block {
	t.Helper()

	block, err := aes.NewCipher(key)
	require.NoError(t, err)

	return block
}

const (
	kek128 = "000102030405060708090A0B0C0D0E0F"
	kek192 = "000102030405060708090A0B0C0D0E0F1011121314151617"
	kek256 = "000102030405060708090A0B0C0D0E0F101112131415161718191A1B1C1D1E1F"
	data16 = "00112233445566778899AABBCCDDEEFF"
	data24 = "00112233445566778899AABBCCDDEEFF0001020304050607"
	data32 = "00112233445566778899AABBCCDDEEFF000102030405060708090A0B0C0D0E0F"
	// rfc5649KEK is the KEK of RFC 5649, 6.
	rfc5649KEK = "5840df6e29b02af1ab493b705bf16ea1ae8338f4dcc176a8"
)

// Test_wrapKey_rfc3394 holds wrap and unwrap to the six vectors of RFC 3394, 4.
func Test_wrapKey_rfc3394(t *testing.T) {
	tests := []struct {
		name, kek, data, want string
	}{
		{"4.1 128-bit data, 128-bit KEK", kek128, data16, "1FA68B0A8112B447AEF34BD8FB5A7B829D3E862371D2CFE5"},
		{"4.2 128-bit data, 192-bit KEK", kek192, data16, "96778B25AE6CA435F92B5B97C050AED2468AB8A17AD84E5D"},
		{"4.3 128-bit data, 256-bit KEK", kek256, data16, "64E8C3F9CE0F5BA263E9777905818A2A93C8191E7D6E8AE7"},
		{
			"4.4 192-bit data, 192-bit KEK", kek192, data24,
			"031D33264E15D33268F24EC260743EDCE1C6C7DDEE725A936BA814915C6762D2",
		},
		{
			"4.5 192-bit data, 256-bit KEK", kek256, data24,
			"A8F9BC1612C68B3FF6E6F4FBE30E71E4769C8B80A32CB8958CD5D17D6B254DA1",
		},
		{
			"4.6 256-bit data, 256-bit KEK", kek256, data32,
			"28C9F404C4B810F4CBCCB35CFB87F8263F5786E2D80ED326CBC7F0E71A99F43BFB988B9B7A02DD21",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kek, data, want := unhex(t, tt.kek), unhex(t, tt.data), unhex(t, tt.want)

			got, err := wrapKey(kek, data)
			require.NoError(t, err)
			require.Equal(t, want, got)

			back, err := unwrapKey(kek, want)
			require.NoError(t, err)
			require.Equal(t, data, back)

			want[0] ^= 1
			_, err = unwrapKey(kek, want)
			require.ErrorIs(t, err, errWrapCheck)
		})
	}
}

// Test_wrapKeyPadded_rfc5649 holds the padded wrap to the two vectors of
// RFC 5649, 6: one longer than a semiblock and one shorter.
func Test_wrapKeyPadded_rfc5649(t *testing.T) {
	tests := []struct {
		name, key, want string
	}{
		{
			"20 octets", "c37b7e6492584340bed12207808941155068f738",
			"138bdeaa9b8fa7fc61f97742e72248ee5ae6ae5360d1ae6a5f54f373fa543b6a",
		},
		{"7 octets", "466f7250617369", "afbeb0f07dfbf5419200f2ccb50bb24f"},
	}

	kek := unhex(t, rfc5649KEK)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, want := unhex(t, tt.key), unhex(t, tt.want)

			got, err := wrapKeyPadded(kek, key)
			require.NoError(t, err)
			require.Equal(t, want, got)

			back, err := unwrapKeyPadded(kek, want)
			require.NoError(t, err)
			require.Equal(t, key, back)
		})
	}
}

// Test_wrapKeyPadded_roundTrip wraps and unwraps every length from 1 to 80
// bytes under each AES key size, the single-block case and the multi-block
// one, and refuses the wrong KEK and a changed byte anywhere.
func Test_wrapKeyPadded_roundTrip(t *testing.T) {
	for _, kek := range []string{kek128, kek192, kek256} {
		k := unhex(t, kek)
		other := bytes.Clone(k)
		other[0] ^= 0x80

		for n := 1; n <= 80; n++ {
			key := bytes.Repeat([]byte{byte(n)}, n)

			wrapped, err := wrapKeyPadded(k, key)
			require.NoError(t, err)
			require.Len(t, wrapped, (n+7)/8*8+8)

			back, err := unwrapKeyPadded(k, wrapped)
			require.NoError(t, err, "%d bytes under %d-bit KEK", n, len(k)*8)
			require.Equal(t, key, back)

			_, err = unwrapKeyPadded(other, wrapped)
			require.ErrorIs(t, err, errWrapCheck)

			for i := range wrapped {
				changed := bytes.Clone(wrapped)
				changed[i] ^= 1

				_, err = unwrapKeyPadded(k, changed)
				require.ErrorIs(t, err, errWrapCheck, "byte %d of %d changed", i, len(wrapped))
			}
		}
	}
}

// Test_unwrapKeyPadded_checks holds the checks of RFC 5649, 3, each on a
// value wrapped with a forged initial value: the length must fall in the last
// semiblock and the padding must be zero.
func Test_unwrapKeyPadded_checks(t *testing.T) {
	k := unhex(t, kek256)
	block := mustBlock(t, k)

	forge := func(length uint32, padded []byte) []byte {
		var aiv [semiblock]byte

		copy(aiv[:], aivPrefix[:])
		aiv[4], aiv[5], aiv[6], aiv[7] = byte(length>>24), byte(length>>16), byte(length>>8), byte(length)

		return wrapBlocks(block, aiv, padded)
	}

	sixteen := bytes.Repeat([]byte{7}, 16)

	tests := []struct {
		name string
		give []byte
		ok   bool
	}{
		{"length 16 of 16", forge(16, sixteen), true},
		{"length 9 of 16", forge(9, append(bytes.Repeat([]byte{7}, 9), make([]byte, 7)...)), true},
		{
			"length 8 of 16, a whole semiblock of padding",
			forge(8, append(bytes.Repeat([]byte{7}, 8), make([]byte, 8)...)), false,
		},
		{"length 17 of 16", forge(17, sixteen), false},
		{"length 0", forge(0, make([]byte, 16)), false},
		{"padding not zero", forge(9, sixteen), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := unwrapKeyPadded(k, tt.give)
			if tt.ok {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, errWrapCheck)
			}
		})
	}

	t.Run("lengths key wrap does not make", func(t *testing.T) {
		for _, n := range []int{0, 8, 15, 17, 23} {
			_, err := unwrapKeyPadded(k, make([]byte, n))
			require.ErrorIs(t, err, errWrapLength, "%d bytes", n)
		}

		_, err := unwrapKeyPadded(make([]byte, 5), make([]byte, 16))
		require.ErrorIs(t, err, errWrapKey)

		_, err = wrapKeyPadded(k, nil)
		require.ErrorIs(t, err, errWrapLength)
	})
}

// FuzzUnwrapKeyPadded holds that unwrapping any bytes under any key never
// panics, and that what does unwrap wraps back to the same bytes.
func FuzzUnwrapKeyPadded(f *testing.F) {
	kek := unhex(f, rfc5649KEK)

	f.Add(kek, unhex(f, "138bdeaa9b8fa7fc61f97742e72248ee5ae6ae5360d1ae6a5f54f373fa543b6a"))
	f.Add(kek, unhex(f, "afbeb0f07dfbf5419200f2ccb50bb24f"))
	f.Add(kek, []byte{})

	f.Fuzz(func(t *testing.T, kek, wrapped []byte) {
		key, err := unwrapKeyPadded(kek, wrapped)
		if err != nil {
			return
		}

		again, err := wrapKeyPadded(kek, key)
		require.NoError(t, err)
		require.Equal(t, wrapped, again)
	})
}
