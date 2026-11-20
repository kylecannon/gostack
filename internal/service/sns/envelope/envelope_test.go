package envelope_test

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/service/sns/envelope"
	"github.com/stretchr/testify/require"
)

const (
	testBase   = "http://localhost.localstack.cloud:4566"
	testTopic  = "arn:aws:sns:us-east-1:111111111111:topic-1"
	testFIFO   = "arn:aws:sns:us-east-1:111111111111:topic-1.fifo"
	testSub    = testTopic + ":0d7a8b3e-1c2f-4e5a-9b6c-7d8e9f0a1b2c"
	testFIFOSb = testFIFO + ":0d7a8b3e-1c2f-4e5a-9b6c-7d8e9f0a1b2c"
	testID     = "6d2e5f3a-0b1c-4d2e-8f3a-4b5c6d7e8f90"
	testToken  = "2336412f37fb687f5d51e6e2425c464de12884ea8e23a1b7d4a5e6f7a8b9c0d1"
	testSeq    = "10000000000000003000"
	confirmMsg = "You have chosen to subscribe to the topic " + testTopic +
		".\nTo confirm the subscription, visit the SubscribeURL included in this message."
	unsubMsg = "You have chosen to deactivate subscription " + testSub +
		".\nTo cancel this operation and restore the subscription, visit the SubscribeURL included in this message."
	// wantTime is testTime as the envelope writes it: UTC, truncated to the
	// millisecond.
	wantTime   = "2026-11-19T17:21:57.602Z"
	attrNumber = "number"
	fifoMsg    = "a fifo message"
)

// The envelope's member names.
const (
	kType       = "Type"
	kMessageID  = "MessageId"
	kSequence   = "SequenceNumber"
	kToken      = "Token"
	kTopicARN   = "TopicArn"
	kSubject    = "Subject"
	kMessage    = "Message"
	kSubscribe  = "SubscribeURL"
	kTimestamp  = "Timestamp"
	kVersion    = "SignatureVersion"
	kSignature  = "Signature"
	kCertURL    = "SigningCertURL"
	kUnsub      = "UnsubscribeURL"
	kAttributes = "MessageAttributes"
)

var testTime = time.Date(2026, 11, 19, 18, 21, 57, 602999000, time.FixedZone("CET", 3600))

// signer is shared so the RSA key is generated once per test binary.
var signer = &envelope.Signer{}

