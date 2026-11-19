package main

import (
	"testing"

	"github.com/kylecannon/gostack/codegen/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validOneOptions turns the checks on with an enum override that narrows the
// accepted set, which no real service needs yet.
var validOneOptions = serviceOptions{Validation: &validation{
	Code:  "BadInput",
	Enums: map[string]enumText{"Mode": {Values: []string{"B", "A"}, Narrow: true}},
}}

// validOneModel has a constraint of every kind the checks act on, a pattern
// Go cannot compile, and structures below a list and a map.
const validOneModel = `{"metadata":{"protocol":"json","jsonVersion":"1.0","targetPrefix":"ValidOne",
	"apiVersion":"2020-01-01","endpointPrefix":"validone"},
	"operations":{"Do":{"name":"Do","input":{"shape":"DoRequest"},"output":{"shape":"DoResponse"}}},
	"shapes":{
		"String":{"type":"string"},
		"Id":{"type":"string"},
		"Name":{"type":"string","pattern":"[a-z]+","min":1,"max":4},
		"Secret":{"type":"string","max":3,"sensitive":true},
		"Mode":{"type":"string","enum":["A","B","C"]},
		"Code":{"type":"string","pattern":"^(?!x).+"},
		"Count":{"type":"integer","min":-5},
		"Item":{"type":"structure","required":["Key"],"members":{"Key":{"shape":"String"}}},
		"Items":{"type":"list","member":{"shape":"Item"},"max":2},
		"Entries":{"type":"map","key":{"shape":"String"},"value":{"shape":"Item"}},
		"DoRequest":{"type":"structure","required":["Id"],"members":{"Id":{"shape":"Id"},
			"Name":{"shape":"Name"},"Secret":{"shape":"Secret"},"Mode":{"shape":"Mode"},"Code":{"shape":"Code"},
			"Count":{"shape":"Count"},"Items":{"shape":"Items"},"Entries":{"shape":"Entries"}}},
		"DoResponse":{"type":"structure","members":{}}}}`

// validOneTest runs in the generated package of validOneModel.
const validOneTest = `package validone

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
)

type handler struct{ Unimplemented }

func (handler) Do(context.Context, *DoInput) (*DoOutput, error) {
	return &DoOutput{}, nil
}

func call(t *testing.T, validated bool, body string) (int, map[string]any) {
	t.Helper()

	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	req.Header.Set("X-Amz-Target", "ValidOne.Do")
	rec := httptest.NewRecorder()
	rc := &request.Context{Service: "validone", Protocol: protocol.KindJSON, Operation: "Do",
		RequestID: "req-1", Request: req}

	if validated {
		ServeValidated(handler{}, rec, rc)
	} else {
		Serve(handler{}, rec, rc)
	}

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}

	return rec.Code, out
}

const bad = ` + "`" + `{"Name":"ABCDEF","Secret":"abcd","Mode":"C","Code":"x","Count":-6,` + "`" + ` +
	` + "`" + `"Items":[{"Key":"k"},{}],"Entries":{"b":{},"a":{}}}` + "`" + `

func TestEveryFailureInModelOrder(t *testing.T) {
	code, out := call(t, true, bad)

	want := "9 validation errors detected: " +
		"Value null at 'id' failed to satisfy constraint: Member must not be null; " +
		"Value 'ABCDEF' at 'name' failed to satisfy constraint: Member must satisfy regular expression pattern: [a-z]+; " +
		"Value 'ABCDEF' at 'name' failed to satisfy constraint: Member must have length less than or equal to 4; " +
		"Value at 'secret' failed to satisfy constraint: Member must have length less than or equal to 3; " +
		"Value 'C' at 'mode' failed to satisfy constraint: Member must satisfy enum value set: [B, A]; " +
		"Value '-6' at 'count' failed to satisfy constraint: Member must have value greater than or equal to -5; " +
		"Value null at 'items.2.member.key' failed to satisfy constraint: Member must not be null; " +
		"Value null at 'entries.a.member.key' failed to satisfy constraint: Member must not be null; " +
		"Value null at 'entries.b.member.key' failed to satisfy constraint: Member must not be null"

	if code != 400 || out["__type"] != "BadInput" || out["message"] != want {
		t.Fatalf("got %d %v %q", code, out["__type"], out["message"])
	}
}

func TestOneFailureAndAListLength(t *testing.T) {
	_, out := call(t, true, ` + "`" + `{"Id":"i","Items":[{"Key":"a"},{"Key":"b"},{"Key":"c"}]}` + "`" + `)

	want := "1 validation error detected: Value at 'items' failed to satisfy constraint: " +
		"Member must have length less than or equal to 2"
	if out["message"] != want {
		t.Fatalf("got %q", out["message"])
	}
}

func TestValidRequestReachesHandler(t *testing.T) {
	code, out := call(t, true, ` + "`" + `{"Id":"i","Name":"ab","Mode":"A","Code":"x","Count":-5}` + "`" + `)
	if code != 200 {
		t.Fatalf("got %d %v", code, out)
	}
}

func TestServeDoesNotCheck(t *testing.T) {
	if code, out := call(t, false, bad); code != 200 {
		t.Fatalf("got %d %v", code, out)
	}
}

func TestUncheckedPatternIsListed(t *testing.T) {
	if validateUnchecked != 1 {
		t.Fatalf("validateUnchecked = %d", validateUnchecked)
	}
}
`

// Test_newModel_ValidationRefusedOnRest checks the option is refused where
// the rest dispatch would ignore it.
func Test_newModel_ValidationRefusedOnRest(t *testing.T) {
	t.Parallel()

	svc, err := spec.ParseBytes([]byte(`{"metadata":{"protocol":"rest-json","apiVersion":"1"},
		"operations":{},"shapes":{}}`))
	require.NoError(t, err)

	_, err = newModel("restone", serviceOptions{Validation: &validation{}}, svc, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Validation option")
}

// Test_validateFile_SSMListsLookaheadPatterns builds ssm with the checks on
// and checks a pattern with a lookahead is listed as unchecked, not compiled.
func Test_validateFile_SSMListsLookaheadPatterns(t *testing.T) {
	t.Parallel()

	loader, err := spec.NewLoader("../../../specs")
	require.NoError(t, err)

	p, err := loader.Botocore("ssm")
	require.NoError(t, err)

	opts := serviceTable["ssm"]
	opts.Validation = &validation{}

	g, err := newModel("ssm", opts, p, nil)
	require.NoError(t, err)

	src, err := g.validateFile()
	require.NoError(t, err)
	assert.Contains(t, src, `//   - OpsItemTitle: pattern ^(?!\s*$).+, which Go's regexp cannot compile`)
	assert.NotContains(t, src, `NewPattern("^(?:^(?!`)
}
