package kms

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"sync"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/require"
)

// importRounds is the number of delete and re-import rounds each key goes
// through while the readers run. Run it with -race -count=20.
const importRounds = 40

// importedKey is an EXTERNAL key with the wrapped material that imports it.
type importedKey struct {
	id      *string
	token   []byte
	wrapped []byte
}

func newImportedKey(ctx context.Context, t *testing.T, p *Provider, spec, usage string, material []byte) importedKey {
	t.Helper()

	made, err := p.CreateKey(ctx, &api.CreateKeyInput{
		Origin: new(originExternal), KeySpec: new(spec), KeyUsage: new(usage),
	})
	require.NoError(t, err)

	params, err := p.GetParametersForImport(ctx, &api.GetParametersForImportInput{
		KeyId: made.KeyMetadata.KeyId, WrappingAlgorithm: new(wrapAESKeySHA256), WrappingKeySpec: new("RSA_2048"),
	})
	require.NoError(t, err)

	parsed, err := x509.ParsePKIXPublicKey(params.PublicKey)
	require.NoError(t, err)

	public, ok := parsed.(*rsa.PublicKey)
	require.True(t, ok)

	// RSA_AES_KEY_WRAP_SHA_256, since an RSA private key is longer than OAEP
	// takes.
	kek := bytes.Repeat([]byte{9}, aes256)

	wrappedKEK, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, public, kek, nil)
	require.NoError(t, err)

	wrappedMaterial, err := wrapKeyPadded(kek, material)
	require.NoError(t, err)

	k := importedKey{
		id: made.KeyMetadata.KeyId, token: params.ImportToken, wrapped: append(wrappedKEK, wrappedMaterial...),
	}
	require.NoError(t, k.load(ctx, p))

	return k
}

func (k importedKey) load(ctx context.Context, p *Provider) error {
	_, err := p.ImportKeyMaterial(ctx, &api.ImportKeyMaterialInput{
		KeyId: k.id, ImportToken: k.token, EncryptedKeyMaterial: k.wrapped, ExpirationModel: new(doesNotExpire),
	})

	return err
}

func (k importedKey) drop(ctx context.Context, p *Provider) error {
	_, err := p.DeleteImportedKeyMaterial(ctx, &api.DeleteImportedKeyMaterialInput{KeyId: k.id})

	return err
}

// refusedForState reports whether err is the KMSInvalidStateException a key
// without material answers.
func refusedForState(err error) bool {
	var awsErr *awserr.Error

	return errors.As(err, &awsErr) && awsErr.Code == codeInvalidState
}

// Test_import_whileUsing deletes and re-imports the material of a symmetric,
// an ECC and an RSA key while other goroutines Encrypt, Decrypt, Sign, Verify
// and read the public key. Every read of material goes through one hold of
// the key's lock (Key.materials), so -race finds no race; every call that
// succeeds uses the imported material; and a data call made after
// DeleteImportedKeyMaterial returned is refused.
func Test_import_whileUsing(t *testing.T) {
	p := New(nil)
	ctx := request.With(t.Context(), &request.Context{AccountID: "000000000000", Region: testRegion})

	symmetric := bytes.Repeat([]byte{7}, 32)

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	ecDER, err := x509.MarshalPKCS8PrivateKey(ecKey)
	require.NoError(t, err)

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	rsaDER, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	require.NoError(t, err)

	rsaPublic, err := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	require.NoError(t, err)

	keys := []importedKey{
		newImportedKey(ctx, t, p, specSymmetric, usageEncrypt, symmetric),
		newImportedKey(ctx, t, p, specP256, "SIGN_VERIFY", ecDER),
		newImportedKey(ctx, t, p, "RSA_2048", usageEncrypt, rsaDER),
	}
	sym, ec, rsaK := keys[0], keys[1], keys[2]

	blob, err := p.Encrypt(ctx, &api.EncryptInput{KeyId: sym.id, Plaintext: []byte("x")})
	require.NoError(t, err)

	offline, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &rsaKey.PublicKey, []byte("offline"), nil)
	require.NoError(t, err)

	message := []byte("message")
	digest := sha256.Sum256(message)

	stop := make(chan struct{})

	var (
		readers sync.WaitGroup
		mu      sync.Mutex
		wrong   []string
	)

	fail := func(what string, err error) {
		if err == nil || refusedForState(err) {
			return
		}

		mu.Lock()
		wrong = append(wrong, what+": "+err.Error())
		mu.Unlock()
	}

	reads := []func(){
		func() {
			out, callErr := p.Decrypt(ctx, &api.DecryptInput{CiphertextBlob: blob.CiphertextBlob})
			if callErr == nil && string(out.Plaintext) != "x" {
				callErr = errors.New("decrypted " + string(out.Plaintext))
			}

			fail("Decrypt", callErr)
		},
		func() {
			_, callErr := p.Encrypt(ctx, &api.EncryptInput{KeyId: sym.id, Plaintext: []byte("y")})
			fail("Encrypt", callErr)
		},
		func() {
			out, callErr := p.Sign(ctx, &api.SignInput{
				KeyId: ec.id, Message: message, MessageType: new("RAW"), SigningAlgorithm: new("ECDSA_SHA_256"),
			})
			if callErr == nil && !ecdsa.VerifyASN1(&ecKey.PublicKey, digest[:], out.Signature) {
				callErr = errors.New("a signature the imported key does not verify")
			}

			fail("Sign", callErr)
		},
		func() {
			out, callErr := p.GetPublicKey(ctx, &api.GetPublicKeyInput{KeyId: rsaK.id})
			if callErr == nil && !bytes.Equal(out.PublicKey, rsaPublic) {
				callErr = errors.New("a public key that is not the imported one")
			}

			fail("GetPublicKey", callErr)
		},
		func() {
			out, callErr := p.Decrypt(ctx, &api.DecryptInput{KeyId: rsaK.id, CiphertextBlob: offline})
			if callErr == nil && string(out.Plaintext) != "offline" {
				callErr = errors.New("RSA decrypted " + string(out.Plaintext))
			}

			fail("Decrypt RSA", callErr)
		},
	}

	for _, read := range reads {
		readers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					read()
				}
			}
		})
	}

	for range importRounds {
		for _, k := range keys {
			require.NoError(t, k.drop(ctx, p))
		}

		// After the delete returned, no data call succeeds.
		_, err = p.Encrypt(ctx, &api.EncryptInput{KeyId: sym.id, Plaintext: []byte("z")})
		require.True(t, refusedForState(err), "Encrypt after the delete: %v", err)

		_, err = p.Sign(ctx, &api.SignInput{
			KeyId: ec.id, Message: message, MessageType: new("RAW"), SigningAlgorithm: new("ECDSA_SHA_256"),
		})
		require.True(t, refusedForState(err), "Sign after the delete: %v", err)

		for _, k := range keys {
			require.NoError(t, k.load(ctx, p))
		}
	}

	close(stop)
	readers.Wait()

	require.Empty(t, wrong)
}
