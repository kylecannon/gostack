package kms_test

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/x509"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/kms"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// serviceKMS is the name the engine keys KMS's state by.
	serviceKMS = "kms"

	opScheduleDeletion = "ScheduleKeyDeletion"
	opDisableKey       = "DisableKey"
	opCreateGrant      = "CreateGrant"
	opListGrants       = "ListGrants"
	opRetireGrant      = "RetireGrant"
	opGenerateMac      = "GenerateMac"
	opVerifyMac        = "VerifyMac"
	opCreateAlias      = "CreateAlias"
	opListAliases      = "ListAliases"
	opListTags         = "ListResourceTags"
	opGetKeyPolicy     = "GetKeyPolicy"
	opPutKeyPolicy     = "PutKeyPolicy"

	fieldSignatureValid = "SignatureValid"
	fieldMacValid       = "MacValid"
	fieldSharedSecret   = "SharedSecret"
	fieldConstraints    = "Constraints"
	fieldContextSubset  = "EncryptionContextSubset"

	aliasPersisted  = "alias/persisted"
	messageRaw      = "RAW"
	usageAgreement  = "KEY_AGREEMENT"
	persistedPolicy = `{"Version":"2012-10-17","Statement":[]}`
)

// startPersisted loads KMS from dir through an engine that writes format, and
// returns the engine and a stack whose gateway tells the engine about every
// change.
func startPersisted(t *testing.T, dir string, format persist.Format) (*persist.Engine, *stack) {
	t.Helper()

	eng := persist.New(persist.Config{
		PersistVarSet: true, BaseDir: dir, Frequency: time.Hour, Formats: []persist.Format{format},
		MountDetector: func(string) bool { return false },
	})

	p := kms.New(eng.MarkDirty)
	eng.Register(serviceKMS, p.Registration().Persist)
	require.NoError(t, eng.Load(t.Context()))

	return eng, stackFor(t, p, eng)
}

// stackFor is a stack in front of p whose gateway marks d dirty.
func stackFor(t *testing.T, p *kms.Provider, d gateway.Dirtier) *stack {
	t.Helper()

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{testListen}} },
		[]service.Registration{p.Registration()}, nil, d,
	)
	require.NoError(t, err)

	return &stack{t: t, handler: gw.Handler()}
}

// sealed is a ciphertext made before a restart, with what decrypts it.
type sealed struct {
	region string
	input  map[string]any
	plain  []byte
}

// signed is a signature or MAC made before a restart, with the call that
// checks it and the member that says it is valid.
type signed struct {
	operation string
	input     map[string]any
	valid     string
}

// derived is a shared secret derived before a restart.
type derived struct {
	input  map[string]any
	secret any
}

// persistedSample is what putPersistedSample made and what a restarted KMS
// must still answer the same way.
type persistedSample struct {
	ids       []string
	replicaID string
	sealed    []sealed
	signed    []signed
	derived   []derived
	// waiting is an import token handed out and not used before the restart.
	waiting   importParams
	waitingID string
	grantKey  string
	grantTok  string
	pendingID string
	material  []byte
}

// signingSpecs are the signing key specs and an algorithm of each.
var signingSpecs = map[string]string{
	rsa2048: "RSASSA_PSS_SHA_256",
	rsa3072: "RSASSA_PKCS1_V1_5_SHA_384",
	rsa4096: "RSASSA_PKCS1_V1_5_SHA_512",
	eccP256: ecdsaSHA256,
	eccP384: "ECDSA_SHA_384",
	eccP521: "ECDSA_SHA_512",
	secgK1:  ecdsaSHA256,
}

// macBits are the sizes of the HMAC key specs, HMAC_<bits>, whose algorithm
// is HMAC_SHA_<bits>.
var macBits = []string{"224", "256", "384", "512"}

func (s *stack) keep(sample *persistedSample, meta map[string]any) string {
	id := keyID(s.t, meta)
	sample.ids = append(sample.ids, id)

	return id
}

