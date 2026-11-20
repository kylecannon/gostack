package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// This file holds the KMS workloads. Each speaks json 1.1 over plain HTTP with a
// SigV4 header the services route on and nobody checks, as the S3 workloads do,
// so the repository takes no dependency on the SDK's kms module. A cell makes
// its key before the clock starts.
//
//	kms/crypt    Encrypt, then Decrypt of the blob it answered, at 32 B and
//	             4 KiB, under one SYMMETRIC_DEFAULT key.
//	kms/datakey  GenerateDataKey of an AES_256 data key under that key.
//	kms/sign     Sign, then Verify of the signature, of a 32 B message, with an
//	             RSA_2048 key (RSASSA_PSS_SHA_256) or an ECC_NIST_P256 key
//	             (ECDSA_SHA_256).
//
// None of these calls changes KMS's state, so a persistence-on cell measures
// what persistence costs a service that is only read: nothing is due to save
// after the key is made.

const (
	serviceKMS    = "kms"
	persistEnvKMS = "PERSIST_KMS"

	workloadCrypt   = "crypt"
	workloadDataKey = "datakey"
	workloadSign    = "sign"

	opEncrypt = "encrypt"
	opDecrypt = "decrypt"
	opDataKey = "datakey"
	opSign    = "sign"
	opVerify  = "verify"

	optSpec = "spec"

	kmsKeyID = "KeyId"

	kmsSpecRSA = "RSA_2048"
	kmsSpecECC = "ECC_NIST_P256"

	kmsPayloadSmall = 32
	kmsPayloadLarge = 4 * bytesPerKiB

	// kmsAuth is a SigV4 header that names the kms signing name and a region. The
	// signature is not checked.
	kmsAuth = "AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/kms/aws4_request, " +
		"SignedHeaders=host;x-amz-date, Signature=abc"
	kmsContentType = "application/x-amz-json-1.1"
	kmsTarget      = "TrentService."
)

// kmsSignAlgorithms is the signing algorithm of each key spec the sign cells use.
var kmsSignAlgorithms = map[string]string{
	kmsSpecRSA: "RSASSA_PSS_SHA_256",
	kmsSpecECC: "ECDSA_SHA_256",
}

// kmsPersistence are the persistence settings of the KMS cells, "" being off.
var kmsPersistence = []string{"", persistEnvKMS}

var errKMS = errors.New("kms")

// kmsClient sends KMS requests to one endpoint.
type kmsClient struct {
	http     *http.Client
	endpoint string
}

func newKMSClient(endpoint string, workers int) kmsClient {
	return kmsClient{http: &http.Client{Transport: newTransport(workers)}, endpoint: endpoint}
}

// call sends one operation with input as its body and decodes the answer into
// out, which may be nil.
func (c kmsClient) call(ctx context.Context, operation string, input, out any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("%s: encode the request: %w", operation, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build %s: %w", operation, err)
	}

	req.Header.Set("Authorization", kmsAuth)
	req.Header.Set("Content-Type", kmsContentType)
	req.Header.Set("X-Amz-Date", s3Date)
	req.Header.Set("X-Amz-Target", kmsTarget+operation)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}

	defer func() { _ = resp.Body.Close() }()

	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s: read the answer: %w", operation, err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s: status %d: %.*s", errKMS, operation, resp.StatusCode, formErrorHead, answer)
	}

	if out == nil {
		return nil
	}

	err = json.Unmarshal(answer, out)
	if err != nil {
		return fmt.Errorf("%s: decode the answer: %w", operation, err)
	}

	return nil
}

// kmsKeyMetadata is the part of CreateKey's KeyMetadata a cell reads.
type kmsKeyMetadata struct {
	KeyID string `json:"KeyId"`
}

// createKey makes a key of spec and usage and returns its id.
func (c kmsClient) createKey(ctx context.Context, spec, usage string) (string, error) {
	var out struct{ KeyMetadata kmsKeyMetadata }

	err := c.call(ctx, "CreateKey", map[string]string{"KeySpec": spec, "KeyUsage": usage}, &out)
	if err != nil {
		return "", err
	}

	if out.KeyMetadata.KeyID == "" {
		return "", fmt.Errorf("%w: CreateKey answered no KeyId", errKMS)
	}

	return out.KeyMetadata.KeyID, nil
}

// encrypt seals plaintext under key and returns the blob.
func (c kmsClient) encrypt(ctx context.Context, key string, plaintext []byte) ([]byte, error) {
	var out struct{ CiphertextBlob []byte }

	err := c.call(ctx, "Encrypt", map[string]any{kmsKeyID: key, "Plaintext": plaintext}, &out)
	if err != nil {
		return nil, err
	}

	if len(out.CiphertextBlob) == 0 {
		return nil, fmt.Errorf("%w: Encrypt answered no CiphertextBlob", errKMS)
	}

	return out.CiphertextBlob, nil
}

