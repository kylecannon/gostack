package s3_test

import (
	"crypto/md5"
	"encoding/base64"
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	headerSSE          = "X-Amz-Server-Side-Encryption"
	headerSSEKMSKey    = "X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id"
	headerSSEKMSCtx    = "X-Amz-Server-Side-Encryption-Context"
	headerBucketKey    = "X-Amz-Server-Side-Encryption-Bucket-Key-Enabled"
	headerCustomerAlgo = "X-Amz-Server-Side-Encryption-Customer-Algorithm"
	headerCustomerKey  = "X-Amz-Server-Side-Encryption-Customer-Key"
	headerCustomerMD5  = "X-Amz-Server-Side-Encryption-Customer-Key-Md5"
	headerSourceAlgo   = "X-Amz-Copy-Source-Server-Side-Encryption-Customer-Algorithm"
	headerSourceKey    = "X-Amz-Copy-Source-Server-Side-Encryption-Customer-Key"
	headerSourceMD5    = "X-Amz-Copy-Source-Server-Side-Encryption-Customer-Key-Md5"

	headerAttributes = "X-Amz-Object-Attributes"

	sseAES256        = "AES256"
	sseKMS           = "aws:kms"
	notAnAlgorithm   = "KMS"
	valueTrue        = "true"
	codeInvalidArg   = "InvalidArgument"
	codeInvalidReq   = "InvalidRequest"
	codeAccessDenied = "AccessDenied"
	managedARNPrefix = "arn:aws:kms:us-east-1:000000000000:key/"

	msgKeyForRead   = "The object was stored using a form of Server Side Encryption. "
	msgDifferentKey = "Requests specifying Server Side Encryption with Customer provided keys " +
		"must provide the correct secret key."
	msgNoMD5Match     = "The calculated MD5 hash of the key did not match the hash that was provided."
	msgBadKeySize     = "The secret key was invalid for the specified algorithm."
	msgPartsNeedKey   = "The multipart upload initiate requested encryption. "
	msgPartsDifferent = "The provided encryption parameters did not match the ones used originally."
)

// customer is an SSE-C key: its base64, and the base64 of its MD5.
type customer struct{ key, md5 string }

func customerOf(raw string) customer {
	sum := md5.Sum([]byte(raw))

	return customer{
		key: base64.StdEncoding.EncodeToString([]byte(raw)),
		md5: base64.StdEncoding.EncodeToString(sum[:]),
	}
}

var (
	keyOne = customerOf("1234567890abcdef1234567890abcdef")
	keyTwo = customerOf("abcdef1234567890abcdef1234567890")
)

// headers is the SSE-C request headers of the key.
func (c customer) headers() map[string]string {
	return map[string]string{headerCustomerAlgo: sseAES256, headerCustomerKey: c.key, headerCustomerMD5: c.md5}
}

// sourceHeaders is the same for a copy's source.
func (c customer) sourceHeaders() map[string]string {
	return map[string]string{headerSourceAlgo: sseAES256, headerSourceKey: c.key, headerSourceMD5: c.md5}
}

func with(header map[string]string, more map[string]string) map[string]string {
	merged := make(map[string]string, len(header)+len(more))
	maps.Copy(merged, header)
	maps.Copy(merged, more)

	return merged
}

// expectError asserts an S3 error reply of the status, code and message.
func expectError(t *testing.T, got reply, status int, code, message string) {
	t.Helper()

	require.Equal(t, status, got.status, string(got.body))

	e := got.err(t)
	assert.Equal(t, code, e.Code)
	assert.Contains(t, e.Message, message)
}

