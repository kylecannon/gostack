package main

import (
	"fmt"
	"testing"

	"github.com/kylecannon/gostack/codegen/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// doRequestID is the table key of validOneModel's required Id.
	doRequestID = "DoRequest.Id"
	// itemKey is the table key of validOneModel's required Item.Key.
	itemKey = "Item.Key"
)

// validOneOptions lists a constraint of every kind the checks act on, with an
// enum override that narrows the accepted set, which no real service needs
// yet. Name's min, Note, Code's pattern and Deep are left unlisted.
var validOneOptions = serviceOptions{Validation: &validation{
	Code: "BadInput",
	Checks: map[string]check{
		doRequestID:          checkRequired,
		"DoRequest.Name":     checkPattern | checkMax,
		"DoRequest.Secret":   checkMax,
		"DoRequest.Mode":     checkEnum,
		"DoRequest.Count":    checkMin,
		"DoRequest.Items":    checkMax,
		itemKey:              checkRequired,
		"LaterRequest.Token": checkRequired,
	},
	Enums: map[string]enumText{"Mode": {Values: []string{"B", "A"}, Narrow: true}},
}}

// validOneModel has a constraint of every kind the checks act on, a pattern
// Go cannot compile, structures below a list and a map, a structure only
// below a list inside a map, and a second operation to call Validate from.
const validOneModel = `{"metadata":{"protocol":"json","jsonVersion":"1.0","targetPrefix":"ValidOne",
	"apiVersion":"2020-01-01","endpointPrefix":"validone"},
	"operations":{"Do":{"name":"Do","input":{"shape":"DoRequest"},"output":{"shape":"DoResponse"}},
		"Later":{"name":"Later","input":{"shape":"LaterRequest"},"output":{"shape":"LaterResponse"}}},
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
		"Groups":{"type":"map","key":{"shape":"String"},"value":{"shape":"Items"}},
		"Deep":{"type":"structure","members":{"V":{"shape":"Name"}}},
		"DeepList":{"type":"list","member":{"shape":"Deep"}},
		"DeepGroups":{"type":"map","key":{"shape":"String"},"value":{"shape":"DeepList"}},
		"DoRequest":{"type":"structure","required":["Id"],"members":{"Id":{"shape":"Id"},
			"Name":{"shape":"Name"},"Note":{"shape":"Name"},"Secret":{"shape":"Secret"},"Mode":{"shape":"Mode"},
			"Code":{"shape":"Code"},"Count":{"shape":"Count"},"Items":{"shape":"Items"},"Entries":{"shape":"Entries"},
			"Groups":{"shape":"Groups"},"Deep":{"shape":"DeepGroups"}}},
		"LaterRequest":{"type":"structure","required":["Token"],"members":{"Token":{"shape":"String"}}},
		"DoResponse":{"type":"structure","members":{}},
		"LaterResponse":{"type":"structure","members":{}}}}`