// decrypt opens blob and checks that it held want.
func (c kmsClient) decrypt(ctx context.Context, blob, want []byte) error {
	var out struct{ Plaintext []byte }

	err := c.call(ctx, "Decrypt", map[string]any{"CiphertextBlob": blob}, &out)
	if err != nil {
		return err
	}

	if !bytes.Equal(out.Plaintext, want) {
		return fmt.Errorf("%w: Decrypt answered %d bytes that are not the %d sealed", errKMS, len(out.Plaintext), len(want))
	}

	return nil
}

// dataKey makes an AES_256 data key under key.
func (c kmsClient) dataKey(ctx context.Context, key string) error {
	var out struct {
		Plaintext      []byte
		CiphertextBlob []byte
	}

	err := c.call(ctx, "GenerateDataKey", map[string]string{kmsKeyID: key, "KeySpec": "AES_256"}, &out)
	if err != nil {
		return err
	}

	if len(out.Plaintext) != 32 || len(out.CiphertextBlob) == 0 {
		return fmt.Errorf("%w: GenerateDataKey answered a %d-byte key", errKMS, len(out.Plaintext))
	}

	return nil
}

// sign signs message under key with algorithm and returns the signature.
func (c kmsClient) sign(ctx context.Context, key, algorithm string, message []byte) ([]byte, error) {
	var out struct{ Signature []byte }

	err := c.call(ctx, "Sign", map[string]any{
		kmsKeyID: key, "SigningAlgorithm": algorithm, "MessageType": "RAW", "Message": message,
	}, &out)
	if err != nil {
		return nil, err
	}

	if len(out.Signature) == 0 {
		return nil, fmt.Errorf("%w: Sign answered no Signature", errKMS)
	}

	return out.Signature, nil
}

// verify checks signature over message, and fails unless KMS finds it valid.
func (c kmsClient) verify(ctx context.Context, key, algorithm string, message, signature []byte) error {
	var out struct{ SignatureValid bool }

	err := c.call(ctx, "Verify", map[string]any{
		kmsKeyID: key, "SigningAlgorithm": algorithm, "MessageType": "RAW", "Message": message,
		"Signature": signature,
	}, &out)
	if err != nil {
		return err
	}

	if !out.SignatureValid {
		return fmt.Errorf("%w: Verify found a %d-byte signature not valid", errKMS, len(signature))
	}

	return nil
}

func kmsCell(workload string, payload int, options []option, persistEnv string, concurrency int) cellSpec {
	return cellSpec{
		Service: serviceKMS, Workload: workload, Payload: payload, Options: options,
		PersistEnv: persistEnv, Concurrency: concurrency,
	}
}

func signCell(spec, persistEnv string, concurrency int) cellSpec {
	return kmsCell(workloadSign, kmsPayloadSmall, []option{{Key: optSpec, Value: spec}}, persistEnv, concurrency)
}

// cryptWorkload is Encrypt and Decrypt of one blob a cycle.
type cryptWorkload struct{}

func (cryptWorkload) Service() string { return serviceKMS }
func (cryptWorkload) Name() string    { return workloadCrypt }
func (cryptWorkload) Ops() []string   { return []string{opEncrypt, opDecrypt} }

func (cryptWorkload) Run(ctx context.Context, c client, cell cellSpec, duration time.Duration) (runResult, error) {
	err := checkLevels(cell)
	if err != nil {
		return runResult{}, err
	}

	if cell.Payload < 1 || cell.Payload > kmsPayloadLarge {
		return runResult{}, fmt.Errorf("%w: payload %d must be 1 to %d", errBadAxis, cell.Payload, kmsPayloadLarge)
	}

	kc := newKMSClient(c.endpoint, cell.Concurrency)

	key, err := kc.createKey(ctx, "SYMMETRIC_DEFAULT", "ENCRYPT_DECRYPT")
	if err != nil {
		return runResult{}, err
	}

	plaintext := []byte(payloadBody(cell.Payload))
	l := loop{workers: cell.Concurrency, duration: duration, ops: cryptWorkload{}.Ops()}

	return l.run(ctx, func(ctx context.Context, _ int, rec *recorder) {
		var blob []byte

		// The closures write variables of this cycle, never Run's err, which every
		// worker would share.
		if !rec.measure(opEncrypt, func() error {
			var sealErr error
			blob, sealErr = kc.encrypt(ctx, key, plaintext)

			return sealErr
		}) {
			return
		}

		if rec.measure(opDecrypt, func() error { return kc.decrypt(ctx, blob, plaintext) }) {
			rec.cycles++
		}
	})
}

