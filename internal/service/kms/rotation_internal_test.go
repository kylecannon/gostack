package kms

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/kms/ciphertext"
	"github.com/stretchr/testify/require"
)

const (
	// rotateRounds is the number of keys the race rotates to the limit.
	rotateRounds = 20
	// sealers is the number of goroutines that encrypt and decrypt while one
	// rotates.
	sealers     = 4
	otherRegion = "us-west-2"
)

type sealed struct {
	blob  []byte
	plain string
}

// Test_rotate_whileEncrypting rotates a key to the limit while other
// goroutines encrypt under it, decrypt what they sealed at once, and replicate
// it: every blob made before, during and after each rotation must decrypt
// after the last one, and the replica must hold the primary's materials as
// they were at one moment. Run it under -race: a read of the material outside
// the key's lock is the defect it looks for.
func Test_rotate_whileEncrypting(t *testing.T) {
	p := New(nil)
	ctx := request.With(t.Context(), &request.Context{AccountID: "000000000000", Region: testRegion})
	west := request.With(t.Context(), &request.Context{AccountID: "000000000000", Region: otherRegion})

	for round := range rotateRounds {
		made, err := p.CreateKey(ctx, &api.CreateKeyInput{MultiRegion: new(true)})
		require.NoError(t, err)

		id := made.KeyMetadata.KeyId
		r := &raceRound{p: p, id: id}

		r.wg.Go(func() {
			for range onDemandRotationLimit {
				_, err := p.RotateKeyOnDemand(ctx, &api.RotateKeyOnDemandInput{KeyId: id})
				r.check("rotate", err)
			}
		})

		r.wg.Go(func() {
			_, err := p.ReplicateKey(ctx, &api.ReplicateKeyInput{KeyId: id, ReplicaRegion: new(otherRegion)})
			r.check("replicate", err)
		})

		for worker := range sealers {
			r.wg.Go(func() { r.seal(ctx, fmt.Sprintf("round %d worker %d", round, worker)) })
		}

		r.wg.Wait()
		require.Empty(t, r.odd)

		for _, s := range r.blobs {
			require.Equal(t, s.plain, decryptOK(ctx, p, s.blob))
		}

		// The replica copied the materials at one moment: its generations are
		// the primary's first ones, the current last.
		primary := generations(p.stateAt("000000000000", testRegion).key(*id))
		replica := generations(p.stateAt("000000000000", otherRegion).key(*id))
		require.Len(t, primary, onDemandRotationLimit+1)
		require.Equal(t, primary[:len(replica)], replica)

		first := r.blobs[0]
		if got := decryptOK(west, p, first.blob); got != first.plain {
			require.Contains(t, got, "InvalidCiphertextException")
		}
	}
}

// raceRound is one key of Test_rotate_whileEncrypting: the blobs its sealers
// made and what went wrong.
type raceRound struct {
	p  *Provider
	id *string
	wg sync.WaitGroup

	mu    sync.Mutex
	blobs []sealed
	odd   []string
}

func (r *raceRound) check(what string, err error) {
	if err == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.odd = append(r.odd, what+": "+err.Error())
}

// seal encrypts under the key, decrypts each blob at once, and keeps it.
func (r *raceRound) seal(ctx context.Context, prefix string) {
	for i := range onDemandRotationLimit {
		plain := prefix + " call " + strconv.Itoa(i)

		out, err := r.p.Encrypt(ctx, &api.EncryptInput{KeyId: r.id, Plaintext: []byte(plain)})
		if err != nil {
			r.check("encrypt", err)

			continue
		}

		if got := decryptOK(ctx, r.p, out.CiphertextBlob); got != plain {
			r.check("decrypt at once", errors.New(got))
		}

		r.mu.Lock()
		r.blobs = append(r.blobs, sealed{blob: out.CiphertextBlob, plain: plain})
		r.mu.Unlock()
	}
}

// generations are a key's materials, oldest first.
func generations(k *Key) [][]byte {
	m := k.materials()

	return append(slices.Clone(m.previous), m.current)
}

// decryptOK decrypts blob, finding the key through it, and returns the
// plaintext, or the error's text.
func decryptOK(ctx context.Context, p *Provider, blob []byte) string {
	out, err := p.Decrypt(ctx, &api.DecryptInput{CiphertextBlob: blob})
	if err != nil {
		return err.Error()
	}

	return string(out.Plaintext)
}