func TestSSECWritesAreValidated(t *testing.T) {
	const (
		versionMsg = "Server Side Encryption with Customer provided key is incompatible with the encryption method specified"
		noAlgoMsg  = "must provide a valid encryption algorithm."
		noKeyMsg   = "must provide an appropriate secret key."
		algoMsg    = "The Encryption request you specified is not valid. Supported value: AES256."
	)

	short := customerOf("1234567890")
	flipped := keyOne.md5
	if flipped[0] == 'a' {
		flipped = "b" + flipped[1:]
	} else {
		flipped = "a" + flipped[1:]
	}

	tests := []struct {
		name    string
		header  map[string]string
		wantErr string
		wantMsg string
	}{
		{"both encryption methods", with(with(keyOne.headers(), map[string]string{headerCustomerAlgo: notAnAlgorithm}),
			map[string]string{headerSSE: sseAES256}), codeInvalidArg, versionMsg},
		{
			"a wrong algorithm", with(keyOne.headers(), map[string]string{headerCustomerAlgo: notAnAlgorithm}),
			"InvalidEncryptionAlgorithmError", algoMsg,
		},
		{
			"no algorithm",
			map[string]string{headerCustomerKey: keyOne.key, headerCustomerMD5: keyOne.md5},
			codeInvalidArg, noAlgoMsg,
		},
		{
			"no key",
			map[string]string{headerCustomerAlgo: sseAES256, headerCustomerMD5: keyOne.md5},
			codeInvalidArg, noKeyMsg,
		},
		{"a key of the wrong size", short.headers(), codeInvalidArg, msgBadKeySize},
		{
			"a key without its MD5",
			map[string]string{headerCustomerAlgo: sseAES256, headerCustomerKey: keyOne.key},
			codeInvalidArg, msgNoMD5Match,
		},
		{
			"an MD5 that is not the key's", with(keyOne.headers(), map[string]string{headerCustomerMD5: flipped}),
			codeInvalidArg, msgNoMD5Match,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newServer(t)
			createBucket(t, s)

			got := s.do(call{
				method: http.MethodPut, target: objectPath, region: regionUSEast1, body: []byte(objectBody),
				header: tt.header,
			})
			expectError(t, got, http.StatusBadRequest, tt.wantErr, tt.wantMsg)

			assert.Equal(t, http.StatusNotFound, get(s, http.MethodHead, objectPath, nil).status,
				"a refused write stores nothing")
		})
	}
}

func TestSSECObjectRetrieval(t *testing.T) {
	s := newServer(t)
	createBucket(t, s)

	written := s.do(call{
		method: http.MethodPut, target: objectPath, region: regionUSEast1, body: []byte(objectBody),
		header: keyOne.headers(),
	})
	require.Equal(t, http.StatusOK, written.status, string(written.body))
	assert.Equal(t, sseAES256, written.header.Get(headerCustomerAlgo))
	assert.Equal(t, keyOne.md5, written.header.Get(headerCustomerMD5))
	assert.Empty(t, written.header.Get(headerSSE), "SSE-C replaces the bucket's default encryption")

	t.Run("the key reads it, and is echoed", func(t *testing.T) {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			got := get(s, method, objectPath, keyOne.headers())
			require.Equal(t, http.StatusOK, got.status, method)
			assert.Equal(t, sseAES256, got.header.Get(headerCustomerAlgo), method)
			assert.Equal(t, keyOne.md5, got.header.Get(headerCustomerMD5), method)
			assert.Empty(t, got.header.Get(headerSSE), method)
		}

		assert.Equal(t, objectBody, string(get(s, http.MethodGet, objectPath, keyOne.headers()).body))
	})

	t.Run("no key is refused", func(t *testing.T) {
		got := get(s, http.MethodGet, objectPath, nil)
		expectError(t, got, http.StatusBadRequest, codeInvalidReq, msgKeyForRead)
		assert.Equal(t, http.StatusBadRequest, get(s, http.MethodHead, objectPath, nil).status)
	})

	t.Run("another key is refused", func(t *testing.T) {
		got := get(s, http.MethodGet, objectPath, keyTwo.headers())
		expectError(t, got, http.StatusForbidden, codeAccessDenied, msgDifferentKey)
		assert.Equal(t, http.StatusForbidden, get(s, http.MethodHead, objectPath, keyTwo.headers()).status)
	})

	t.Run("the key is validated before it is used", func(t *testing.T) {
		wrongAlgo := with(keyOne.headers(), map[string]string{headerCustomerAlgo: notAnAlgorithm})
		expectError(t, get(s, http.MethodGet, objectPath, wrongAlgo), http.StatusBadRequest,
			"InvalidEncryptionAlgorithmError", "Supported value: AES256.")
		assert.Equal(t, http.StatusBadRequest, get(s, http.MethodHead, objectPath, wrongAlgo).status)

		noMD5 := map[string]string{headerCustomerAlgo: sseAES256, headerCustomerKey: keyOne.key}
		expectError(t, get(s, http.MethodGet, objectPath, noMD5), http.StatusBadRequest, codeInvalidReq, msgKeyForRead)
	})

	t.Run("GetObjectAttributes asks for the key too", func(t *testing.T) {
		attributes := map[string]string{headerAttributes: "ETag"}
		target := objectPath + "?attributes"

		expectError(t, get(s, http.MethodGet, target, attributes), http.StatusBadRequest, codeInvalidReq, msgKeyForRead)
		expectError(t, get(s, http.MethodGet, target, with(attributes, keyTwo.headers())), http.StatusForbidden,
			codeAccessDenied, "Access Denied")

		got := get(s, http.MethodGet, target, with(attributes, keyOne.headers()))
		require.Equal(t, http.StatusOK, got.status, string(got.body))
	})
}