// seal encrypts plain under id in region and keeps the ciphertext.
func (s *stack) seal(sample *persistedSample, region, id string, input map[string]any) {
	s.t.Helper()

	plain := random(s.t, 48)
	encrypt := map[string]any{fieldKeyID: id, fieldPlaintext: b64(plain)}
	decrypt := map[string]any{}

	for k, v := range input {
		encrypt[k], decrypt[k] = v, v
	}

	r := s.call(region, opEncrypt, encrypt)
	require.Equal(s.t, http.StatusOK, r.status, "%v", r.body)

	decrypt[fieldCiphertextBlob] = b64(blobOf(s.t, r.body, fieldCiphertextBlob))
	sample.sealed = append(sample.sealed, sealed{region: region, input: decrypt, plain: plain})
}

// putPersistedSample makes a key of every spec, a rotated key, a replicated
// key, imported keys, a key pending deletion and a disabled one, an alias, a
// grant, tags and a policy, and uses each key once.
func putPersistedSample(t *testing.T, s *stack) *persistedSample {
	t.Helper()

	sample := &persistedSample{}

	symmetric := s.keep(sample, s.createKey(map[string]any{
		fieldTags: []map[string]string{{fieldTagKey: tag1, fieldTagValue: value1}},
	}))
	s.seal(sample, usEast1, symmetric, map[string]any{fieldEncryptionContext: map[string]string{"a": "b"}})
	s.ok(opPutKeyPolicy, map[string]any{
		fieldKeyID: symmetric, fieldPolicyName: policyDefault, fieldPolicy: persistedPolicy,
	})
	s.ok(opCreateAlias, map[string]any{fieldAliasName: aliasPersisted, fieldTargetKeyID: symmetric})
	s.seal(sample, usEast1, aliasPersisted, nil)

	grant := s.ok(opCreateGrant, map[string]any{
		fieldKeyID: symmetric, fieldGranteePrincipal: "arn:aws:iam::000000000000:role/grantee", fieldName: "g1",
		fieldOperations:  []string{opEncrypt, opDecrypt},
		fieldConstraints: map[string]any{fieldContextSubset: map[string]string{"a": "b"}},
	})
	token, ok := grant[fieldGrantToken].(string)
	require.True(t, ok)

	sample.grantKey, sample.grantTok = symmetric, token

	putSigningKeys(t, s, sample)
	putRotatedAndReplicated(t, s, sample)
	putImported(t, s, sample)

	sample.pendingID = s.keep(sample, s.createKey(map[string]any{}))
	s.seal(sample, usEast1, sample.pendingID, nil)
	s.ok(opScheduleDeletion, map[string]any{fieldKeyID: sample.pendingID, fieldPendingWindow: 7})

	disabled := s.keep(sample, s.createKey(map[string]any{}))
	s.ok(opDisableKey, map[string]any{fieldKeyID: disabled})

	return sample
}

func putSigningKeys(t *testing.T, s *stack, sample *persistedSample) {
	t.Helper()

	for spec, algorithm := range signingSpecs {
		id := s.keep(sample, s.createKey(map[string]any{fieldKeySpec: spec, fieldKeyUsage: usageSignVerify}))
		input := map[string]any{
			fieldKeyID: id, fieldMessage: b64([]byte(spec)), fieldMessageType: messageRaw, fieldSigningAlgorithm: algorithm,
		}
		input[fieldSignature] = b64(blobOf(t, s.ok(opSign, input), fieldSignature))
		sample.signed = append(sample.signed, signed{operation: opVerify, input: input, valid: fieldSignatureValid})
	}

	for _, bits := range macBits {
		id := s.keep(sample, s.createKey(map[string]any{fieldKeySpec: "HMAC_" + bits, fieldKeyUsage: usageGenerateMAC}))
		input := map[string]any{fieldKeyID: id, fieldMessage: b64([]byte(bits)), fieldMacAlgorithm: "HMAC_SHA_" + bits}
		input[fieldMac] = b64(blobOf(t, s.ok(opGenerateMac, input), fieldMac))
		sample.signed = append(sample.signed, signed{operation: opVerifyMac, input: input, valid: fieldMacValid})
	}

	rsaEncrypt := s.keep(sample, s.createKey(map[string]any{fieldKeySpec: rsa2048, fieldKeyUsage: usageEncryptDecrypt}))
	s.seal(sample, usEast1, rsaEncrypt, map[string]any{fieldAlgorithm: oaepSHA256})

	peer, err := ecdh.P256().GenerateKey(rand.Reader)
	require.NoError(t, err)

	peerDER, err := x509.MarshalPKIXPublicKey(peer.PublicKey())
	require.NoError(t, err)

	agreement := s.keep(sample, s.createKey(map[string]any{fieldKeySpec: eccP256, fieldKeyUsage: usageAgreement}))
	input := map[string]any{fieldKeyID: agreement, fieldAgreement: algorithmECDH, fieldPublicKey: b64(peerDER)}
	sample.derived = append(sample.derived, derived{input: input, secret: s.ok(opDerive, input)[fieldSharedSecret]})
}