// validOneTest runs in the generated package of validOneModel.
const validOneTest = `package validone

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
)

// handler runs the checks first, as a service's handler does where LocalStack
// makes them.
type handler struct{ Unimplemented }

func (handler) Do(_ context.Context, in *DoInput) (*DoOutput, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}

	return &DoOutput{}, nil
}

func (handler) Later(_ context.Context, in *LaterInput) (*LaterOutput, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}

	return &LaterOutput{}, nil
}

// lax answers without running the checks.
type lax struct{ Unimplemented }

func (lax) Do(context.Context, *DoInput) (*DoOutput, error) {
	return &DoOutput{}, nil
}

func (lax) Later(context.Context, *LaterInput) (*LaterOutput, error) {
	return &LaterOutput{}, nil
}

func call(t *testing.T, serve func(Handler, http.ResponseWriter, *request.Context), h Handler, op, body string,
) (int, map[string]any) {
	t.Helper()

	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	req.Header.Set("X-Amz-Target", "ValidOne."+op)
	rec := httptest.NewRecorder()
	rc := &request.Context{Service: "validone", Protocol: protocol.KindJSON, Operation: op,
		RequestID: "req-1", Request: req}

	serve(h, rec, rc)

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}

	return rec.Code, out
}

const bad = ` + "`" + `{"Name":"ABCDEF","Secret":"abcd","Mode":"C","Code":"x","Count":-6,` + "`" + ` +
	` + "`" + `"Items":[{"Key":"k"},{}],"Entries":{"b":{},"a":{}}}` + "`" + `

func TestEveryFailureInModelOrder(t *testing.T) {
	code, out := call(t, ServeValidated, handler{}, "Do", bad)

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
	_, out := call(t, ServeValidated, handler{}, "Do",
		` + "`" + `{"Id":"i","Items":[{"Key":"a"},{"Key":"b"},{"Key":"c"}]}` + "`" + `)

	want := "1 validation error detected: Value at 'items' failed to satisfy constraint: " +
		"Member must have length less than or equal to 2"
	if out["message"] != want {
		t.Fatalf("got %q", out["message"])
	}
}

// Name's min, every trait of Note, Code's pattern and Deep's V are unlisted.
func TestUnlistedReachesHandler(t *testing.T) {
	body := ` + "`" + `{"Id":"i","Name":"ab","Note":"NOT-OK","Mode":"A","Code":"x","Count":-5,` + "`" + ` +
		` + "`" + `"Groups":{"g":[{}]},"Deep":{"d":[{"V":"TOO-LONG"}]}}` + "`" + `
	if code, out := call(t, ServeValidated, handler{}, "Do", body); code != 200 {
		t.Fatalf("got %d %v", code, out)
	}
}

// Neither Serve nor ServeValidated runs the checks: a request that breaks
// them, and one whose required member is missing, reach the handler.
func TestDispatchNeverChecks(t *testing.T) {
	for name, serve := range map[string]func(Handler, http.ResponseWriter, *request.Context){
		"Serve": Serve, "ServeValidated": ServeValidated,
	} {
		if code, out := call(t, serve, lax{}, "Do", bad); code != 200 {
			t.Fatalf("%s Do: got %d %v", name, code, out)
		}

		if code, out := call(t, serve, lax{}, "Later", "{}"); code != 200 {
			t.Fatalf("%s Later: got %d %v", name, code, out)
		}
	}
}

// Validate gives the error a handler answers where it runs the checks.
func TestHandlerRunsChecks(t *testing.T) {
	code, out := call(t, ServeValidated, handler{}, "Later", "{}")
	if code != 400 {
		t.Fatalf("got %d %v", code, out)
	}

	want := "1 validation error detected: Value null at 'token' failed to satisfy constraint: Member must not be null"
	if err := (&LaterInput{}).Validate(); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Validate() = %v", err)
	}

	token := "t"
	if err := (&LaterInput{Token: &token}).Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
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

// Test_newModel_ValidationRefusesBadEntries checks a table entry the
// generator cannot check fails generation instead of checking nothing.
func Test_newModel_ValidationRefusesBadEntries(t *testing.T) {
	t.Parallel()

	svc, err := spec.ParseBytes([]byte(validOneModel))
	require.NoError(t, err)

	tests := []struct {
		name    string
		val     validation
		wantErr string
	}{
		{"an unknown shape", validation{Checks: map[string]check{"Nope.X": checkMax}}, "no structure Nope"},
		{"an unknown member", validation{Checks: map[string]check{"DoRequest.Nope": checkMax}}, "has no member Nope"},
		{
			"a trait the member lacks",
			validation{Checks: map[string]check{"DoRequest.Secret": checkMin | checkEnum}},
			"has no min, enum to check",
		},
		{
			"a lookahead pattern",
			validation{Checks: map[string]check{"DoRequest.Code": checkPattern}},
			"no pattern Go's regexp can compile",
		},
		{
			"a member below a list inside a map",
			validation{Checks: map[string]check{"Deep.V": checkMax}},
			"Deep.V: no operation input reaches it",
		},
		{
			"the Go name of an input, where the model names DoRequest",
			validation{Checks: map[string]check{"DoInput.Name": checkMax}},
			"no structure DoInput in the model",
		},
		{
			"a shape in another case",
			validation{Checks: map[string]check{"item.Key": checkRequired}},
			"no structure item in the model",
		},
		{
			"a second key for a field, by its Go name",
			validation{Checks: map[string]check{"DoRequest.Name": checkMax, "DoInput.Name": checkMin}},
			"DoInput.Name: no structure DoInput in the model",
		},
		{
			"a second key for a field, in another case",
			validation{Checks: map[string]check{itemKey: checkRequired, "item.Key": checkRequired}},
			"item.Key: no structure item in the model",
		},
		{
			"a member in another case",
			validation{Checks: map[string]check{"Item.key": checkRequired}},
			"Item has no member key",
		},
		{
			"a key with no member",
			validation{Checks: map[string]check{"Item.": checkRequired}},
			"Item has no member ",
		},
		{
			"an output shape",
			validation{Checks: map[string]check{"DoResponse.X": checkRequired}},
			"DoResponse has no member X",
		},
		{
			"no check at all",
			validation{Checks: map[string]check{itemKey: 0}},
			"at least one check",
		},
		{
			"a trait bit the generator does not know",
			validation{Checks: map[string]check{itemKey: checkRequired | 1<<6}},
			"check bits 0b1000000 are not ones the generator knows",
		},
		{
			"an enum override for no shape",
			validation{
				Checks: map[string]check{"DoRequest.Mode": checkEnum},
				Enums:  map[string]enumText{"Nope": {Values: []string{"x"}}},
			},
			"Validation.Enums Nope: no such shape in the model",
		},
		{
			"an enum override no listed check uses",
			validation{
				Checks: map[string]check{doRequestID: checkRequired},
				Enums:  map[string]enumText{"Mode": {Values: []string{"x"}}},
			},
			"Validation.Enums Mode: no listed enum check uses it",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := newModel("validone", serviceOptions{Validation: &tt.val}, svc, nil)
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// cycleModel is the reference cycle In -> A -> B -> A, where A reaches B
// before its own constrained member X. A struct met again inside the cycle
// must not be settled as having nothing to check before the cycle is.
const cycleModel = `{"metadata":{"protocol":"json","jsonVersion":"1.0","targetPrefix":"Cycle",
	"apiVersion":"2020-01-01","endpointPrefix":"cycle"},
	"operations":{"Do":{"name":"Do","input":{"shape":"In"},"output":{"shape":"Out"}}},
	"shapes":{
		"Short":{"type":"string","min":1},
		"In":{"type":"structure","members":{"A":{"shape":"A"}}},
		"A":{"type":"structure",%s"members":{"B":{"shape":"B"},"X":{"shape":"Short"}}},
		"B":{"type":"structure","members":{"A":{"shape":"A"}}},
		"Out":{"type":"structure","members":{}}}}`

// Test_markValidation_ReferenceCycle checks every struct of the cycle gets a
// validate method when only A.X is listed, so a.b.a.x is checked.
func Test_markValidation_ReferenceCycle(t *testing.T) {
	t.Parallel()

	svc, err := spec.ParseBytes([]byte(fmt.Sprintf(cycleModel, "")))
	require.NoError(t, err)

	opts := serviceOptions{Validation: &validation{Checks: map[string]check{"A.X": checkMin}}}

	g, err := newModel("cycle", opts, svc, nil)
	require.NoError(t, err)

	assert.True(t, g.ops["Do"].input.validates, "In")
	assert.True(t, g.structs["A"].validates, "A")
	assert.True(t, g.structs["B"].validates, "B")
}

// Test_markChecks_ReferenceCycle is the same cycle for the missing-member
// check, with X required.
func Test_markChecks_ReferenceCycle(t *testing.T) {
	t.Parallel()

	svc, err := spec.ParseBytes([]byte(fmt.Sprintf(cycleModel, `"required":["X"],`)))
	require.NoError(t, err)

	g, err := newModel("cycle", serviceOptions{}, svc, nil)
	require.NoError(t, err)

	assert.True(t, g.ops["Do"].input.needsCheck, "In")
	assert.True(t, g.structs["A"].needsCheck, "A")
	assert.True(t, g.structs["B"].needsCheck, "B")
}