// Test_Signer_Body builds each envelope from a fixed input and compares it with
// the bodies LocalStack's snapshots record: the field order of the
// aws.validated SQS bodies (TestSNSSubscriptionSQS::test_publish_sqs_verify_signature,
// TestSNSPublishCrud::test_publish_message_before_subscribe_topic,
// TestSNSSubscriptionSQSFifo::test_publish_fifo_messages_to_dlq) and of AWS's
// HTTP confirmation document, and the values of
// TestSNSSubscriptionHttp::test_subscribe_external_http_endpoint_lambda_url_sig_validation.
// Every signed envelope is verified against the served certificate.
func Test_Signer_Body(t *testing.T) {
	t.Parallel()

	certURL := testBase + envelope.CertPath
	unsubURL := testBase + "/?Action=Unsubscribe&SubscriptionArn=" + testSub
	fifoUnsubURL := testBase + "/?Action=Unsubscribe&SubscriptionArn=" + testFIFOSb
	subURL := testBase + "/?Action=ConfirmSubscription&TopicArn=" + testTopic + "&Token=" + testToken
	attrs := []envelope.Attribute{
		{Name: attrNumber, DataType: "Number", StringValue: new("1")},
		{Name: "bin", DataType: "Binary", BinaryValue: []byte{0, 1, 0xfe, 0xff}},
		{Name: "a-str", DataType: "String.custom", StringValue: new("x&y")},
	}
	attr := func(dataType, value string) map[string]any { return map[string]any{kType: dataType, "Value": value} }
	numberAttr := attr("Number", "1")
	wantAttrs := map[string]any{
		attrNumber: numberAttr,
		"bin":      attr("Binary", "AAH+/w=="),
		"a-str":    attr("String.custom", "x&y"),
	}
	notification := string(envelope.Notification)
	signedNotification := []string{
		kType, kMessageID, kTopicARN, kMessage, kTimestamp, kVersion, kSignature, kCertURL, kUnsub,
	}
	confirmation := []string{
		kType, kMessageID, kToken, kTopicARN, kMessage, kSubscribe, kTimestamp, kVersion, kSignature, kCertURL,
	}

	tests := []struct {
		name       string
		give       envelope.Message
		wantKeys   []string
		wantValues map[string]any
	}{
		{
			name: "notification signature version 1",
			give: envelope.Message{
				MessageID: testID, TopicARN: testTopic, SubscriptionARN: testSub,
				Message: "test_external_http_endpoint", Timestamp: testTime,
			},
			wantKeys: signedNotification,
			wantValues: map[string]any{
				kType: notification, kMessageID: testID, kTopicARN: testTopic,
				kMessage: "test_external_http_endpoint", kTimestamp: wantTime,
				kVersion: "1", kCertURL: certURL, kUnsub: unsubURL,
			},
		},
		{
			name: "notification signature version 2 with attributes",
			give: envelope.Message{
				Type: envelope.Notification, MessageID: testID, TopicARN: testTopic, SubscriptionARN: testSub,
				Message: "test signature value with attributes", Timestamp: testTime,
				Attributes: attrs, SignatureVersion: "2",
			},
			wantKeys: append(slices.Clip(signedNotification), kAttributes),
			wantValues: map[string]any{
				kType: notification, kMessageID: testID, kTopicARN: testTopic,
				kMessage: "test signature value with attributes", kTimestamp: wantTime,
				kVersion: "2", kCertURL: certURL, kUnsub: unsubURL, kAttributes: wantAttrs,
			},
		},
		{
			name: "notification with subject",
			give: envelope.Message{
				MessageID: testID, TopicARN: testTopic, SubscriptionARN: testSub,
				Subject: "[Subject] Test subject", Message: "with subject", Timestamp: testTime,
				SignatureVersion: "1",
			},
			wantKeys: []string{
				kType, kMessageID, kTopicARN, kSubject, kMessage, kTimestamp, kVersion, kSignature, kCertURL, kUnsub,
			},
			wantValues: map[string]any{
				kType: notification, kMessageID: testID, kTopicARN: testTopic,
				kSubject: "[Subject] Test subject", kMessage: "with subject", kTimestamp: wantTime,
				kVersion: "1", kCertURL: certURL, kUnsub: unsubURL,
			},
		},
		{
			name: "subscription confirmation signature version 1",
			give: envelope.Message{
				Type: envelope.SubscriptionConfirmation, MessageID: testID, TopicARN: testTopic,
				Token: testToken, Message: confirmMsg, Timestamp: testTime,
			},
			wantKeys: confirmation,
			wantValues: map[string]any{
				kType: "SubscriptionConfirmation", kMessageID: testID, kToken: testToken,
				kTopicARN: testTopic, kMessage: confirmMsg, kSubscribe: subURL,
				kTimestamp: wantTime, kVersion: "1", kCertURL: certURL,
			},
		},
		{
			name: "subscription confirmation signature version 2",
			give: envelope.Message{
				Type: envelope.SubscriptionConfirmation, MessageID: testID, TopicARN: testTopic,
				Token: testToken, Message: confirmMsg, Timestamp: testTime, SignatureVersion: "2",
			},
			wantKeys: confirmation,
			wantValues: map[string]any{
				kType: "SubscriptionConfirmation", kMessageID: testID, kToken: testToken,
				kTopicARN: testTopic, kMessage: confirmMsg, kSubscribe: subURL,
				kTimestamp: wantTime, kVersion: "2", kCertURL: certURL,
			},
		},
		{
			name: "unsubscribe confirmation signature version 1",
			give: envelope.Message{
				Type: envelope.UnsubscribeConfirmation, MessageID: testID, TopicARN: testTopic,
				SubscriptionARN: testSub, Token: testToken, Message: unsubMsg, Timestamp: testTime,
			},
			wantKeys: confirmation,
			wantValues: map[string]any{
				kType: "UnsubscribeConfirmation", kMessageID: testID, kToken: testToken,
				kTopicARN: testTopic, kMessage: unsubMsg, kSubscribe: subURL,
				kTimestamp: wantTime, kVersion: "1", kCertURL: certURL,
			},
		},
		{
			name: "unsubscribe confirmation signature version 2",
			give: envelope.Message{
				Type: envelope.UnsubscribeConfirmation, MessageID: testID, TopicARN: testTopic,
				SubscriptionARN: testSub, Token: testToken, Message: unsubMsg, Timestamp: testTime,
				SignatureVersion: "2",
			},
			wantKeys: confirmation,
			wantValues: map[string]any{
				kType: "UnsubscribeConfirmation", kMessageID: testID, kToken: testToken,
				kTopicARN: testTopic, kMessage: unsubMsg, kSubscribe: subURL,
				kTimestamp: wantTime, kVersion: "2", kCertURL: certURL,
			},
		},
		{
			name: "fifo notification is not signed",
			give: envelope.Message{
				MessageID: testID, TopicARN: testFIFO, SubscriptionARN: testFIFOSb,
				Message: fifoMsg, Timestamp: testTime, SequenceNumber: testSeq,
				SignatureVersion: "2",
			},
			wantKeys: []string{kType, kMessageID, kSequence, kTopicARN, kMessage, kTimestamp, kUnsub},
			wantValues: map[string]any{
				kType: notification, kMessageID: testID, kSequence: testSeq,
				kTopicARN: testFIFO, kMessage: fifoMsg, kTimestamp: wantTime, kUnsub: fifoUnsubURL,
			},
		},
		{
			name: "fifo notification with subject and attributes",
			give: envelope.Message{
				MessageID: testID, TopicARN: testFIFO, SubscriptionARN: testFIFOSb, Subject: "s",
				Message: fifoMsg, Timestamp: testTime, SequenceNumber: "10000000000000004000",
				Attributes: attrs[:1],
			},
			wantKeys: []string{
				kType, kMessageID, kSequence, kTopicARN, kSubject, kMessage, kTimestamp, kUnsub, kAttributes,
			},
			wantValues: map[string]any{
				kType: notification, kMessageID: testID, kSequence: "10000000000000004000",
				kTopicARN: testFIFO, kSubject: "s", kMessage: fifoMsg, kTimestamp: wantTime,
				kUnsub: fifoUnsubURL, kAttributes: map[string]any{attrNumber: numberAttr},
			},
		},
	}

	cert := servedCert(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			body, err := signer.Body(&tt.give, testBase)
			require.NoError(t, err)

			keys, values := parseOrdered(t, body)
			require.Equal(t, tt.wantKeys, keys)

			signature, signed := values[kSignature].(string)
			delete(values, kSignature)
			require.Equal(t, tt.wantValues, values)

			if signed {
				verify(t, cert, values, signature)
			}
		})
	}
}