// Cells is each payload at every concurrency, persistence off and on. The quick
// profile is the 32 B cell at concurrency 16, the 4 KiB one, and the 32 B one with
// persistence on.
func (cryptWorkload) Cells(profile string) []cellSpec {
	if profile == profileQuick {
		return []cellSpec{
			kmsCell(workloadCrypt, kmsPayloadSmall, nil, "", levelMid),
			kmsCell(workloadCrypt, kmsPayloadLarge, nil, "", levelMid),
			kmsCell(workloadCrypt, kmsPayloadSmall, nil, persistEnvKMS, levelMid),
		}
	}

	var cells []cellSpec

	for _, persistEnv := range kmsPersistence {
		for _, payload := range []int{kmsPayloadSmall, kmsPayloadLarge} {
			for _, level := range []int{levelLow, levelMid, levelHigh} {
				cells = append(cells, kmsCell(workloadCrypt, payload, nil, persistEnv, level))
			}
		}
	}

	return cells
}

// dataKeyWorkload is one GenerateDataKey a cycle.
type dataKeyWorkload struct{}

func (dataKeyWorkload) Service() string { return serviceKMS }
func (dataKeyWorkload) Name() string    { return workloadDataKey }
func (dataKeyWorkload) Ops() []string   { return []string{opDataKey} }

func (dataKeyWorkload) Run(ctx context.Context, c client, cell cellSpec, duration time.Duration) (runResult, error) {
	err := checkLevels(cell)
	if err != nil {
		return runResult{}, err
	}

	kc := newKMSClient(c.endpoint, cell.Concurrency)

	key, err := kc.createKey(ctx, "SYMMETRIC_DEFAULT", "ENCRYPT_DECRYPT")
	if err != nil {
		return runResult{}, err
	}

	l := loop{workers: cell.Concurrency, duration: duration, ops: dataKeyWorkload{}.Ops()}

	return l.run(ctx, func(ctx context.Context, _ int, rec *recorder) {
		if rec.measure(opDataKey, func() error { return kc.dataKey(ctx, key) }) {
			rec.cycles++
		}
	})
}

// Cells is every concurrency, persistence off and on; a data key has no payload
// axis. The quick profile is concurrency 16.
func (dataKeyWorkload) Cells(profile string) []cellSpec {
	if profile == profileQuick {
		return []cellSpec{kmsCell(workloadDataKey, 0, nil, "", levelMid)}
	}

	var cells []cellSpec

	for _, persistEnv := range kmsPersistence {
		for _, level := range []int{levelLow, levelMid, levelHigh} {
			cells = append(cells, kmsCell(workloadDataKey, 0, nil, persistEnv, level))
		}
	}

	return cells
}

// signWorkload is Sign and Verify of one message a cycle.
type signWorkload struct{}

func (signWorkload) Service() string { return serviceKMS }
func (signWorkload) Name() string    { return workloadSign }
func (signWorkload) Ops() []string   { return []string{opSign, opVerify} }

func (signWorkload) Run(ctx context.Context, c client, cell cellSpec, duration time.Duration) (runResult, error) {
	err := checkLevels(cell)
	if err != nil {
		return runResult{}, err
	}

	spec := cell.option(optSpec)

	algorithm, ok := kmsSignAlgorithms[spec]
	if !ok {
		return runResult{}, fmt.Errorf("%w: %s=%q, want %s or %s", errBadAxis, optSpec, spec, kmsSpecRSA, kmsSpecECC)
	}

	if cell.Payload < 1 || cell.Payload > kmsPayloadLarge {
		return runResult{}, fmt.Errorf("%w: payload %d must be 1 to %d", errBadAxis, cell.Payload, kmsPayloadLarge)
	}

	kc := newKMSClient(c.endpoint, cell.Concurrency)

	key, err := kc.createKey(ctx, spec, "SIGN_VERIFY")
	if err != nil {
		return runResult{}, err
	}

	message := []byte(payloadBody(cell.Payload))
	l := loop{workers: cell.Concurrency, duration: duration, ops: signWorkload{}.Ops()}

	return l.run(ctx, func(ctx context.Context, _ int, rec *recorder) {
		var signature []byte

		if !rec.measure(opSign, func() error {
			var signErr error
			signature, signErr = kc.sign(ctx, key, algorithm, message)

			return signErr
		}) {
			return
		}

		if rec.measure(opVerify, func() error { return kc.verify(ctx, key, algorithm, message, signature) }) {
			rec.cycles++
		}
	})
}

// Cells is each key spec at every concurrency, persistence off and on. The
// quick profile is both specs at concurrency 16.
func (signWorkload) Cells(profile string) []cellSpec {
	if profile == profileQuick {
		return []cellSpec{signCell(kmsSpecRSA, "", levelMid), signCell(kmsSpecECC, "", levelMid)}
	}

	var cells []cellSpec

	for _, persistEnv := range kmsPersistence {
		for _, spec := range []string{kmsSpecRSA, kmsSpecECC} {
			for _, level := range []int{levelLow, levelMid, levelHigh} {
				cells = append(cells, signCell(spec, persistEnv, level))
			}
		}
	}

	return cells
}
