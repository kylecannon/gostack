package ssm_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/ssm"
	"github.com/stretchr/testify/require"
)

const (
	kPutParameter            = "PutParameter"
	kName                    = "Name"
	kValue                   = "Value"
	kLit3                    = "123"
	kType                    = "Type"
	kString                  = "String"
	kVersion                 = "Version"
	kGetParameter            = "GetParameter"
	kGetParameters           = "GetParameters"
	kParameters              = "Parameters"
	kInvalidParameters       = "InvalidParameters"
	kParameterNotFound       = "ParameterNotFound"
	kABC                     = "/a/b/c"
	kValidationException     = "ValidationException"
	kOverwrite               = "Overwrite"
	kTags                    = "Tags"
	kDescribeParameters      = "DescribeParameters"
	kValueX                  = "value"
	kLabelParameterVersion   = "LabelParameterVersion"
	kLatest                  = "latest"
	kGetParametersByPath     = "GetParametersByPath"
	kPath                    = "Path"
	kValues                  = "Values"
	kGetParameterHistory     = "GetParameterHistory"
	kEnv                     = "env"
	kResourceType            = "ResourceType"
	kResourceID              = "ResourceId"
	kListTagsForResource     = "ListTagsForResource"
	kTagList                 = "TagList"
	kGetMaintenanceWindow    = "GetMaintenanceWindow"
	kCreateMaintenanceWindow = "CreateMaintenanceWindow"
	kDeleteMaintenanceWindow = "DeleteMaintenanceWindow"
)

const (
	nameRooted        = "/param-x"
	nameX             = "param-x"
	fieldNames        = "Names"
	fieldKey          = "Key"
	prefixPath        = "/prefix/path"
	fieldVersion      = "ParameterVersion"
	fieldLabels       = "Labels"
	prefix            = "/prefix"
	fieldFilters      = "ParameterFilters"
	tagA              = "/t/a"
	tagB              = "/t/b"
	resourceParameter = "Parameter"
	envProd           = "prod"
	tagNone           = "/t/none"
	fieldWindowID     = "WindowId"
	appDB             = "/app/db"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))

	m.Run()
}

// client calls one gateway that serves p.
type client struct {
	t   *testing.T
	url string
}

func newClient(t *testing.T, p *ssm.Provider) *client {
	t.Helper()

	return clientWith(t, p, nil)
}

// call sends one json 1.1 call and returns the status and the decoded body.
func (c *client) call(op string, body any) (int, map[string]any) {
	c.t.Helper()

	payload, err := json.Marshal(body)
	require.NoError(c.t, err)

	req, err := http.NewRequestWithContext(c.t.Context(), http.MethodPost, c.url+"/", bytes.NewReader(payload))
	require.NoError(c.t, err)

	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "AmazonSSM."+op)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20240101/us-east-1/ssm/aws4_request, "+
		"SignedHeaders=host, Signature=x")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(c.t, err)

	defer func() { require.NoError(c.t, resp.Body.Close()) }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(c.t, err)

	out := map[string]any{}
	if len(raw) > 0 {
		require.NoError(c.t, json.Unmarshal(raw, &out), string(raw))
	}

	return resp.StatusCode, out
}

// ok sends a call that must succeed.
func (c *client) ok(op string, body any) map[string]any {
	c.t.Helper()

	status, out := c.call(op, body)
	require.Equal(c.t, http.StatusOK, status, "%s: %v", op, out)

	return out
}

// fails sends a call that must fail with code, and returns its message.
func (c *client) fails(op string, body any, code string) string {
	c.t.Helper()

	status, out := c.call(op, body)
	require.Equal(c.t, http.StatusBadRequest, status, "%s: %v", op, out)
	require.Contains(c.t, out["__type"], code)

	msg, _ := out["message"].(string)
	if msg == "" {
		msg, _ = out["Message"].(string)
	}

	return msg
}

type obj = map[string]any

func param(out map[string]any) map[string]any {
	p, _ := out[resourceParameter].(map[string]any)

	return p
}

func list(out map[string]any, key string) []any {
	l, _ := out[key].([]any)

	return l
}

