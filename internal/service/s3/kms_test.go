package s3_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/blob"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	managedDescription = "Default key that protects my S3 objects when no other key is defined"
	kmsAccount         = "000000000000"
	sseKMSDSSE         = "aws:kms:dsse"
	kmsStateEnabled    = "Enabled"
	fakeKeyID          = "fake-key-id"
	kmsTarget          = "TrentService."
)

// fakeKMS answers the two KMS calls S3 makes, CreateKey and DescribeKey, for
// keys it holds by ARN. S3 reaches it through the handler it is bound to, as it
// reaches the real KMS through the gateway.
type fakeKMS struct {
	mu      sync.Mutex
	keys    map[string]*fakeKey
	created []string // the description of every CreateKey, in order
	calls   atomic.Int64
}

type fakeKey struct {
	enabled bool
	state   string
}

// kmsRequest is the members of the two calls the fake reads.
type kmsRequest struct {
	KeyID       string `json:"KeyId"`
	Description string `json:"Description"`
}

func newFakeKMS() *fakeKMS {
	return &fakeKMS{keys: make(map[string]*fakeKey)}
}

func (f *fakeKMS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)

	if r.Header.Get("Content-Type") != "application/x-amz-json-1.1" {
		http.Error(w, "not json 1.1", http.StatusBadRequest)

		return
	}

	var in kmsRequest

	err := json.NewDecoder(r.Body).Decode(&in)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	// Credential=<account>/<date>/<region>/kms/aws4_request
	scope := strings.Split(strings.SplitN(r.Header.Get("Authorization"), "Credential=", 2)[1], "/")
	region := scope[2]

	f.mu.Lock()
	defer f.mu.Unlock()

	switch strings.TrimPrefix(r.Header.Get("X-Amz-Target"), kmsTarget) {
	case "CreateKey":
		arn := "arn:aws:kms:" + region + ":" + kmsAccount + ":key/made-" + strconv.Itoa(len(f.created))
		f.created = append(f.created, in.Description)
		f.keys[arn] = &fakeKey{enabled: true, state: kmsStateEnabled}
		writeKMS(w, http.StatusOK, `{"KeyMetadata":{"Arn":"`+arn+`","Enabled":true}}`)
	case "DescribeKey":
		key, ok := f.keys[in.KeyID]
		if !ok {
			writeKMS(w, http.StatusBadRequest,
				`{"__type":"NotFoundException","message":"Key '`+in.KeyID+`' does not exist"}`)

			return
		}

		writeKMS(w, http.StatusOK, `{"KeyMetadata":{"Arn":"`+in.KeyID+`","Enabled":`+
			strconv.FormatBool(key.enabled)+`,"KeyState":"`+key.state+`"}}`)
	default:
		http.Error(w, "unexpected KMS call", http.StatusBadRequest)
	}
}

// add makes a key the test names, enabled.
func (f *fakeKMS) add(arn string) {
	f.set(arn, true, kmsStateEnabled)
}

func (f *fakeKMS) set(arn string, enabled bool, state string) {
	f.mu.Lock()
	f.keys[arn] = &fakeKey{enabled: enabled, state: state}
	f.mu.Unlock()
}

func (f *fakeKMS) descriptions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.created...)
}

func writeKMS(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// bindKMS binds S3's calls to h, with those to KMS answered by fake.
func bindKMS(provider *s3.Provider, h http.Handler, fake *fakeKMS) {
	provider.Bind(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("X-Amz-Target"), kmsTarget) {
			fake.ServeHTTP(w, r)

			return
		}

		h.ServeHTTP(w, r)
	}))
}

// kmsStack is S3 bound to a gateway whose KMS calls the fake answers.
type kmsStack struct {
	*server

	s3       *s3.Provider
	kms      *fakeKMS
	validate *atomic.Bool
}

func newKMSStack(t *testing.T) *kmsStack {
	t.Helper()

	validate := new(atomic.Bool)
	provider := s3.New(func() s3.Settings {
		return s3.Settings{LocalstackHost: localstackHost, ValidateKMSKeys: validate.Load()}
	}, blob.NewEphemeral(t.TempDir()))
	t.Cleanup(func() { provider.Close(t.Context()) })

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{benchListen}} },
		[]service.Registration{provider.Registration()},
		nil,
		nil,
	)
	require.NoError(t, err)

	fake := newFakeKMS()
	bindKMS(provider, gw.Handler(), fake)

	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)

	return &kmsStack{server: &server{t: t, url: srv.URL}, s3: provider, kms: fake, validate: validate}
}