// Test_rotate_keepsReadersSnapshot holds rotation to never writing into a list
// a reader took: a materials() copy taken before a rotation still names the
// materials it had.
func Test_rotate_keepsReadersSnapshot(t *testing.T) {
	k := &Key{ID: "k", ARN: "arn", Spec: specSymmetric, state: stateEnabled, enabled: true}
	k.Material = fmt.Appendf(nil, "material-%029d.", 0)
	k.previous = make([][]byte, 0, onDemandRotationLimit)

	var before []keyMaterial

	for i := 1; i <= 3; i++ {
		before = append(before, k.materials())
		require.NoError(t, k.rotate(fmt.Appendf(nil, "material-%029d.", i)))
	}

	for i, m := range before {
		require.Equal(t, fmt.Sprintf("material-%029d.", i), string(m.current))
		require.Len(t, m.previous, i)
	}

	require.Len(t, k.materials().previous, 3)
}

// BenchmarkDecrypt_rotations opens a 32-byte blob on a key that rotation gave 1
// and 100 earlier materials, sealed under the current material (tried first)
// or under the newest earlier one (tried last, as LocalStack orders them).
// The API allows ten rotations; the 100 are set on the key here.
func BenchmarkDecrypt_rotations(b *testing.B) {
	for _, rotations := range []int{1, 100} {
		for _, sealedBy := range []string{"current", "last"} {
			for _, workers := range []int{1, 16} {
				name := "proto=json/payload=32B/concurrency=" + strconv.Itoa(workers) +
					"/rotations=" + strconv.Itoa(rotations) + "/material=" + sealedBy
				b.Run(name, func(b *testing.B) { benchDecryptRotated(b, rotations, sealedBy, workers) })
			}
		}
	}
}

// benchDecryptRotated times Decrypt on a key with rotations earlier materials,
// of a blob sealed under the material sealedBy names: "current" or "last".
func benchDecryptRotated(b *testing.B, rotations int, sealedBy string, workers int) {
	p := New(nil)
	ctx := request.With(b.Context(), &request.Context{AccountID: "000000000000", Region: testRegion})

	made, err := p.CreateKey(ctx, &api.CreateKeyInput{})
	if err != nil {
		b.Fatal(err)
	}

	k := p.stateAt("000000000000", testRegion).key(*made.KeyMetadata.KeyId)

	k.mu.Lock()
	for range rotations {
		k.previous = append(k.previous, k.Material)
		k.Material = make([]byte, aes256)
		_, _ = rand.Read(k.Material)
	}

	sealing := k.Material
	if sealedBy == "last" {
		sealing = k.previous[len(k.previous)-1]
	}

	k.mu.Unlock()

	blob, err := ciphertext.Seal(sealing, k.ID, []byte(strings.Repeat("x", 32)), nil, rand.Reader)
	if err != nil {
		b.Fatal(err)
	}

	g, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{"0.0.0.0:4566"}} },
		[]service.Registration{p.Registration()}, nil, nil,
	)
	if err != nil {
		b.Fatal(err)
	}

	handler := g.Handler()
	body := `{"CiphertextBlob":"` + base64.StdEncoding.EncodeToString(blob) + `"}`

	b.SetBytes(int64(32 * workers))
	b.ReportAllocs()

	for b.Loop() {
		statuses := make([]int, workers)

		var wg sync.WaitGroup

		for i := range workers {
			wg.Go(func() { statuses[i] = serveOnce(b, handler, body) })
		}

		wg.Wait()

		if slices.ContainsFunc(statuses, func(s int) bool { return s != http.StatusOK }) {
			b.Fatalf("Decrypt answered %v", statuses)
		}
	}
}

// serveOnce sends one Decrypt of body and returns its status.
func serveOnce(b *testing.B, handler http.Handler, body string) int {
	req, err := http.NewRequestWithContext(b.Context(), http.MethodPost, "http://localhost:4566/",
		strings.NewReader(body))
	if err != nil {
		return 0
	}

	req.Header = http.Header{
		"Content-Type": {"application/x-amz-json-1.1"},
		"Authorization": {"AWS4-HMAC-SHA256 Credential=test/20130524/" + testRegion +
			"/kms/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc"},
		"X-Amz-Target": {"TrentService.Decrypt"},
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	return rec.Code
}