func TestObjectWithoutSSECIgnoresNoKey(t *testing.T) {
	s := newServer(t)
	seed(t, s, nil)

	assert.Equal(t, http.StatusOK, get(s, http.MethodGet, objectPath, nil).status)
	assert.Equal(t, sseAES256, get(s, http.MethodHead, objectPath, nil).header.Get(headerSSE),
		"a bucket's default encryption is SSE-S3")
}

func TestSSECCopy(t *testing.T) {
	newCopyServer := func(t *testing.T) *server {
		t.Helper()

		s := newServer(t)
		createBucket(t, s)
		put(t, s, objectPath, objectBody, keyOne.headers())

		return s
	}

	t.Run("a copy to a target that is not encrypted takes the bucket's default", func(t *testing.T) {
		s := newCopyServer(t)

		got := copyTo(s, sourceRef, keyOne.sourceHeaders())
		require.Equal(t, http.StatusOK, got.status, string(got.body))
		assert.Equal(t, sseAES256, got.header.Get(headerSSE))
		assert.Empty(t, got.header.Get(headerCustomerMD5))

		read := get(s, http.MethodGet, destPath, nil)
		require.Equal(t, http.StatusOK, read.status)
		assert.Equal(t, objectBody, string(read.body))
	})

	t.Run("a copy to an SSE-C target needs the key to read", func(t *testing.T) {
		s := newCopyServer(t)

		got := copyTo(s, sourceRef, with(keyOne.sourceHeaders(), keyTwo.headers()))
		require.Equal(t, http.StatusOK, got.status, string(got.body))
		assert.Equal(t, sseAES256, got.header.Get(headerCustomerAlgo))
		assert.Equal(t, keyTwo.md5, got.header.Get(headerCustomerMD5))
		assert.Empty(t, got.header.Get(headerSSE))

		expectError(t, get(s, http.MethodGet, destPath, nil), http.StatusBadRequest, codeInvalidReq, msgKeyForRead)
		assert.Equal(t, http.StatusOK, get(s, http.MethodGet, destPath, keyTwo.headers()).status)
	})

	t.Run("an SSE-C source needs its key", func(t *testing.T) {
		s := newCopyServer(t)

		expectError(t, copyTo(s, sourceRef, keyTwo.headers()), http.StatusBadRequest, codeInvalidReq, msgKeyForRead)
		expectError(t, copyTo(s, sourceRef, keyTwo.sourceHeaders()), http.StatusForbidden,
			codeAccessDenied, "Access Denied")

		wrongAlgo := with(keyOne.sourceHeaders(), map[string]string{headerSourceAlgo: notAnAlgorithm})
		expectError(t, copyTo(s, sourceRef, wrongAlgo), http.StatusBadRequest,
			"InvalidEncryptionAlgorithmError", "Supported value: AES256.")
	})

	t.Run("the target's own parameters are validated", func(t *testing.T) {
		s := newCopyServer(t)

		target := with(keyTwo.headers(), map[string]string{headerCustomerAlgo: notAnAlgorithm})
		wrongAlgo := with(keyOne.sourceHeaders(), target)
		expectError(t, copyTo(s, sourceRef, wrongAlgo), http.StatusBadRequest,
			"InvalidEncryptionAlgorithmError", "Supported value: AES256.")

		both := with(keyOne.sourceHeaders(), with(keyTwo.headers(), map[string]string{headerSSE: sseAES256}))
		expectError(t, copyTo(s, sourceRef, both), http.StatusBadRequest, codeInvalidArg, "incompatible")
	})
}

