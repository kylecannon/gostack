package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// This file holds the Secrets Manager and SSM Parameter Store workloads. Each
// speaks json 1.1 over plain HTTP with a SigV4 header the services route on and
// nobody checks, as the KMS workloads do, so the repository takes no dependency
// on the SDK's secretsmanager and ssm modules. A cell makes what it reads
// before the clock starts.
//
//	secretsmanager/value  PutSecretValue, then GetSecretValue of the secret, at
//	                      256 B and 64 KiB (the 65,536-byte SecretString limit)
//	ssm/parameter         PutParameter with Overwrite, then GetParameter of the
//	                      parameter, at 256 B and 4 KiB (the Standard tier's
//	                      4,096-character value limit)
//	ssm/path              GetParametersByPath of a path of ten parameters, one
//	                      full page, at 256 B and 4 KiB a value
//
// The secret cells share one secret: Secrets Manager keeps up to 100 versions
// of a secret that no staging label names, so a secret a worker would be 64
// secrets of 100 versions at 64 KiB, which is the state of a different test
// than the one these cells make. Every PutSecretValue adds a version, and the
// oldest drops out once the secret holds 100, so the state a save writes stays
// the same size. An SSM parameter a worker keeps to its own name; PutParameter
// with Overwrite keeps up to 100 versions of it, bounded the same way.
//
// GetSecretValue stamps the secret's LastAccessedDate with the day, and
// persistence is told only when the day changes, so it is the PutSecretValue
// of each cycle that a persistence-on secret cell saves for. Neither SSM read
// changes anything.

const (
	serviceSecrets    = "secretsmanager"
	persistEnvSecrets = "PERSIST_SECRETSMANAGER"
	serviceSSM        = "ssm"
	persistEnvSSM     = "PERSIST_SSM"

	workloadValue     = "value"
	workloadParameter = "parameter"
	workloadPath      = "path"

	opPutSecretValue = "put-secret-value"
	opGetSecretValue = "get-secret-value"
	opPutParameter   = "put-parameter"
	opGetParameter   = "get-parameter"
	opGetByPath      = "get-parameters-by-path"

	secretsPayloadSmall = 256
	// secretsPayloadLimit is the longest SecretString: 65,536 bytes.
	secretsPayloadLimit = 64 * bytesPerKiB
	ssmPayloadSmall     = 256
	// ssmPayloadLimit is the longest value of a Standard tier parameter: 4,096
	// characters.
	ssmPayloadLimit = 4 * bytesPerKiB

	// pathParameters is how many parameters the path cells' path holds, and the
	// size of a GetParametersByPath page.
	pathParameters = 10

	// ssmCellsPerSetting is the cells of an SSM workload with persistence on or
	// off: two payloads at three concurrencies.
	ssmCellsPerSetting = 6

	secretsAuth = "AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/secretsmanager/aws4_request, " +
		"SignedHeaders=host;x-amz-date, Signature=abc"
	secretsTarget = "secretsmanager."
	ssmAuth       = "AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/ssm/aws4_request, " +
		"SignedHeaders=host;x-amz-date, Signature=abc"
	ssmTarget      = "AmazonSSM."
	secretsContent = "application/x-amz-json-1.1"

	secretName = "bench-secret"
	pathPrefix = "/bench-path/"
	paramRoot  = "/bench-parameter/"

	fieldSecretID = "SecretId"
	fieldName     = "Name"
)

var (
	// secretsPersistence and ssmPersistence are the persistence settings of the
	// cells, "" being off.
	secretsPersistence = []string{"", persistEnvSecrets}
	ssmPersistence     = []string{"", persistEnvSSM}

	errSecrets = errors.New("secretsmanager")
	errSSM     = errors.New("ssm")
)

// jsonClient sends json 1.1 requests of one service to one endpoint.
type jsonClient struct {
	http     *http.Client
	endpoint string
	auth     string
	target   string
	// fail is the error every refused call wraps.
	fail error
}