func Test_putAndGet_echoesRequestedName(t *testing.T) {
	c := newClient(t, ssm.New())

	out := c.ok(kPutParameter, obj{kName: nameRooted, kValue: kLit3, kType: kString, "Description": "test"})
	require.InDelta(t, 1, out[kVersion], 0)
	require.Equal(t, "Standard", out["Tier"])

	got := param(c.ok(kGetParameter, obj{kName: nameX}))
	require.Equal(t, nameX, got[kName])
	require.Equal(t, kLit3, got[kValue])
	require.Equal(t, "arn:aws:ssm:us-east-1:000000000000:parameter/param-x", got["ARN"])
	require.Equal(t, "text", got["DataType"])

	require.Equal(t, nameRooted, param(c.ok(kGetParameter, obj{kName: nameRooted}))[kName])

	many := c.ok(kGetParameters, obj{fieldNames: []string{nameRooted}})
	require.Equal(t, nameRooted, entry(list(many, kParameters), 0)[kName])
	require.Empty(t, list(many, kInvalidParameters))
	require.Contains(t, many, kInvalidParameters)

	byARN := param(c.ok(kGetParameter, obj{kName: got["ARN"]}))
	require.Equal(t, nameX, byARN[kName])

	c.ok("DeleteParameter", obj{kName: nameX})
	c.fails(kGetParameter, obj{kName: nameX}, kParameterNotFound)
}

func Test_hierarchicalNames(t *testing.T) {
	c := newClient(t, ssm.New())

	c.ok(kPutParameter, obj{kName: "/a//b//c", kValue: kLit3, kType: kString})
	require.Equal(t, kABC, param(c.ok(kGetParameter, obj{kName: kABC}))[kName])

	for _, bad := range []string{"/a//b//c", "a/b/c"} {
		msg := c.fails(kGetParameter, obj{kName: bad}, kValidationException)
		require.Contains(t, msg, "sub-paths divided by slash symbol")
		c.fails(kGetParameters, obj{fieldNames: []string{bad}}, kValidationException)
	}

	byARN := c.ok(kGetParameters, obj{fieldNames: []string{"arn:aws:ssm:us-east-1:000000000000:parameter/a/b/c"}})
	require.Equal(t, kABC, entry(list(byARN, kParameters), 0)[kName])
}

func Test_putChecks(t *testing.T) {
	c := newClient(t, ssm.New())

	c.fails(kPutParameter, obj{kName: "x", kValue: "", kType: kString}, kValidationException)
	c.fails(kPutParameter, obj{kName: "awsome", kValue: "v", kType: kString}, kValidationException)
	c.fails(kPutParameter, obj{kName: "/aws/x/y", kValue: "v", kType: kString}, "AccessDeniedException")
	c.fails(kPutParameter, obj{kName: "x", kValue: "v"}, kValidationException)
	c.fails(kPutParameter, obj{kName: "x", kValue: "v", kType: "Bad"}, kValidationException)
	c.fails(kPutParameter, obj{kName: "x", kValue: "v", kType: kString, "DataType": "TEXT"}, kValidationException)

	c.ok(kPutParameter, obj{kName: "x", kValue: "v", kType: kString})
	c.fails(kPutParameter, obj{kName: "x", kValue: "w", kType: kString}, "ParameterAlreadyExists")
	c.fails(kPutParameter, obj{kName: "x", kValue: "w", kOverwrite: true, kTags: []obj{{fieldKey: "k", kValue: "v"}}},
		kValidationException)

	out := c.ok(kPutParameter, obj{kName: "x", kValue: "w", kOverwrite: true})
	require.InDelta(t, 2, out[kVersion], 0)
	require.Equal(t, kString, param(c.ok(kGetParameter, obj{kName: "x"}))[kType])
}

func Test_secureStringIsStoredAsMotoDoes(t *testing.T) {
	c := newClient(t, ssm.New())

	c.ok(kPutParameter, obj{kName: "s", kValue: "secret", kType: "SecureString"})

	require.Equal(t, "kms:alias/aws/ssm:secret", param(c.ok(kGetParameter, obj{kName: "s"}))[kValue])
	require.Equal(t, "secret", param(c.ok(kGetParameter, obj{kName: "s", "WithDecryption": true}))[kValue])

	described := list(c.ok(kDescribeParameters, obj{}), kParameters)
	require.Equal(t, "alias/aws/ssm", entry(described, 0)["KeyId"])
	require.NotContains(t, described[0], kValue)
}