func putRotatedAndReplicated(t *testing.T, s *stack, sample *persistedSample) {
	t.Helper()

	rotated := s.keep(sample, s.createKey(map[string]any{}))
	s.seal(sample, usEast1, rotated, nil)
	s.ok(opRotate, map[string]any{fieldKeyID: rotated})
	s.seal(sample, usEast1, rotated, nil)
	s.ok(opEnableRotation, map[string]any{fieldKeyID: rotated, fieldRotationPeriod: 180})

	primary := s.keep(sample, s.createKey(map[string]any{fieldMultiRegion: true}))
	s.seal(sample, usEast1, primary, nil)
	replica := s.ok(opReplicate, map[string]any{fieldKeyID: primary, fieldReplicaRegion: usWest2})
	sample.replicaID = keyID(t, object(t, replica["ReplicaKeyMetadata"]))
	s.seal(sample, usWest2, sample.replicaID, nil)
}

func putImported(t *testing.T, s *stack, sample *persistedSample) {
	t.Helper()

	sample.material = random(t, 32)

	imported := s.keep(sample, s.createKey(map[string]any{fieldOrigin: originExternal}))
	s.ok(opImport, s.importParams(imported, oaepSHA256).input(t, imported, sample.material))
	s.seal(sample, usEast1, imported, nil)

	deleted := s.keep(sample, s.createKey(map[string]any{fieldOrigin: originExternal}))
	s.ok(opImport, s.importParams(deleted, oaepSHA256).input(t, deleted, sample.material))
	s.ok(opDeleteImport, map[string]any{fieldKeyID: deleted})

	sample.waitingID = s.keep(sample, s.createKey(map[string]any{fieldOrigin: originExternal}))
	sample.waiting = s.importParams(sample.waitingID, oaepSHA256)
}

// views are the answers a key's describing calls give, which a restart must
// not change.
func (s *stack) views(sample *persistedSample) map[string]any {
	s.t.Helper()

	out := map[string]any{opListAliases: s.ok(opListAliases, map[string]any{})}

	for _, id := range sample.ids {
		key := map[string]any{fieldKeyID: id}
		out[id] = []any{
			s.ok(opDescribe, key), s.ok(opListTags, key), s.ok(opRotationStatus, key),
			s.ok(opGetKeyPolicy, map[string]any{fieldKeyID: id, fieldPolicyName: policyDefault}),
			s.ok(opListGrants, key), s.call(usEast1, opGetPublicKey, key).body,
		}
	}

	replica := s.call(usWest2, opDescribe, map[string]any{fieldKeyID: sample.replicaID})
	out[sample.replicaID] = replica.body

	return out
}

// checkPersistedSample uses what putPersistedSample made on a restarted KMS.
func checkPersistedSample(t *testing.T, s *stack, sample *persistedSample) {
	t.Helper()

	// The key pending deletion refuses its ciphertext until it is enabled
	// again, and then decrypts it.
	pending := map[string]any{fieldKeyID: sample.pendingID}
	s.fails(opEncrypt, map[string]any{fieldKeyID: sample.pendingID, fieldPlaintext: b64([]byte("x"))},
		codeKMSInvalid, keyARN(sample.pendingID)+" is pending deletion.")
	s.ok("CancelKeyDeletion", pending)
	s.ok("EnableKey", pending)

	for _, c := range sample.sealed {
		r := s.call(c.region, opDecrypt, c.input)
		require.Equal(t, http.StatusOK, r.status, "%v", r.body)
		assert.Equal(t, c.plain, blobOf(t, r.body, fieldPlaintext))
	}

	for _, c := range sample.signed {
		assert.Equal(t, true, s.ok(c.operation, c.input)[c.valid], "%v", c.input)
	}

	for _, c := range sample.derived {
		assert.Equal(t, c.secret, s.ok(opDerive, c.input)[fieldSharedSecret])
	}

	// The wrapping key of a token handed out before the restart still
	// unwraps.
	s.ok(opImport, sample.waiting.input(t, sample.waitingID, sample.material))

	// The grant token index came back with the grant.
	s.ok(opRetireGrant, map[string]any{fieldGrantToken: sample.grantTok})
	assert.Empty(t, s.ok(opListGrants, map[string]any{fieldKeyID: sample.grantKey})[fieldGrants])
}