func Test_Signer_Body_unknownType(t *testing.T) {
	t.Parallel()

	_, err := signer.Body(&envelope.Message{Type: "Bogus", TopicARN: testTopic}, testBase)
	require.ErrorIs(t, err, envelope.ErrUnknownType)
}

// Test_Signer_Body_escaping checks that the body is ASCII as json.dumps
// writes it, that every string parses back to its input, and that the
// signature still verifies over the parsed text.
func Test_Signer_Body_escaping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		giveMessage string
		giveSubject string
		wantMessage string
		wantSubject string
	}{
		{
			name:        "quotes and backslashes",
			giveMessage: `say "hi" \ bye`, giveSubject: `a"b`,
			wantMessage: `say "hi" \ bye`, wantSubject: `a"b`,
		},
		{
			name:        "control characters",
			giveMessage: "a\nb\rc\td\be\ff\x01g\x7f", giveSubject: "x\ty",
			wantMessage: "a\nb\rc\td\be\ff\x01g\x7f", wantSubject: "x\ty",
		},
		{
			name:        "non-ascii and astral",
			giveMessage: "café αβγ € 🎉 <&>", giveSubject: "é",
			wantMessage: "café αβγ € 🎉 <&>", wantSubject: "é",
		},
		{
			name:        "invalid utf-8",
			giveMessage: "a\xffb", giveSubject: "s\xfe",
			wantMessage: "a�b", wantSubject: "s�",
		},
	}

	cert := servedCert(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			body, err := signer.Body(&envelope.Message{
				MessageID: testID, TopicARN: testTopic, SubscriptionARN: testSub,
				Subject: tt.giveSubject, Message: tt.giveMessage, Timestamp: testTime,
			}, testBase)
			require.NoError(t, err)

			for i, c := range body {
				require.Less(t, c, byte(0x80), "byte %d of the body is not ASCII", i)
			}

			_, values := parseOrdered(t, body)
			require.Equal(t, tt.wantMessage, values[kMessage])
			require.Equal(t, tt.wantSubject, values[kSubject])
			verify(t, cert, values, stringValue(t, values, kSignature))
		})
	}
}