func Test_labelsHistoryAndPath(t *testing.T) {
	c := newClient(t, ssm.New())

	c.ok(kPutParameter, obj{kName: prefixPath, kValue: kValueX, kType: kString})
	c.ok(kPutParameter, obj{kName: "/prefix/other", kValue: "o", kType: kString})
	c.ok(kPutParameter, obj{kName: "/prefix/deep/one", kValue: "d", kType: kString})

	out := c.ok(kLabelParameterVersion, obj{
		kName: prefixPath, fieldVersion: 1,
		fieldLabels: []string{kLatest, "awsbad", "1st"},
	})
	require.Equal(t, []any{"awsbad", "1st"}, list(out, "InvalidLabels"))

	byLabel := list(c.ok(kGetParametersByPath, obj{
		kPath:        prefix,
		fieldFilters: []obj{{fieldKey: "Label", kValues: []string{kLatest}}},
	}), kParameters)
	require.Len(t, byLabel, 1)
	require.Equal(t, prefixPath, entry(byLabel, 0)[kName])
	require.Equal(t, kValueX, entry(byLabel, 0)[kValue])

	require.Len(t, list(c.ok(kGetParametersByPath, obj{kPath: prefix}), kParameters), 2)
	require.Len(t, list(c.ok(kGetParametersByPath, obj{kPath: "/prefix/", "Recursive": true}), kParameters), 3)

	c.ok(kPutParameter, obj{kName: prefixPath, kValue: "v2", kOverwrite: true})
	require.Equal(t, kValueX, param(c.ok(kGetParameter, obj{kName: "/prefix/path:latest"}))[kValue])
	require.Equal(t, "v2", param(c.ok(kGetParameter, obj{kName: "/prefix/path:2"}))[kValue])
	c.fails(kGetParameter, obj{kName: "/prefix/path:9"}, "ParameterVersionNotFound")

	history := list(c.ok(kGetParameterHistory, obj{kName: prefixPath}), kParameters)
	require.Len(t, history, 2)
	require.Equal(t, []any{kLatest}, entry(history, 0)[fieldLabels])
	require.Equal(t, []any{}, entry(history, 1)[fieldLabels])

	// A label moves to the version it is put on.
	c.ok(kLabelParameterVersion, obj{kName: prefixPath, fieldLabels: []string{kLatest}})
	require.Equal(t, "v2", param(c.ok(kGetParameter, obj{kName: "/prefix/path:latest"}))[kValue])

	un := c.ok("UnlabelParameterVersion", obj{
		kName: prefixPath, fieldVersion: 2,
		fieldLabels: []string{kLatest, "gone"},
	})
	require.Equal(t, []any{kLatest}, list(un, "RemovedLabels"))
	require.Equal(t, []any{"gone"}, list(un, "InvalidLabels"))

	c.fails(kLabelParameterVersion, obj{kName: "/missing", fieldLabels: []string{"l"}}, kParameterNotFound)
	c.fails(kGetParametersByPath, obj{
		kPath:        prefix,
		fieldFilters: []obj{{fieldKey: kName, kValues: []string{"x"}}},
	}, "InvalidFilterKey")
}