func (k *kmsStack) put(target string, header map[string]string) reply {
	k.t.Helper()

	return k.do(call{method: http.MethodPut, target: target, region: regionUSEast1, body: []byte("body"), header: header})
}

func (k *kmsStack) bucket(name string) {
	k.t.Helper()

	got := k.do(call{method: http.MethodPut, target: "/" + name, region: regionUSEast1})
	require.Equal(k.t, http.StatusOK, got.status, string(got.body))
}

// codeAndMessage is the code and message of an S3 error answer.
func codeAndMessage(t *testing.T, r reply) s3Error {
	t.Helper()

	e := r.err(t)

	return s3Error{Code: e.Code, Message: e.Message}
}

func TestManagedKMSKey(t *testing.T) {
	t.Parallel()

	t.Run("an aws:kms write without a key gets one key KMS made, on every surface", func(t *testing.T) {
		t.Parallel()

		k := newKMSStack(t)
		k.bucket("bucket")
		k.bucket("other")

		first := k.put("/bucket/a", map[string]string{headerSSE: sseKMS})
		require.Equal(t, http.StatusOK, first.status, string(first.body))

		arn := first.header.Get(headerSSEKMSKey)
		assert.Equal(t, "arn:aws:kms:us-east-1:000000000000:key/made-0", arn)
		assert.Equal(t, []string{managedDescription}, k.kms.descriptions(), "CreateKey with LocalStack's description")

		assert.Equal(t, arn, k.put("/other/b", map[string]string{headerSSE: sseKMS}).header.Get(headerSSEKMSKey),
			"another bucket of the account and region shares the key")
		assert.Equal(t, arn, get(k.server, http.MethodHead, "/bucket/a", nil).header.Get(headerSSEKMSKey))

		copied := k.do(call{method: http.MethodPut, target: "/other/copy", region: regionUSEast1, header: map[string]string{
			"X-Amz-Copy-Source": "/bucket/a", headerSSE: sseKMS,
		}})
		require.Equal(t, http.StatusOK, copied.status, string(copied.body))
		assert.Equal(t, arn, copied.header.Get(headerSSEKMSKey), "CopyObject")

		upload := k.do(call{
			method: http.MethodPost, target: "/bucket/upload?uploads", region: regionUSEast1,
			header: map[string]string{headerSSE: sseKMS},
		})
		require.Equal(t, http.StatusOK, upload.status, string(upload.body))
		assert.Equal(t, arn, upload.header.Get(headerSSEKMSKey), "CreateMultipartUpload")

		config := cfgDoc("ServerSideEncryptionConfiguration", "<Rule><ApplyServerSideEncryptionByDefault>"+
			"<SSEAlgorithm>aws:kms</SSEAlgorithm></ApplyServerSideEncryptionByDefault></Rule>")
		require.Equal(t, http.StatusOK, cfgPut(k.server, "/other?encryption", config, nil).status)
		assert.Equal(t, arn, k.put("/other/default", nil).header.Get(headerSSEKMSKey), "the bucket's default")

		assert.Len(t, k.kms.descriptions(), 1, "one key for the account and region")
	})

	t.Run("a POST upload and a presigned PUT get it too", func(t *testing.T) {
		t.Parallel()

		k := newKMSStack(t)
		k.bucket("bucket")

		arn := k.put("/bucket/a", map[string]string{headerSSE: sseKMS}).header.Get(headerSSEKMSKey)

		posted := postForm(t, k.server, localstackHost, "/bucket", [][2]string{
			{fieldKey, "posted"}, {"x-amz-server-side-encryption", sseKMS},
		}, []byte("body"))
		require.Equal(t, http.StatusNoContent, posted.status, string(posted.body))
		assert.Equal(t, arn, posted.header.Get(headerSSEKMSKey), "POST")

		presigned := k.do(call{
			method: http.MethodPut, target: "/bucket/presigned?" +
				signV4(http.MethodPut, localstackHost, "/bucket/presigned", time.Now()),
			body: []byte("body"), header: map[string]string{headerSSE: sseKMS},
		})
		require.Equal(t, http.StatusOK, presigned.status, string(presigned.body))
		assert.Equal(t, arn, presigned.header.Get(headerSSEKMSKey), "presigned PUT")
	})

	t.Run("a gateway that serves no KMS checks nothing and derives the key", func(t *testing.T) {
		t.Parallel()

		provider := s3.New(func() s3.Settings {
			return s3.Settings{LocalstackHost: localstackHost, ValidateKMSKeys: true}
		}, blob.NewEphemeral(t.TempDir()))
		t.Cleanup(func() { provider.Close(t.Context()) })

		gw, err := gateway.New(
			func() gateway.Settings { return gateway.Settings{Listen: []string{benchListen}} },
			[]service.Registration{provider.Registration()},
			nil,
			nil,
		)
		require.NoError(t, err)

		provider.Bind(gw.Handler())

		srv := httptest.NewServer(gw.Handler())
		t.Cleanup(srv.Close)

		k := &kmsStack{server: &server{t: t, url: srv.URL}, s3: provider}
		k.bucket("bucket")

		managed := k.put("/bucket/a", map[string]string{headerSSE: sseKMS})
		require.Equal(t, http.StatusOK, managed.status, string(managed.body))
		assert.True(t, strings.HasPrefix(managed.header.Get(headerSSEKMSKey), managedARNPrefix))
		assert.Equal(t, managed.header.Get(headerSSEKMSKey),
			k.put("/bucket/b", map[string]string{headerSSE: sseKMS}).header.Get(headerSSEKMSKey))

		named := k.put("/bucket/c", map[string]string{headerSSE: sseKMS, headerSSEKMSKey: fakeKeyID})
		require.Equal(t, http.StatusOK, named.status, string(named.body))
		assert.Equal(t, managedARNPrefix+fakeKeyID, named.header.Get(headerSSEKMSKey))
		assert.Equal(t, http.StatusOK, get(k.server, http.MethodGet, "/bucket/c", nil).status)
	})

	t.Run("aws:kms:dsse gets no managed key, as in LocalStack", func(t *testing.T) {
		t.Parallel()

		k := newKMSStack(t)
		k.bucket("bucket")

		got := k.put("/bucket/a", map[string]string{headerSSE: sseKMSDSSE})
		require.Equal(t, http.StatusOK, got.status, string(got.body))
		assert.Equal(t, sseKMSDSSE, got.header.Get(headerSSE))
		assert.Empty(t, got.header.Get(headerSSEKMSKey))
		assert.Empty(t, k.kms.descriptions())
	})

	t.Run("concurrent first writes make one key", func(t *testing.T) {
		t.Parallel()

		k := newKMSStack(t)
		k.bucket("bucket")

		const writers = 8

		arns := make([]string, writers)

		var wg sync.WaitGroup

		for i := range writers {
			wg.Go(func() {
				arns[i] = k.put("/bucket/k"+strconv.Itoa(i), map[string]string{headerSSE: sseKMS}).header.Get(headerSSEKMSKey)
			})
		}

		wg.Wait()

		assert.Len(t, k.kms.descriptions(), 1)

		for _, arn := range arns {
			assert.Equal(t, arns[0], arn)
		}
	})

	t.Run("the key survives a snapshot and goes with a reset", func(t *testing.T) {
		t.Parallel()

		k := newKMSStack(t)
		k.bucket("bucket")

		arn := k.put("/bucket/a", map[string]string{headerSSE: sseKMS}).header.Get(headerSSEKMSKey)

		var saved bytes.Buffer
		require.NoError(t, k.s3.Snapshot(&saved))
		assert.Contains(t, saved.String(), `"managedKMSKeys":[{"Account":"000000000000","Region":"us-east-1","ARN":"`+arn)

		k.s3.Reset("", "")
		require.NoError(t, k.s3.Restore(bytes.NewReader(saved.Bytes())))

		assert.Equal(t, arn, k.put("/bucket/b", map[string]string{headerSSE: sseKMS}).header.Get(headerSSEKMSKey))
		assert.Len(t, k.kms.descriptions(), 1, "the restored key is used, none is made")

		k.s3.Reset(kmsAccount, regionUSEast1)
		k.bucket("bucket")
		assert.NotEqual(t, arn, k.put("/bucket/c", map[string]string{headerSSE: sseKMS}).header.Get(headerSSEKMSKey))
		assert.Len(t, k.kms.descriptions(), 2, "a reset forgets the key, so the next write makes one")
	})
}