func TestSSECMultipart(t *testing.T) {
	part := []byte(strings.Repeat("a", partSize+1))

	t.Run("an upload with a key takes parts with the same key", func(t *testing.T) {
		s := newServer(t)
		createBucket(t, s)

		created := s.do(call{
			method: http.MethodPost, target: multipartKey + "?uploads", region: regionUSEast1,
			header: keyOne.headers(),
		})
		require.Equal(t, http.StatusOK, created.status, string(created.body))
		assert.Equal(t, sseAES256, created.header.Get(headerCustomerAlgo))
		assert.Equal(t, keyOne.md5, created.header.Get(headerCustomerMD5))

		id := decodeXML[initiated](t, created).UploadID

		sent := s.do(call{
			method: http.MethodPut, target: multipartKey + "?partNumber=1&uploadId=" + id, region: regionUSEast1,
			body: part, header: keyOne.headers(),
		})
		require.Equal(t, http.StatusOK, sent.status, string(sent.body))
		assert.Equal(t, sseAES256, sent.header.Get(headerCustomerAlgo))
		assert.Equal(t, keyOne.md5, sent.header.Get(headerCustomerMD5))

		done := complete(s, id, completeBody([]string{sent.header.Get(headerETag)}), nil)
		require.Equal(t, http.StatusOK, done.status, string(done.body))
		assert.Empty(t, done.header.Get(headerCustomerMD5))
		assert.NotContains(t, string(done.body), "Checksum", "an SSE-C upload gets no default checksum")

		expectError(t, get(s, http.MethodGet, multipartKey, nil), http.StatusBadRequest, codeInvalidReq, msgKeyForRead)

		read := get(s, http.MethodGet, multipartKey, keyOne.headers())
		require.Equal(t, http.StatusOK, read.status)
		assert.Equal(t, keyOne.md5, read.header.Get(headerCustomerMD5))
		assert.Empty(t, read.header.Get("X-Amz-Checksum-Crc64nvme"))
		assert.Empty(t, read.header.Get("X-Amz-Checksum-Type"))
	})

	t.Run("parts must carry the key the upload has, or none when it has none", func(t *testing.T) {
		s := newServer(t)
		createBucket(t, s)

		plain := startUpload(t, s, multipartKey, nil)
		sendPart := func(id string, header map[string]string) reply {
			return s.do(call{
				method: http.MethodPut, target: multipartKey + "?partNumber=1&uploadId=" + id, region: regionUSEast1,
				body: []byte("testbody"), header: header,
			})
		}

		expectError(t, sendPart(plain, keyOne.headers()), http.StatusBadRequest, codeInvalidReq, msgPartsNeedKey)

		keyed := s.do(call{
			method: http.MethodPost, target: multipartKey + "?uploads", region: regionUSEast1,
			header: keyOne.headers(),
		})
		require.Equal(t, http.StatusOK, keyed.status)

		id := decodeXML[initiated](t, keyed).UploadID

		expectError(t, sendPart(id, nil), http.StatusBadRequest, codeInvalidReq, msgPartsNeedKey)
		expectError(t, sendPart(id, keyTwo.headers()), http.StatusBadRequest, codeInvalidReq, msgPartsDifferent)
		assert.Equal(t, http.StatusOK, sendPart(id, keyOne.headers()).status)
	})

	t.Run("the upload's own parameters are validated", func(t *testing.T) {
		s := newServer(t)
		createBucket(t, s)

		got := s.do(call{
			method: http.MethodPost, target: multipartKey + "?uploads", region: regionUSEast1,
			header: with(keyOne.headers(), map[string]string{headerSSE: sseAES256}),
		})
		expectError(t, got, http.StatusBadRequest, codeInvalidArg, "incompatible")
	})
}