func Test_BaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		giveCertHost string
		giveExternal string
		want         string
	}{
		{
			name:         "cert host set",
			giveCertHost: "sns.us-east-1.amazonaws.com", giveExternal: "http://localhost:4566",
			want: "https://sns.us-east-1.amazonaws.com",
		},
		{
			name:         "external url",
			giveCertHost: "", giveExternal: "http://localhost.localstack.cloud:4566",
			want: "http://localhost.localstack.cloud:4566",
		},
		{
			name:         "external url trailing slashes",
			giveCertHost: "", giveExternal: "https://example.test:4566//",
			want: "https://example.test:4566",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.want, envelope.BaseURL(tt.giveCertHost, tt.giveExternal))
		})
	}
}

func Test_Signer_ServeHTTP(t *testing.T) {
	t.Parallel()

	t.Run("get serves an RSA CA certificate", func(t *testing.T) {
		t.Parallel()

		rec := serve(t, http.MethodGet)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"))

		block, rest := pem.Decode(rec.Body.Bytes())
		require.NotNil(t, block)
		require.Equal(t, "CERTIFICATE", block.Type)
		require.Empty(t, bytes.TrimSpace(rest))

		cert, err := x509.ParseCertificate(block.Bytes)
		require.NoError(t, err)
		require.Equal(t, x509.RSA, cert.PublicKeyAlgorithm)
		require.True(t, cert.IsCA)
		require.NotEmpty(t, cert.SubjectKeyId)
		require.Equal(t, x509.SHA256WithRSA, cert.SignatureAlgorithm)
		require.NoError(t, cert.CheckSignatureFrom(cert))

		again, err := signer.CertificatePEM()
		require.NoError(t, err)
		require.Equal(t, rec.Body.Bytes(), again, "the certificate is generated once")
	})

	t.Run("head has no body", func(t *testing.T) {
		t.Parallel()

		rec := serve(t, http.MethodHead)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Zero(t, rec.Body.Len())
	})

	t.Run("post is not allowed", func(t *testing.T) {
		t.Parallel()

		rec := serve(t, http.MethodPost)
		require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
		require.Equal(t, "GET, HEAD", rec.Header().Get("Allow"))
	})

	t.Run("each signer has its own pair", func(t *testing.T) {
		t.Parallel()

		other, err := (&envelope.Signer{}).CertificatePEM()
		require.NoError(t, err)

		mine, err := signer.CertificatePEM()
		require.NoError(t, err)
		require.NotEqual(t, mine, other)
	})
}