func TestKMSKeyValidation(t *testing.T) {
	t.Parallel()

	const (
		known  = "arn:aws:kms:us-east-1:000000000000:key/known"
		bareID = "known"
	)

	t.Run("skipped by default: any key is taken and KMS is not asked", func(t *testing.T) {
		t.Parallel()

		k := newKMSStack(t)
		k.bucket("bucket")

		got := k.put("/bucket/a", map[string]string{headerSSE: sseKMS, headerSSEKMSKey: fakeKeyID})
		require.Equal(t, http.StatusOK, got.status, string(got.body))
		assert.Equal(t, "arn:aws:kms:us-east-1:000000000000:key/fake-key-id", got.header.Get(headerSSEKMSKey))
		assert.Zero(t, k.kms.calls.Load())
	})

	t.Run("a key in another region is refused without asking KMS", func(t *testing.T) {
		t.Parallel()

		k := newKMSStack(t)
		k.bucket("bucket")

		got := k.put("/bucket/a", map[string]string{
			headerSSE: sseKMS, headerSSEKMSKey: "arn:aws:kms:us-west-2:000000000000:key/abc",
		})
		require.Equal(t, http.StatusBadRequest, got.status)
		assert.Equal(t, s3Error{Code: "KMS.NotFoundException", Message: "Invalid arn us-west-2"}, codeAndMessage(t, got))
		assert.Zero(t, k.kms.calls.Load())
	})

	t.Run("on: a missing, disabled or deleted key is refused on every write and on GetObject", func(t *testing.T) {
		t.Parallel()

		k := newKMSStack(t)
		k.bucket("bucket")
		k.kms.add(known)
		k.validate.Store(true)

		missing := map[string]string{headerSSE: sseKMS, headerSSEKMSKey: fakeKeyID}
		notFound := s3Error{
			Code: "KMS.NotFoundException", Message: "Key 'arn:aws:kms:us-east-1:000000000000:key/fake-key-id' does not exist",
		}

		for name, got := range map[string]reply{
			"put": k.put("/bucket/a", missing),
			"multipart": k.do(call{
				method: http.MethodPost, target: "/bucket/up?uploads", region: regionUSEast1, header: missing,
			}),
			"copy, before the source is looked up": k.do(call{
				method: http.MethodPut, target: "/bucket/copy", region: regionUSEast1,
				header: map[string]string{"X-Amz-Copy-Source": "/bucket/none", headerSSE: sseKMS, headerSSEKMSKey: fakeKeyID},
			}),
			"AES256 with a key id": k.put("/bucket/a", map[string]string{headerSSE: sseAES256, headerSSEKMSKey: fakeKeyID}),
		} {
			require.Equal(t, http.StatusBadRequest, got.status, name)
			assert.Equal(t, notFound, codeAndMessage(t, got), name)
		}

		posted := postForm(t, k.server, localstackHost, "/bucket", [][2]string{
			{fieldKey, "posted"},
			{"x-amz-server-side-encryption", sseKMS},
			{"x-amz-server-side-encryption-aws-kms-key-id", fakeKeyID},
		}, []byte("body"))
		assert.Equal(t, http.StatusNoContent, posted.status, "a POST's key is not checked, as in LocalStack")

		ok := k.put("/bucket/a", map[string]string{headerSSE: sseKMS, headerSSEKMSKey: bareID})
		require.Equal(t, http.StatusOK, ok.status, string(ok.body))
		assert.Equal(t, known, ok.header.Get(headerSSEKMSKey))

		k.kms.set(known, false, "Disabled")

		disabled := s3Error{Code: "KMS.DisabledException", Message: known + " is disabled."}
		for name, got := range map[string]reply{
			"put": k.put("/bucket/b", map[string]string{headerSSE: sseKMS, headerSSEKMSKey: bareID}),
			"get": get(k.server, http.MethodGet, "/bucket/a", nil),
		} {
			require.Equal(t, http.StatusBadRequest, got.status, name)
			assert.Equal(t, disabled, codeAndMessage(t, got), name)
		}

		assert.Equal(t, http.StatusOK, get(k.server, http.MethodHead, "/bucket/a", nil).status,
			"HeadObject does not check the key")

		k.kms.set(known, false, "PendingDeletion")

		got := get(k.server, http.MethodGet, "/bucket/a", nil)
		require.Equal(t, http.StatusBadRequest, got.status)
		assert.Equal(t, s3Error{Code: "KMS.KMSInvalidStateException", Message: known + " is pending deletion."},
			codeAndMessage(t, got))

		k.validate.Store(false)
		assert.Equal(t, http.StatusOK, get(k.server, http.MethodGet, "/bucket/a", nil).status,
			"turning the check off reads the object again")
	})
}