func newSecretsClient(endpoint string, workers int) jsonClient {
	return jsonClient{
		http: &http.Client{Transport: newTransport(workers)}, endpoint: endpoint,
		auth: secretsAuth, target: secretsTarget, fail: errSecrets,
	}
}

func newSSMClient(endpoint string, workers int) jsonClient {
	return jsonClient{
		http: &http.Client{Transport: newTransport(workers)}, endpoint: endpoint,
		auth: ssmAuth, target: ssmTarget, fail: errSSM,
	}
}

// call sends one operation with input as its body and decodes the answer into
// out, which may be nil.
func (c jsonClient) call(ctx context.Context, operation string, input, out any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("%s: encode the request: %w", operation, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build %s: %w", operation, err)
	}

	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Content-Type", secretsContent)
	req.Header.Set("X-Amz-Date", s3Date)
	req.Header.Set("X-Amz-Target", c.target+operation)

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
		return fmt.Errorf("%w: %s: status %d: %.*s", c.fail, operation, resp.StatusCode, formErrorHead, answer)
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

// checkPayload refuses a payload outside 1 to limit.
func checkPayload(cell cellSpec, limit int) error {
	if cell.Payload < 1 || cell.Payload > limit {
		return fmt.Errorf("%w: payload %d must be 1 to %d", errBadAxis, cell.Payload, limit)
	}

	return nil
}

// tokens makes the ClientRequestToken every Secrets Manager write needs, 32
// characters, a new one for each call and for each cell.
type tokens struct {
	cell uint64
	next atomic.Uint64
}

func newTokens() *tokens {
	return &tokens{cell: uint64(time.Now().UnixNano())}
}

func (t *tokens) new() string {
	return fmt.Sprintf("%016x%016x", t.cell, t.next.Add(1))
}

// secretID is the name of the cell's secret: one a cell, so a container that
// runs two cells holds two secrets.
func (t *tokens) secretID() string {
	return secretName + "-" + strconv.FormatUint(t.cell, 16)
}

// createSecret makes the cell's secret with value.
func (c jsonClient) createSecret(ctx context.Context, value string, tok *tokens) error {
	return c.call(ctx, "CreateSecret", map[string]string{
		fieldName: tok.secretID(), "SecretString": value, "ClientRequestToken": tok.new(),
	}, nil)
}

// putSecretValue adds a version of the cell's secret.
func (c jsonClient) putSecretValue(ctx context.Context, value string, tok *tokens) error {
	return c.call(ctx, "PutSecretValue", map[string]string{
		fieldSecretID: tok.secretID(), "SecretString": value, "ClientRequestToken": tok.new(),
	}, nil)
}

// getSecretValue reads the cell's secret and checks that it holds want bytes.
func (c jsonClient) getSecretValue(ctx context.Context, id string, want int) error {
	var out struct{ SecretString string }

	err := c.call(ctx, "GetSecretValue", map[string]string{fieldSecretID: id}, &out)
	if err != nil {
		return err
	}

	if len(out.SecretString) != want {
		return fmt.Errorf("%w: GetSecretValue answered %d bytes, not %d", c.fail, len(out.SecretString), want)
	}

	return nil
}

// parameterValue is the part of a Parameter a cell reads.
type parameterValue struct{ Value string }

// putParameter writes name, a String, over its earlier versions.
func (c jsonClient) putParameter(ctx context.Context, name, value string) error {
	return c.call(ctx, "PutParameter", map[string]any{
		fieldName: name, "Value": value, "Type": "String", "Overwrite": true,
	}, nil)
}

// getParameter reads name and checks that it holds want bytes.
func (c jsonClient) getParameter(ctx context.Context, name string, want int) error {
	var out struct{ Parameter parameterValue }

	err := c.call(ctx, "GetParameter", map[string]string{fieldName: name}, &out)
	if err != nil {
		return err
	}

	if len(out.Parameter.Value) != want {
		return fmt.Errorf("%w: GetParameter answered %d bytes, not %d", c.fail, len(out.Parameter.Value), want)
	}

	return nil
}

// getByPath reads one page of the parameters under path and checks that it is
// the full page of values of want bytes.
func (c jsonClient) getByPath(ctx context.Context, path string, want int) error {
	var out struct{ Parameters []parameterValue }

	err := c.call(ctx, "GetParametersByPath", map[string]any{"Path": path, "MaxResults": pathParameters}, &out)
	if err != nil {
		return err
	}

	if len(out.Parameters) != pathParameters {
		return fmt.Errorf("%w: GetParametersByPath answered %d parameters, not %d", c.fail, len(out.Parameters),
			pathParameters)
	}

	for _, p := range out.Parameters {
		if len(p.Value) != want {
			return fmt.Errorf("%w: GetParametersByPath answered a value of %d bytes, not %d", c.fail, len(p.Value), want)
		}
	}

	return nil
}

func secretsCell(payload int, persistEnv string, concurrency int) cellSpec {
	return cellSpec{
		Service: serviceSecrets, Workload: workloadValue, Payload: payload,
		PersistEnv: persistEnv, Concurrency: concurrency,
	}
}

func ssmCell(workload string, payload int, persistEnv string, concurrency int) cellSpec {
	return cellSpec{
		Service: serviceSSM, Workload: workload, Payload: payload,
		PersistEnv: persistEnv, Concurrency: concurrency,
	}
}

// valueWorkload is PutSecretValue and GetSecretValue of one secret a cycle.
type valueWorkload struct{}

func (valueWorkload) Service() string { return serviceSecrets }
func (valueWorkload) Name() string    { return workloadValue }
func (valueWorkload) Ops() []string   { return []string{opPutSecretValue, opGetSecretValue} }

func (valueWorkload) Run(ctx context.Context, c client, cell cellSpec, duration time.Duration) (runResult, error) {
	err := checkLevels(cell)
	if err != nil {
		return runResult{}, err
	}

	err = checkPayload(cell, secretsPayloadLimit)
	if err != nil {
		return runResult{}, err
	}

	sc := newSecretsClient(c.endpoint, cell.Concurrency)
	tok := newTokens()
	value := payloadBody(cell.Payload)

	err = sc.createSecret(ctx, value, tok)
	if err != nil {
		return runResult{}, err
	}

	l := loop{workers: cell.Concurrency, duration: duration, ops: valueWorkload{}.Ops()}

	return l.run(ctx, func(ctx context.Context, _ int, rec *recorder) {
		if !rec.measure(opPutSecretValue, func() error { return sc.putSecretValue(ctx, value, tok) }) {
			return
		}

		if rec.measure(opGetSecretValue, func() error { return sc.getSecretValue(ctx, tok.secretID(), len(value)) }) {
			rec.cycles++
		}
	})
}

// Cells is each payload at every concurrency, persistence off and on. The quick
// profile is both payloads at concurrency 16 and the small one with persistence
// on.
func (valueWorkload) Cells(profile string) []cellSpec {
	if profile == profileQuick {
		return []cellSpec{
			secretsCell(secretsPayloadSmall, "", levelMid),
			secretsCell(secretsPayloadLimit, "", levelMid),
			secretsCell(secretsPayloadSmall, persistEnvSecrets, levelMid),
		}
	}

	var cells []cellSpec

	for _, persistEnv := range secretsPersistence {
		for _, payload := range []int{secretsPayloadSmall, secretsPayloadLimit} {
			for _, level := range []int{levelLow, levelMid, levelHigh} {
				cells = append(cells, secretsCell(payload, persistEnv, level))
			}
		}
	}

	return cells
}

// parameterWorkload is PutParameter and GetParameter of one parameter a worker.
type parameterWorkload struct{}

func (parameterWorkload) Service() string { return serviceSSM }
func (parameterWorkload) Name() string    { return workloadParameter }
func (parameterWorkload) Ops() []string   { return []string{opPutParameter, opGetParameter} }

func (parameterWorkload) Run(ctx context.Context, c client, cell cellSpec, duration time.Duration) (runResult, error) {
	err := checkLevels(cell)
	if err != nil {
		return runResult{}, err
	}

	err = checkPayload(cell, ssmPayloadLimit)
	if err != nil {
		return runResult{}, err
	}

	pc := newSSMClient(c.endpoint, cell.Concurrency)
	value := payloadBody(cell.Payload)

	l := loop{workers: cell.Concurrency, duration: duration, ops: parameterWorkload{}.Ops()}

	return l.run(ctx, func(ctx context.Context, worker int, rec *recorder) {
		name := paramRoot + strconv.Itoa(worker)

		if !rec.measure(opPutParameter, func() error { return pc.putParameter(ctx, name, value) }) {
			return
		}

		if rec.measure(opGetParameter, func() error { return pc.getParameter(ctx, name, len(value)) }) {
			rec.cycles++
		}
	})
}

// Cells is each payload at every concurrency, persistence off and on. The quick
// profile is both payloads at concurrency 16 and the small one with persistence
// on.
func (parameterWorkload) Cells(profile string) []cellSpec {
	if profile == profileQuick {
		return []cellSpec{
			ssmCell(workloadParameter, ssmPayloadSmall, "", levelMid),
			ssmCell(workloadParameter, ssmPayloadLimit, "", levelMid),
			ssmCell(workloadParameter, ssmPayloadSmall, persistEnvSSM, levelMid),
		}
	}

	return ssmCells(workloadParameter)
}

// pathWorkload is GetParametersByPath of one full page a cycle.
type pathWorkload struct{}

func (pathWorkload) Service() string { return serviceSSM }
func (pathWorkload) Name() string    { return workloadPath }
func (pathWorkload) Ops() []string   { return []string{opGetByPath} }

func (pathWorkload) Run(ctx context.Context, c client, cell cellSpec, duration time.Duration) (runResult, error) {
	err := checkLevels(cell)
	if err != nil {
		return runResult{}, err
	}

	err = checkPayload(cell, ssmPayloadLimit)
	if err != nil {
		return runResult{}, err
	}

	pc := newSSMClient(c.endpoint, cell.Concurrency)
	value := payloadBody(cell.Payload)

	for i := range pathParameters {
		err = pc.putParameter(ctx, pathPrefix+strconv.Itoa(i), value)
		if err != nil {
			return runResult{}, err
		}
	}

	l := loop{workers: cell.Concurrency, duration: duration, ops: pathWorkload{}.Ops()}

	return l.run(ctx, func(ctx context.Context, _ int, rec *recorder) {
		if rec.measure(opGetByPath, func() error { return pc.getByPath(ctx, pathPrefix, len(value)) }) {
			rec.cycles++
		}
	})
}

// Cells is each payload at every concurrency, persistence off and on. The quick
// profile is the small payload at concurrency 16.
func (pathWorkload) Cells(profile string) []cellSpec {
	if profile == profileQuick {
		return []cellSpec{ssmCell(workloadPath, ssmPayloadSmall, "", levelMid)}
	}

	return ssmCells(workloadPath)
}

// ssmCells is every payload of the SSM workloads at every concurrency,
// persistence off and on.
func ssmCells(workload string) []cellSpec {
	cells := make([]cellSpec, 0, ssmCellsPerSetting*len(ssmPersistence))

	for _, persistEnv := range ssmPersistence {
		for _, payload := range []int{ssmPayloadSmall, ssmPayloadLimit} {
			for _, level := range []int{levelLow, levelMid, levelHigh} {
				cells = append(cells, ssmCell(workload, payload, persistEnv, level))
			}
		}
	}

	return cells
}
