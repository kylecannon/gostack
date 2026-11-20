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
	kmsNotFoundCode    = "KMS.NotFoundException"
	// westKey is a key of another region than the buckets of these tests.
	westKey           = "arn:aws:kms:us-west-2:000000000000:key/abc"
	westRegionMessage = "Invalid arn us-west-2"
	sourcePath        = "/bucket/source"
	uploadsPath       = "/bucket/up?uploads"
	opPutObject       = "PutObject"
)

// fakeKMS answers the two KMS calls S3 makes, CreateKey and DescribeKey, for
// keys it holds by ARN. S3 reaches it through the handler it is bound to, as it
// reaches the real KMS through the gateway.
type fakeKMS struct {
	mu      sync.Mutex
	keys    map[string]*fakeKey
	created []string // the description of every CreateKey, in order
	calls   atomic.Int64
	// answers are the DescribeKey answers a test scripts by key ARN, in place of
	// the key the fake holds.
	answers map[string]fakeAnswer
}

// fakeAnswer is a whole KMS answer: its status and body.
type fakeAnswer struct {
	status int
	body   string
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
	return &fakeKMS{keys: make(map[string]*fakeKey), answers: make(map[string]fakeAnswer)}
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
		if scripted, ok := f.answers[in.KeyID]; ok {
			writeKMS(w, scripted.status, scripted.body)

			return
		}

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

// answer scripts what DescribeKey answers for the key arn.
func (f *fakeKMS) answer(arn string, status int, body string) {
	f.mu.Lock()
	f.answers[arn] = fakeAnswer{status: status, body: body}
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
		assert.Equal(t, s3Error{Code: kmsNotFoundCode, Message: westRegionMessage}, codeAndMessage(t, got))
		assert.Zero(t, k.kms.calls.Load())
	})

	t.Run("a key in another region is refused for aws:kms and kept for aws:kms:dsse", func(t *testing.T) {
		t.Parallel()

		k := newKMSStack(t)
		k.bucket("bucket")
		require.Equal(t, http.StatusOK, k.put(sourcePath, nil).status)

		surfaces := []struct {
			name  string
			write func(algorithm string) reply
		}{
			{opPutObject, func(algorithm string) reply {
				return k.put("/bucket/a", map[string]string{headerSSE: algorithm, headerSSEKMSKey: westKey})
			}},
			{"CreateMultipartUpload", func(algorithm string) reply {
				return k.do(call{
					method: http.MethodPost, target: uploadsPath, region: regionUSEast1,
					header: map[string]string{headerSSE: algorithm, headerSSEKMSKey: westKey},
				})
			}},
			{"CopyObject", func(algorithm string) reply {
				return copyTo(k.server, sourcePath, map[string]string{headerSSE: algorithm, headerSSEKMSKey: westKey})
			}},
		}

		for _, surface := range surfaces {
			refused := surface.write(sseKMS)
			require.Equal(t, http.StatusBadRequest, refused.status, surface.name+" aws:kms")
			assert.Equal(t, s3Error{Code: kmsNotFoundCode, Message: westRegionMessage},
				codeAndMessage(t, refused), surface.name+" aws:kms")

			kept := surface.write(sseKMSDSSE)
			require.Equal(t, http.StatusOK, kept.status, surface.name+" aws:kms:dsse: "+string(kept.body))
			assert.Equal(t, sseKMSDSSE, kept.header.Get(headerSSE), surface.name)
			assert.Equal(t, westKey, kept.header.Get(headerSSEKMSKey), surface.name+": the key stays as it was sent")
		}

		assert.Equal(t, westKey, get(k.server, http.MethodHead, "/bucket/a", nil).header.Get(headerSSEKMSKey))
		assert.Zero(t, k.kms.calls.Load(), "KMS is not asked")
	})

	t.Run("on: a typed 4xx KMS answer is a 400 KMS.<type>, a 5xx or an untyped one a 500", func(t *testing.T) {
		t.Parallel()

		k := newKMSStack(t)
		k.bucket("bucket")
		k.validate.Store(true)

		keyARN := func(id string) string { return "arn:aws:kms:us-east-1:000000000000:key/" + id }
		k.kms.answer(keyARN("denied"), http.StatusBadRequest,
			`{"__type":"AccessDeniedException","message":"not allowed to describe"}`)
		k.kms.answer(keyARN("namespaced"), http.StatusBadRequest,
			`{"__type":"com.amazonaws.kms#InvalidArnException","message":"bad arn"}`)
		k.kms.answer(keyARN("typed500"), http.StatusInternalServerError,
			`{"__type":"com.amazonaws.kms#KMSInternalException","message":"KMS broke"}`)
		k.kms.answer(keyARN("panic"), http.StatusInternalServerError,
			`{"__type":"InternalError","message":"handler panicked"}`)
		k.kms.answer(keyARN("typed503"), http.StatusServiceUnavailable,
			`{"__type":"DependencyTimeoutException","message":"slow"}`)
		k.kms.answer(keyARN("untyped"), http.StatusServiceUnavailable, `{"message":"no type"}`)
		k.kms.answer(keyARN("html"), http.StatusBadGateway, `<html>bad gateway</html>`)

		for id, want := range map[string]s3Error{
			"denied":     {Code: "KMS.AccessDeniedException", Message: "not allowed to describe"},
			"namespaced": {Code: "KMS.InvalidArnException", Message: "bad arn"},
		} {
			got := k.put("/bucket/a", map[string]string{headerSSE: sseKMS, headerSSEKMSKey: id})
			require.Equal(t, http.StatusBadRequest, got.status, id)
			assert.Equal(t, want, codeAndMessage(t, got), id)
		}

		for id, detail := range map[string]string{
			"typed500": "500 with error type KMSInternalException",
			"panic":    "500 with error type InternalError",
			"typed503": "503 with error type DependencyTimeoutException",
			"untyped":  "503 with no error type",
			"html":     "502 with no error type",
		} {
			got := k.put("/bucket/a", map[string]string{headerSSE: sseKMS, headerSSEKMSKey: id})
			require.Equal(t, http.StatusInternalServerError, got.status, id)
			assert.Equal(t, s3Error{
				Code:    "InternalError",
				Message: "We encountered an internal error. Please try again. DescribeKey answered status " + detail,
			}, codeAndMessage(t, got), id)
		}
	})

	t.Run("on: a key KMS answers with another error is passed on by GetObject too", func(t *testing.T) {
		t.Parallel()

		k := newKMSStack(t)
		k.bucket("bucket")
		written := k.put("/bucket/a", map[string]string{headerSSE: sseKMS, headerSSEKMSKey: "denied"})
		require.Equal(t, http.StatusOK, written.status, string(written.body))

		k.validate.Store(true)
		k.kms.answer("arn:aws:kms:us-east-1:000000000000:key/denied", http.StatusBadRequest,
			`{"__type":"AccessDeniedException","message":"not allowed to describe"}`)

		got := get(k.server, http.MethodGet, "/bucket/a", nil)
		require.Equal(t, http.StatusBadRequest, got.status)
		assert.Equal(t, s3Error{Code: "KMS.AccessDeniedException", Message: "not allowed to describe"},
			codeAndMessage(t, got))
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
				method: http.MethodPost, target: uploadsPath, region: regionUSEast1, header: missing,
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

// TestCopyEncryptionAfterSource holds a CopyObject to resolving its target's
// encryption after the source has been found and checked, as LocalStack does:
// what the encryption headers say never changes a missing source's answer, and a
// copy that fails makes no managed key.
func TestCopyEncryptionAfterSource(t *testing.T) {
	t.Parallel()

	// stack is a KMS stack with the source of the copies.
	stack := func(t *testing.T) *kmsStack {
		t.Helper()

		k := newKMSStack(t)
		k.bucket("bucket")
		require.Equal(t, http.StatusOK, k.put(sourcePath, nil).status)

		return k
	}

	t.Run("a missing source answers NoSuchKey whatever the encryption headers", func(t *testing.T) {
		t.Parallel()

		k := stack(t)

		for name, header := range map[string]map[string]string{
			"aws:kms without a key":          {headerSSE: sseKMS},
			"aws:kms in another region":      {headerSSE: sseKMS, headerSSEKMSKey: westKey},
			"aws:kms:dsse in another region": {headerSSE: sseKMSDSSE, headerSSEKMSKey: westKey},
		} {
			got := copyTo(k.server, "/bucket/none", header)
			require.Equal(t, http.StatusNotFound, got.status, name)
			assert.Equal(t, "NoSuchKey", got.err(t).Code, name)
		}

		assert.Empty(t, k.kms.descriptions(), "no KMS key was made")
	})

	t.Run("a copy whose source condition fails makes no KMS key", func(t *testing.T) {
		t.Parallel()

		k := stack(t)

		got := copyTo(k.server, sourcePath, map[string]string{headerSSE: sseKMS, headerCopyIfMatch: `"not-the-etag"`})
		require.Equal(t, http.StatusPreconditionFailed, got.status, string(got.body))
		assert.Empty(t, k.kms.descriptions(), "no KMS key was made")
	})

	t.Run("the destination's encryption is checked before the ACL and the object lock", func(t *testing.T) {
		t.Parallel()

		k := stack(t)

		badACL := map[string]string{cfgHeaderACL: cfgFakeACL}
		badLock := map[string]string{headerLockMode: "GOVERNANCE"} // a mode with no retain-until date
		withWest := map[string]string{headerSSE: sseKMS, headerSSEKMSKey: westKey}

		for name, header := range map[string]map[string]string{
			"a bad ACL":         with(withWest, badACL),
			"a bad object lock": with(withWest, badLock),
			"both":              with(with(withWest, badACL), badLock),
		} {
			got := copyTo(k.server, sourcePath, header)
			require.Equal(t, http.StatusBadRequest, got.status, name)
			assert.Equal(t, s3Error{Code: kmsNotFoundCode, Message: westRegionMessage}, codeAndMessage(t, got), name)
		}

		// With nothing wrong with the encryption, the ACL and the lock are answered,
		// the lock first, as LocalStack resolves it first.
		for name, header := range map[string]map[string]string{
			"a bad ACL":         badACL,
			"a bad object lock": badLock,
			"both":              with(badACL, badLock),
		} {
			got := copyTo(k.server, sourcePath, header)
			require.Equal(t, http.StatusBadRequest, got.status, name)
			assert.Equal(t, "InvalidArgument", got.err(t).Code, name)
		}

		both := copyTo(k.server, sourcePath, with(badACL, badLock))
		assert.Contains(t, both.err(t).Message, "must both be supplied", "the lock is answered before the ACL")

		// The source is found before any of them.
		missing := copyTo(k.server, "/bucket/none", with(withWest, badACL))
		require.Equal(t, http.StatusNotFound, missing.status)
		assert.Equal(t, "NoSuchKey", missing.err(t).Code)

		assert.Empty(t, k.kms.descriptions(), "no KMS key was made")
	})

	t.Run("a copy that succeeds makes the managed key, and one of another region is refused", func(t *testing.T) {
		t.Parallel()

		k := stack(t)

		refused := copyTo(k.server, sourcePath, map[string]string{headerSSE: sseKMS, headerSSEKMSKey: westKey})
		require.Equal(t, http.StatusBadRequest, refused.status)
		assert.Equal(t, kmsNotFoundCode, refused.err(t).Code)
		assert.Empty(t, k.kms.descriptions())

		made := copyTo(k.server, sourcePath, map[string]string{headerSSE: sseKMS})
		require.Equal(t, http.StatusOK, made.status, string(made.body))
		assert.Equal(t, "arn:aws:kms:us-east-1:000000000000:key/made-0", made.header.Get(headerSSEKMSKey))
		assert.Equal(t, []string{managedDescription}, k.kms.descriptions())
	})
}

// TestKMSKeyRegionWithValidation holds a key ARN of another region than the bucket's to
// the region check with S3_SKIP_KMS_KEY_VALIDATION off: it is refused before KMS is asked,
// on every write and on GetObject.
func TestKMSKeyRegionWithValidation(t *testing.T) {
	t.Parallel()

	k := newKMSStack(t)
	k.bucket("bucket")
	require.Equal(t, http.StatusOK, k.put(sourcePath, nil).status)

	// An aws:kms:dsse object keeps the key with the check off, to be read with it on.
	kept := k.put("/bucket/dsse", map[string]string{headerSSE: sseKMSDSSE, headerSSEKMSKey: westKey})
	require.Equal(t, http.StatusOK, kept.status, string(kept.body))

	k.validate.Store(true)

	want := s3Error{Code: kmsNotFoundCode, Message: westRegionMessage}

	for _, algorithm := range []string{sseKMS, sseKMSDSSE, sseAES256} {
		header := map[string]string{headerSSE: algorithm, headerSSEKMSKey: westKey}

		for name, got := range map[string]reply{
			opPutObject: k.put("/bucket/a", header),
			"CreateMultipartUpload": k.do(call{
				method: http.MethodPost, target: uploadsPath, region: regionUSEast1, header: header,
			}),
			"CopyObject": copyTo(k.server, sourcePath, header),
		} {
			require.Equal(t, http.StatusBadRequest, got.status, name+" "+algorithm)
			assert.Equal(t, want, codeAndMessage(t, got), name+" "+algorithm)
		}
	}

	got := get(k.server, http.MethodGet, "/bucket/dsse", nil)
	require.Equal(t, http.StatusBadRequest, got.status)
	assert.Equal(t, want, codeAndMessage(t, got), "GetObject")

	assert.Zero(t, k.kms.calls.Load(), "KMS is not asked: the region is checked before DescribeKey")
}

// TestKMSDefaultRuleKeys holds a bucket's default encryption rule to LocalStack's: an
// aws:kms rule's key is the object's, and an aws:kms:dsse rule's key is ignored.
func TestKMSDefaultRuleKeys(t *testing.T) {
	t.Parallel()

	k := newKMSStack(t)
	k.bucket("bucket")
	k.bucket("dsse")

	rule := func(algorithm string) string {
		return cfgDoc("ServerSideEncryptionConfiguration", "<Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>"+
			algorithm+"</SSEAlgorithm><KMSMasterKeyID>arn:aws:kms:us-east-1:000000000000:key/ruled"+
			"</KMSMasterKeyID></ApplyServerSideEncryptionByDefault></Rule>")
	}

	require.Equal(t, http.StatusOK, cfgPut(k.server, "/bucket?encryption", rule("aws:kms"), nil).status)
	require.Equal(t, http.StatusOK, cfgPut(k.server, "/dsse?encryption", rule(sseKMSDSSE), nil).status)

	k.validate.Store(true)

	// KMS holds no key "ruled". A write names no key, so none is checked, as in LocalStack.
	written := k.put("/bucket/a", nil)
	require.Equal(t, http.StatusOK, written.status, string(written.body))
	assert.Equal(t, "arn:aws:kms:us-east-1:000000000000:key/ruled", written.header.Get(headerSSEKMSKey))

	got := get(k.server, http.MethodGet, "/bucket/a", nil)
	require.Equal(t, http.StatusBadRequest, got.status, "the aws:kms rule's key is the object's, and KMS lacks it")
	assert.Equal(t, kmsNotFoundCode, codeAndMessage(t, got).Code)

	dsse := k.put("/dsse/a", nil)
	require.Equal(t, http.StatusOK, dsse.status, string(dsse.body))
	assert.Equal(t, sseKMSDSSE, dsse.header.Get(headerSSE))
	assert.Empty(t, dsse.header.Get(headerSSEKMSKey), "an aws:kms:dsse rule's key is ignored, as in LocalStack")

	read := get(k.server, http.MethodGet, "/dsse/a", nil)
	require.Equal(t, http.StatusOK, read.status, string(read.body))
	assert.Empty(t, read.header.Get(headerSSEKMSKey))
	assert.Equal(t, sseKMSDSSE, read.header.Get(headerSSE))
}