// FuzzBody checks that any message and subject produce a body that parses,
// round-trips the (valid UTF-8 form of the) text, and verifies.
func FuzzBody(f *testing.F) {
	f.Add("hello", "", false)
	f.Add("line\nbreak \"quoted\" \\ \x00", "subject\t", true)
	f.Add("café 🎉 \xff", "\xfe", false)

	pemBytes, err := signer.CertificatePEM()
	require.NoError(f, err)

	cert := parseCert(f, pemBytes)

	f.Fuzz(func(t *testing.T, message, subject string, v2 bool) {
		version := "1"
		if v2 {
			version = "2"
		}

		body, err := signer.Body(&envelope.Message{
			MessageID: testID, TopicARN: testTopic, SubscriptionARN: testSub,
			Subject: subject, Message: message, Timestamp: testTime, SignatureVersion: version,
		}, testBase)
		require.NoError(t, err)

		_, values := parseOrdered(t, body)
		require.Equal(t, strings.ToValidUTF8(message, "�"), values[kMessage])
		verify(t, cert, values, stringValue(t, values, kSignature))
	})
}

// verify checks a signature as AWS's sns-verify-signature-of-message and
// LocalStack's tests do: the canonical string is rebuilt from the parsed
// envelope, and checked against the served certificate's public key with
// PKCS1 v1.5 and the hash SignatureVersion names.
func verify(t *testing.T, cert *x509.Certificate, values map[string]any, signature string) {
	t.Helper()

	fields := []string{kMessage, kMessageID, kSubject, kTimestamp, kTopicARN, kType}
	if values[kType] != string(envelope.Notification) {
		fields = []string{kMessage, kMessageID, kSubscribe, kTimestamp, kToken, kTopicARN, kType}
	}

	var canonical strings.Builder

	for _, f := range fields {
		if _, ok := values[f]; ok {
			canonical.WriteString(f + "\n" + stringValue(t, values, f) + "\n")
		}
	}

	sig, err := base64.StdEncoding.DecodeString(signature)
	require.NoError(t, err)

	algo, other := x509.SHA1WithRSA, x509.SHA256WithRSA
	if values[kVersion] == "2" {
		algo, other = other, algo
	}

	require.NoError(t, cert.CheckSignature(algo, []byte(canonical.String()), sig))
	require.Error(t, cert.CheckSignature(other, []byte(canonical.String()), sig),
		"the signature must use the hash its SignatureVersion names")
}

func stringValue(t *testing.T, values map[string]any, key string) string {
	t.Helper()

	s, ok := values[key].(string)
	require.True(t, ok, "%s is a string", key)

	return s
}

func serve(t *testing.T, method string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	signer.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, envelope.CertPath, nil))

	return rec
}

// servedCert fetches the certificate through the route handler, as a
// subscriber fetches SigningCertURL.
func servedCert(t *testing.T) *x509.Certificate {
	t.Helper()

	rec := serve(t, http.MethodGet)
	require.Equal(t, http.StatusOK, rec.Code)

	return parseCert(t, rec.Body.Bytes())
}

func parseCert(t testing.TB, pemBytes []byte) *x509.Certificate {
	t.Helper()

	block, _ := pem.Decode(pemBytes)
	require.NotNil(t, block)

	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)

	return cert
}

// parseOrdered returns the envelope's top-level keys in body order, and its
// values.
func parseOrdered(t *testing.T, body []byte) ([]string, map[string]any) {
	t.Helper()

	var values map[string]any
	require.NoError(t, json.Unmarshal(body, &values))

	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	require.NoError(t, err)
	require.Equal(t, json.Delim('{'), tok)

	var keys []string

	for dec.More() {
		tok, err = dec.Token()
		require.NoError(t, err)

		key, ok := tok.(string)
		require.True(t, ok)

		keys = append(keys, key)

		var skip json.RawMessage
		require.NoError(t, dec.Decode(&skip))
	}

	require.Len(t, values, len(keys), "no key appears twice")

	return keys, values
}