// Test_persisted_keys_survive_a_restart writes keys through the gateway,
// flushes in each PERSIST_FORMAT, loads them into a new provider, and checks
// that every key answers as before: its ciphertexts decrypt, the
// pre-rotation one included, its signatures and MACs verify, and its
// metadata, tags, policy, rotation, grants and aliases are unchanged.
func Test_persisted_keys_survive_a_restart(t *testing.T) {
	for format, file := range map[persist.Format]string{
		persist.FormatJSON: "store.json", persist.FormatBinary: "store.bin",
	} {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			eng, s := startPersisted(t, dir, format)

			sample := putPersistedSample(t, s)
			before := s.views(sample)
			require.NoError(t, eng.Flush(t.Context()))

			info, err := os.Stat(filepath.Join(dir, serviceKMS, file))
			require.NoError(t, err, "the state is written as %s", file)
			assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the snapshot holds key material")

			_, restarted := startPersisted(t, dir, format)
			assert.Equal(t, before, restarted.views(sample))
			checkPersistedSample(t, restarted, sample)
		})
	}
}

// Test_a_restored_snapshot_snapshots_the_same writes a snapshot of the sample,
// restores it and snapshots again: the bytes are the same, so nothing a
// snapshot holds is lost or changed on the way back.
func Test_a_restored_snapshot_snapshots_the_same(t *testing.T) {
	p := kms.New(nil)
	putPersistedSample(t, stackFor(t, p, nil))

	first := snapshotOf(t, p)

	restored := kms.New(nil)
	require.NoError(t, restored.Restore(bytesReader(first)))
	assert.Equal(t, string(first), string(snapshotOf(t, restored)))
}

// Test_a_corrupt_material_is_refused changes one byte of a key's material in
// a snapshot: Restore refuses the file and leaves KMS empty, rather than
// restore a key that decrypts nothing it encrypted.
func Test_a_corrupt_material_is_refused(t *testing.T) {
	p := kms.New(nil)
	s := stackFor(t, p, nil)
	id := keyID(t, s.createKey(map[string]any{
		fieldTags: []map[string]string{{fieldTagKey: tagCustomMaterial, fieldTagValue: b64(make([]byte, 32))}},
	}))

	saved := snapshotOf(t, p)
	corrupt := replaceOnce(t, saved, `"material":"`+b64(make([]byte, 32))+`"`,
		`"material":"`+b64(append(make([]byte, 31), 1))+`"`)

	restored := kms.New(nil)
	require.ErrorContains(t, restored.Restore(bytesReader(corrupt)), "does not match its digest")

	r := stackFor(t, restored, nil).call(usEast1, opDescribe, map[string]any{fieldKeyID: id})
	assert.Equal(t, http.StatusBadRequest, r.status, "the refused file restored nothing")
}

// dirtyCounter is a gateway.Dirtier that counts the calls that marked KMS.
type dirtyCounter struct{ n atomic.Int32 }

func (d *dirtyCounter) MarkDirty(string) { d.n.Add(1) }