func TestDefaultEncryptionIsEchoed(t *testing.T) {
	t.Run("an object takes the bucket's default, SSE-S3 for a new bucket", func(t *testing.T) {
		s := newServer(t)
		createBucket(t, s)

		written := s.do(call{method: http.MethodPut, target: objectPath, region: regionUSEast1, body: []byte(objectBody)})
		assert.Equal(t, sseAES256, written.header.Get(headerSSE))
		assert.Empty(t, written.header.Get(headerSSEKMSKey))
		assert.Empty(t, written.header.Get(headerBucketKey))

		for _, method := range []string{http.MethodGet, http.MethodHead} {
			assert.Equal(t, sseAES256, get(s, method, objectPath, nil).header.Get(headerSSE), method)
		}
	})

	t.Run("a deleted default leaves new objects unencrypted", func(t *testing.T) {
		s := newServer(t)
		createBucket(t, s)

		require.Equal(t, http.StatusNoContent, cfgDelete(s, "/bucket?encryption").status)

		written := s.do(call{method: http.MethodPut, target: objectPath, region: regionUSEast1, body: []byte(objectBody)})
		assert.Empty(t, written.header.Get(headerSSE))
		assert.Empty(t, get(s, http.MethodHead, objectPath, nil).header.Get(headerSSE))
	})

	t.Run("a bucket default of aws:kms gives the object the key and the bucket key", func(t *testing.T) {
		s := newServer(t)
		createBucket(t, s)

		config := cfgDoc("ServerSideEncryptionConfiguration", "<Rule><ApplyServerSideEncryptionByDefault>"+
			"<SSEAlgorithm>aws:kms</SSEAlgorithm><KMSMasterKeyID>my-key-id</KMSMasterKeyID>"+
			"</ApplyServerSideEncryptionByDefault><BucketKeyEnabled>true</BucketKeyEnabled></Rule>")
		require.Equal(t, http.StatusOK, cfgPut(s, "/bucket?encryption", config, nil).status)

		written := s.do(call{method: http.MethodPut, target: objectPath, region: regionUSEast1, body: []byte(objectBody)})
		assert.Equal(t, sseKMS, written.header.Get(headerSSE))
		assert.Equal(t, "arn:aws:kms:us-east-1:000000000000:key/my-key-id", written.header.Get(headerSSEKMSKey))
		assert.Equal(t, valueTrue, written.header.Get(headerBucketKey))

		for _, method := range []string{http.MethodGet, http.MethodHead} {
			got := get(s, method, objectPath, nil)
			assert.Equal(t, sseKMS, got.header.Get(headerSSE), method)
			assert.Equal(t, "arn:aws:kms:us-east-1:000000000000:key/my-key-id", got.header.Get(headerSSEKMSKey), method)
			assert.Equal(t, valueTrue, got.header.Get(headerBucketKey), method)
		}
	})

	t.Run("a request names its own key and context; no key id means the managed key", func(t *testing.T) {
		s := newServer(t)
		createBucket(t, s)

		const context = "eyJhIjoiYiJ9"

		named := s.do(call{
			method: http.MethodPut, target: objectPath, region: regionUSEast1, body: []byte(objectBody),
			header: map[string]string{
				headerSSE: sseKMS, headerSSEKMSKey: "arn:aws:kms:us-east-1:000000000000:key/abc",
				headerBucketKey: valueTrue, headerSSEKMSCtx: context,
			},
		})
		require.Equal(t, http.StatusOK, named.status, string(named.body))
		assert.Equal(t, sseKMS, named.header.Get(headerSSE))
		assert.Equal(t, "arn:aws:kms:us-east-1:000000000000:key/abc", named.header.Get(headerSSEKMSKey))
		assert.Equal(t, valueTrue, named.header.Get(headerBucketKey))
		assert.Equal(t, context, named.header.Get(headerSSEKMSCtx))

		managed := s.do(call{
			method: http.MethodPut, target: "/bucket/other", region: regionUSEast1,
			body: []byte(objectBody), header: map[string]string{headerSSE: sseKMS},
		})
		require.Equal(t, http.StatusOK, managed.status, string(managed.body))

		key := managed.header.Get(headerSSEKMSKey)
		assert.True(t, strings.HasPrefix(key, "arn:aws:kms:us-east-1:000000000000:key/"), key)
		assert.Empty(t, managed.header.Get(headerBucketKey), "S3 leaves the bucket key out unless it is on")

		again := s.do(call{
			method: http.MethodPut, target: "/bucket/third", region: regionUSEast1,
			body: []byte(objectBody), header: map[string]string{headerSSE: sseKMS},
		})
		assert.Equal(t, key, again.header.Get(headerSSEKMSKey), "the managed key does not change")
	})

	t.Run("a copy and a multipart upload echo the key; a copy takes no bucket key from the bucket", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, nil)

		kms := map[string]string{headerSSE: sseKMS, headerSSEKMSKey: "k1", headerBucketKey: valueTrue}

		copied := copyTo(s, sourceRef, kms)
		require.Equal(t, http.StatusOK, copied.status, string(copied.body))
		assert.Equal(t, sseKMS, copied.header.Get(headerSSE))
		assert.Equal(t, "arn:aws:kms:us-east-1:000000000000:key/k1", copied.header.Get(headerSSEKMSKey))
		assert.Equal(t, valueTrue, copied.header.Get(headerBucketKey))

		id := startUpload(t, s, multipartKey, kms)

		sent := s.do(call{
			method: http.MethodPut, target: multipartKey + "?partNumber=1&uploadId=" + id, region: regionUSEast1,
			body: []byte("small"),
		})
		require.Equal(t, http.StatusOK, sent.status, string(sent.body))
		assert.Equal(t, sseKMS, sent.header.Get(headerSSE))
		assert.Equal(t, "arn:aws:kms:us-east-1:000000000000:key/k1", sent.header.Get(headerSSEKMSKey))

		done := complete(s, id, completeBody([]string{sent.header.Get(headerETag)}), nil)
		require.Equal(t, http.StatusOK, done.status, string(done.body))
		assert.Contains(t, string(done.body), "<ETag>")
		assert.Equal(t, sseKMS, done.header.Get(headerSSE))
		assert.Equal(t, "arn:aws:kms:us-east-1:000000000000:key/k1", done.header.Get(headerSSEKMSKey))
		assert.NotContains(t, string(done.body), "Checksum", "S3 omits the checksum of a KMS object here")

		head := get(s, http.MethodHead, multipartKey, nil)
		assert.Equal(t, "arn:aws:kms:us-east-1:000000000000:key/k1", head.header.Get(headerSSEKMSKey))
		assert.NotEmpty(t, get(s, http.MethodHead, multipartKey, map[string]string{"X-Amz-Checksum-Mode": "ENABLED"}).
			header.Get("X-Amz-Checksum-Crc64nvme"), "a GET still has it")
	})
}