func Test_tagsAndFilters(t *testing.T) {
	c := newClient(t, ssm.New())

	c.ok(kPutParameter, obj{kName: tagA, kValue: "1", kType: kString, kTags: []obj{{fieldKey: kEnv, kValue: "dev"}}})
	c.ok(kPutParameter, obj{kName: tagB, kValue: "2", kType: "StringList"})

	c.ok("AddTagsToResource", obj{
		kResourceType: resourceParameter, kResourceID: tagB,
		kTags: []obj{{fieldKey: kEnv, kValue: envProd}},
	})

	tags := list(c.ok(kListTagsForResource, obj{kResourceType: resourceParameter, kResourceID: tagB}), kTagList)
	require.Equal(t, []any{map[string]any{fieldKey: kEnv, kValue: envProd}}, tags)

	prod := list(c.ok(kDescribeParameters, obj{fieldFilters: []obj{
		{fieldKey: "tag:env", kValues: []string{envProd}},
	}}), kParameters)
	require.Len(t, prod, 1)
	require.Equal(t, tagB, entry(prod, 0)[kName])

	lists := list(c.ok(kDescribeParameters, obj{"Filters": []obj{{fieldKey: kType, kValues: []string{"StringList"}}}}),
		kParameters)
	require.Len(t, lists, 1)

	c.ok("RemoveTagsFromResource", obj{kResourceType: resourceParameter, kResourceID: tagB, "TagKeys": []string{kEnv}})
	require.Empty(t, list(c.ok(kListTagsForResource, obj{kResourceType: resourceParameter, kResourceID: tagB}), kTagList))

	c.fails(kListTagsForResource, obj{kResourceType: resourceParameter, kResourceID: tagNone}, "InvalidResourceId")
	c.fails(kListTagsForResource, obj{kResourceType: "Nope", kResourceID: "x"}, "InvalidResourceType")
	c.fails(kDescribeParameters, obj{
		"Filters":    []obj{{fieldKey: kName, kValues: []string{"x"}}},
		fieldFilters: []obj{{fieldKey: kName, kValues: []string{"x"}}},
	}, kValidationException)

	deleted := c.ok("DeleteParameters", obj{fieldNames: []string{tagA, tagNone}})
	require.Equal(t, []any{tagA}, list(deleted, "DeletedParameters"))
	require.Equal(t, []any{tagNone}, list(deleted, kInvalidParameters))
}

func Test_maintenanceWindows(t *testing.T) {
	c := newClient(t, ssm.New())

	msg := c.fails(kGetMaintenanceWindow, obj{fieldWindowID: "mw-00000000000000000"}, "DoesNotExistException")
	require.Equal(t, "Maintenance window mw-00000000000000000 does not exist", msg)

	id := text(c.ok(kCreateMaintenanceWindow, obj{
		kName: "w", "Schedule": "cron(0 0 * * ? *)", "Duration": 2, "Cutoff": 1,
		"AllowUnassociatedTargets": false,
	})[fieldWindowID])
	require.True(t, strings.HasPrefix(id, "mw-"))
	require.Len(t, id, 20)

	require.Equal(t, "w", c.ok(kGetMaintenanceWindow, obj{fieldWindowID: id})[kName])
	require.Len(t, list(c.ok("DescribeMaintenanceWindows", obj{}), "WindowIdentities"), 1)
	c.ok(kDeleteMaintenanceWindow, obj{fieldWindowID: id})
	c.fails(kDeleteMaintenanceWindow, obj{fieldWindowID: id}, "DoesNotExistException")
}

func Test_otherOperationsAre501(t *testing.T) {
	c := newClient(t, ssm.New())

	status, _ := c.call("CreateDocument", obj{kName: "d", "Content": "{}"})
	require.Equal(t, http.StatusNotImplemented, status)
}

func Test_mutating(t *testing.T) {
	reg := ssm.New().Registration()

	for _, op := range []string{
		kPutParameter, "DeleteParameter", "DeleteParameters", kLabelParameterVersion,
		"UnlabelParameterVersion", "AddTagsToResource", "RemoveTagsFromResource", kCreateMaintenanceWindow,
		kDeleteMaintenanceWindow,
	} {
		require.True(t, reg.Mutating(op), op)
	}

	for _, op := range []string{
		kGetParameter, kGetParameters, kGetParametersByPath, kDescribeParameters,
		kGetParameterHistory, kListTagsForResource, kGetMaintenanceWindow, "CreateDocument",
	} {
		require.False(t, reg.Mutating(op), op)
	}
}

// entry is item i of a list, as an object.
func entry(l []any, i int) map[string]any {
	e, _ := l[i].(map[string]any)

	return e
}

// text is a string member of a response.
func text(v any) string {
	s, _ := v.(string)

	return s
}