// Test_data_operations_do_not_mark_kms_dirty runs the operations that use a
// key without changing it, each of which must leave persistence idle.
func Test_data_operations_do_not_mark_kms_dirty(t *testing.T) {
	var changed atomic.Int32

	p := kms.New(func(string) { changed.Add(1) })
	dirty := &dirtyCounter{}
	s := stackFor(t, p, dirty)

	symmetric := keyID(t, s.createKey(map[string]any{}))
	signing := keyID(t, s.createKey(map[string]any{fieldKeySpec: eccP256, fieldKeyUsage: usageSignVerify}))
	mac := keyID(t, s.createKey(map[string]any{fieldKeySpec: hmac256, fieldKeyUsage: usageGenerateMAC}))
	require.Positive(t, dirty.n.Load(), "CreateKey marks KMS dirty")
	dirty.n.Store(0)

	blob := s.encrypt(symmetric, []byte("plain"), nil)
	s.ok(opDecrypt, map[string]any{fieldCiphertextBlob: b64(blob)})

	sign := map[string]any{
		fieldKeyID: signing, fieldMessage: b64([]byte("m")), fieldMessageType: messageRaw,
		fieldSigningAlgorithm: ecdsaSHA256,
	}
	sign[fieldSignature] = b64(blobOf(t, s.ok(opSign, sign), fieldSignature))
	s.ok(opVerify, sign)

	macInput := map[string]any{fieldKeyID: mac, fieldMessage: b64([]byte("m")), fieldMacAlgorithm: alg256}
	macInput[fieldMac] = b64(blobOf(t, s.ok(opGenerateMac, macInput), fieldMac))
	s.ok(opVerifyMac, macInput)

	s.ok("GenerateDataKey", map[string]any{fieldKeyID: symmetric})
	s.ok("GenerateDataKeyWithoutPlaintext", map[string]any{fieldKeyID: symmetric})
	s.ok("GenerateDataKeyPair", map[string]any{fieldKeyID: symmetric, fieldKeyPairSpec: eccP256})
	s.ok("GenerateRandom", map[string]any{fieldNumberOfBytes: 16})
	s.ok("ReEncrypt", map[string]any{fieldCiphertextBlob: b64(blob), fieldDestinationKeyID: symmetric})

	assert.Zero(t, dirty.n.Load(), "a data operation marked KMS dirty")
	assert.Zero(t, changed.Load(), "a data operation reported a change")
}

// Test_snapshot_while_rotating_and_importing takes snapshots while keys rotate
// and take imported material: under -race a read outside the key's lock is a
// data race, and the last snapshot restores keys whose every ciphertext
// decrypts.
func Test_snapshot_while_rotating_and_importing(t *testing.T) {
	p := kms.New(nil)
	s := stackFor(t, p, nil)

	rotated := keyID(t, s.createKey(map[string]any{}))
	external := keyID(t, s.createKey(map[string]any{fieldOrigin: originExternal}))
	params := s.importParams(external, oaepSHA256)
	blobs := make([][]byte, 0, 11)
	blobs = append(blobs, s.encrypt(rotated, []byte("before"), nil))

	stop := make(chan struct{})

	var wg sync.WaitGroup

	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				_ = p.Snapshot(io.Discard)
			}
		}
	})

	for range 9 {
		s.ok(opRotate, map[string]any{fieldKeyID: rotated})
		blobs = append(blobs, s.encrypt(rotated, []byte("before"), nil))
	}

	s.ok(opImport, params.input(t, external, random(t, 32)))
	blobs = append(blobs, s.encrypt(external, []byte("before"), nil))

	close(stop)
	wg.Wait()

	restored := kms.New(nil)
	require.NoError(t, restored.Restore(bytesReader(snapshotOf(t, p))))

	after := stackFor(t, restored, nil)
	for _, blob := range blobs {
		assert.Equal(t, []byte("before"), after.decrypted(usEast1, blob))
	}
}

// snapshotOf is a snapshot of p.
func snapshotOf(t *testing.T, p *kms.Provider) []byte {
	t.Helper()

	var out bytes.Buffer
	require.NoError(t, p.Snapshot(&out))

	return out.Bytes()
}

// keyARN is the ARN of a key of the test account in us-east-1.
func keyARN(id string) string { return "arn:aws:kms:us-east-1:" + account + ":key/" + id }

// bytesReader reads b.
func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// replaceOnce is b with its one occurrence of old replaced by replacement.
func replaceOnce(t *testing.T, b []byte, old, replacement string) []byte {
	t.Helper()

	require.Equal(t, 1, bytes.Count(b, []byte(old)), "%s holds %s once", b, old)

	return bytes.Replace(b, []byte(old), []byte(replacement), 1)
}