func TestCopyInPlaceWithBucketEncryption(t *testing.T) {
	aes := cfgDoc("ServerSideEncryptionConfiguration", "<Rule><ApplyServerSideEncryptionByDefault>"+
		"<SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault>"+
		"<BucketKeyEnabled>false</BucketKeyEnabled></Rule>")

	t.Run("the default encryption of a new bucket does not allow it", func(t *testing.T) {
		s := newServer(t)
		seed(t, s, nil)

		got := s.do(call{
			method: http.MethodPut, target: objectPath, region: regionUSEast1,
			header: map[string]string{headerCopySource: sourceRef},
		})
		expectError(t, got, http.StatusBadRequest, codeInvalidReq, "copy an object to itself")
	})

	t.Run("an encryption put on the bucket allows it, even one that restates AES256", func(t *testing.T) {
		s := newServer(t)
		createBucket(t, s)
		require.Equal(t, http.StatusOK, cfgPut(s, "/bucket?encryption", aes, nil).status)
		put(t, s, objectPath, objectBody, nil)

		got := s.do(call{
			method: http.MethodPut, target: objectPath, region: regionUSEast1,
			header: map[string]string{headerCopySource: sourceRef},
		})
		assert.Equal(t, http.StatusOK, got.status, string(got.body))
	})

	t.Run("deleting the configuration refuses it again", func(t *testing.T) {
		s := newServer(t)
		createBucket(t, s)
		require.Equal(t, http.StatusOK, cfgPut(s, "/bucket?encryption", aes, nil).status)
		put(t, s, objectPath, objectBody, nil)
		require.Equal(t, http.StatusNoContent, cfgDelete(s, "/bucket?encryption").status)

		got := s.do(call{
			method: http.MethodPut, target: objectPath, region: regionUSEast1,
			header: map[string]string{headerCopySource: sourceRef},
		})
		assert.Equal(t, http.StatusBadRequest, got.status, string(got.body))
	})
}
