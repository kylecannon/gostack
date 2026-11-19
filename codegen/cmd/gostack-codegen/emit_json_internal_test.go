package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/codegen/spec"
	"github.com/stretchr/testify/require"
)

// attributeValue is the structure of jsonOneModel the options make a union.
const attributeValue = "AttributeValue"

// jsonOneModel is a json 1.1 service with errors that carry members, one of
// them under a message member named "Message", a union and a structure the
// service options treat as one.
const jsonOneModel = `{"metadata":{"protocol":"json","jsonVersion":"1.1","targetPrefix":"JsonOne_20200101",
	"apiVersion":"2020-01-01","endpointPrefix":"jsonone"},
	"operations":{"Do":{"name":"Do","input":{"shape":"DoRequest"},"output":{"shape":"DoResponse"},
		"errors":[{"shape":"Failed"},{"shape":"Canceled"}]}},
	"shapes":{
		"String":{"type":"string"},
		"Blob":{"type":"blob"},
		"Boolean":{"type":"boolean"},
		"DoRequest":{"type":"structure","members":{"Value":{"shape":"AttributeValue"},"Choice":{"shape":"Choice"}}},
		"DoResponse":{"type":"structure","members":{"Value":{"shape":"AttributeValue"}}},
		"AttributeValue":{"type":"structure","members":{"S":{"shape":"String"},"B":{"shape":"Blob"},
			"BOOL":{"shape":"Boolean"},"L":{"shape":"AttributeList"},"M":{"shape":"AttributeMap"}}},
		"AttributeList":{"type":"list","member":{"shape":"AttributeValue"}},
		"AttributeMap":{"type":"map","key":{"shape":"String"},"value":{"shape":"AttributeValue"}},
		"Choice":{"type":"structure","union":true,"members":{"A":{"shape":"String"},"B":{"shape":"String"}}},
		"Reasons":{"type":"list","member":{"shape":"String"}},
		"Failed":{"type":"structure","exception":true,"error":{"httpStatusCode":400,"senderFault":true},
			"members":{"message":{"shape":"String"},"Item":{"shape":"AttributeMap"}}},
		"Canceled":{"type":"structure","exception":true,
			"members":{"Message":{"shape":"String"},"Reasons":{"shape":"Reasons"}}}}}`

// jsonOneTest runs in the generated package of jsonOneModel.
const jsonOneTest = `package jsonone

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

func (handler) Do(_ context.Context, in *DoInput) (*DoOutput, error) {
	s := ""
	if in.Value != nil && in.Value.S != nil {
		s = *in.Value.S
	}

	switch s {
	case "fail":
		return nil, (&Failed{Item: map[string]AttributeValue{"id": {S: &s}}}).Err("it failed")
	case "cancel":
		return nil, (&Canceled{Reasons: []string{"one", "two"}}).Err("it was canceled")
	case "plain":
		return nil, NewCanceled("no reasons")
	}

	return &DoOutput{Value: in.Value}, nil
}

func call(t *testing.T, body string) (int, string, map[string]any) {
	t.Helper()

	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	rec := httptest.NewRecorder()

	Serve(handler{}, rec, &request.Context{Service: "jsonone", Protocol: protocol.KindJSON, Operation: "Do",
		RequestID: "req-1", Request: req})

	got := map[string]any{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}

	return rec.Code, rec.Header().Get("Content-Type"), got
}

func TestJSON11(t *testing.T) {
	if Metadata.TargetPrefix != "JsonOne_20200101" {
		t.Errorf("TargetPrefix %q", Metadata.TargetPrefix)
	}

	code, ct, got := call(t, ` + "`" + `{"Value":{"S":"x"}}` + "`" + `)
	if code != 200 || ct != "application/x-amz-json-1.1" {
		t.Errorf("success: status %d, Content-Type %q", code, ct)
	}

	if v, _ := got["Value"].(map[string]any); v["S"] != "x" {
		t.Errorf("success body %v", got)
	}

	code, ct, _ = call(t, ` + "`" + `{"Value":{"S":"plain"}}` + "`" + `)
	if code != 400 || ct != "application/x-amz-json-1.1" {
		t.Errorf("error: status %d, Content-Type %q", code, ct)
	}
}

func TestErrorMembers(t *testing.T) {
	tests := []struct {
		give string
		want map[string]any
	}{
		{"fail", map[string]any{"__type": "Failed", "message": "it failed",
			"Item": map[string]any{"id": map[string]any{"S": "fail"}}}},
		{"cancel", map[string]any{"__type": "Canceled", "Message": "it was canceled",
			"Reasons": []any{"one", "two"}}},
		{"plain", map[string]any{"__type": "Canceled", "Message": "no reasons"}},
	}

	for _, tt := range tests {
		_, _, got := call(t, ` + "`" + `{"Value":{"S":"` + "`" + ` + tt.give + ` + "`" + `"}}` + "`" + `)
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(tt.want)

		if string(gotJSON) != string(wantJSON) {
			t.Errorf("%s: got %s, want %s", tt.give, gotJSON, wantJSON)
		}
	}
}

func TestMembersSet(t *testing.T) {
	s, f := "", false

	tests := []struct {
		name string
		give interface{ MembersSet() int }
		want int
	}{
		{"none", &AttributeValue{}, 0},
		{"an empty string", &AttributeValue{S: &s}, 1},
		{"an empty list and a false boolean", &AttributeValue{L: []AttributeValue{}, BOOL: &f}, 2},
		{"an empty map", &AttributeValue{M: map[string]AttributeValue{}}, 1},
		{"a union with none", &Choice{}, 0},
		{"a union with both", &Choice{A: &s, B: &s}, 2},
	}

	for _, tt := range tests {
		if got := tt.give.MembersSet(); got != tt.want {
			t.Errorf("%s: %d members set, want %d", tt.name, got, tt.want)
		}
	}

	decoded := map[string]int{
		` + "`" + `{"B":""}` + "`" + `: 1,
		` + "`" + `{"L":[],"M":{}}` + "`" + `: 2,
		` + "`" + `{}` + "`" + `: 0,
	}
	for body, want := range decoded {
		in, err := decodeDoJSON([]byte(` + "`" + `{"Value":` + "`" + ` + body + ` + "`" + `}` + "`" + `))
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}

		if got := in.Value.MembersSet(); got != want {
			t.Errorf("decoded %s: %d members set, want %d", body, got, want)
		}
	}
}
`

// queryOneModel is a query service with a non-flattened map whose value is a
// structure holding a blob, as SNS's MessageAttributeMap, on both the input
// and the output, and an error with a member beside its message.
const queryOneModel = `{"metadata":{"protocol":"query","apiVersion":"2020-01-01","endpointPrefix":"queryone",
	"xmlNamespace":"http://queryone.example/doc/2020-01-01/"},
	"operations":{"Get":{"name":"Get","input":{"shape":"GetInput"},
		"output":{"shape":"GetResponse","resultWrapper":"GetResult"},"errors":[{"shape":"Verification"}]}},
	"shapes":{
		"String":{"type":"string"},
		"Binary":{"type":"blob"},
		"GetInput":{"type":"structure","members":{"Attrs":{"shape":"AttributeMap"}}},
		"GetResponse":{"type":"structure","members":{"Attrs":{"shape":"AttributeMap"}}},
		"AttributeMap":{"type":"map","key":{"shape":"String","locationName":"Name"},
			"value":{"shape":"AttributeValue","locationName":"Value"}},
		"AttributeValue":{"type":"structure","required":["DataType"],
			"members":{"DataType":{"shape":"String"},"BinaryValue":{"shape":"Binary"}}},
		"Verification":{"type":"structure","exception":true,"error":{"code":"Verification","httpStatusCode":400,
			"senderFault":true},"members":{"Message":{"shape":"String"},"Status":{"shape":"String"}}}}}`

// queryOneTest runs in the generated package of queryOneModel.
const queryOneTest = `package queryone

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
)

type handler struct{ Unimplemented }

func (handler) Get(_ context.Context, in *GetInput) (*GetOutput, error) {
	if _, ok := in.Attrs["fail"]; ok {
		status := "Pending"

		return nil, (&Verification{Status: &status}).Err("not verified")
	}

	return &GetOutput{Attrs: in.Attrs}, nil
}

func call(form string) (int, string) {
	req := httptest.NewRequest("POST", "/", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()

	Serve(handler{}, rec, &request.Context{Service: "queryone", Protocol: protocol.KindQuery, Operation: "Get",
		RequestID: "req-1", Request: req})

	return rec.Code, rec.Body.String()
}

func TestStructMapRoundTrip(t *testing.T) {
	code, body := call("Action=Get&Attrs.entry.1.Name=a&Attrs.entry.1.Value.DataType=Binary" +
		"&Attrs.entry.1.Value.BinaryValue=AQI%3D")

	want := "<GetResult><Attrs><entry><Name>a</Name><Value><DataType>Binary</DataType>" +
		"<BinaryValue>AQI=</BinaryValue></Value></entry></Attrs></GetResult>"
	if code != 200 || !strings.Contains(body, want) {
		t.Errorf("status %d, body %s, want it to hold %s", code, body, want)
	}
}

func TestErrorMembers(t *testing.T) {
	code, body := call("Action=Get&Attrs.entry.1.Name=fail&Attrs.entry.1.Value.DataType=String")

	want := "<Code>Verification</Code><Message>not verified</Message><Type>Sender</Type></Error>" +
		"<RequestId>req-1</RequestId><Status>Pending</Status></ErrorResponse>"
	if code != 400 || !strings.Contains(body, want) {
		t.Errorf("status %d, body %s, want it to hold %s", code, body, want)
	}
}
`

// Test_files_JSONAndQuerySeams generates the packages of jsonOneModel and
// queryOneModel into a scratch directory under testdata, inside the module so
// that they can import its internal packages, and runs a test in each: json
// 1.1 content types and target prefix, json and query errors with members,
// MembersSet, and a query map of structures holding a blob, both ways.
func Test_files_JSONAndQuerySeams(t *testing.T) {
	t.Parallel()

	require.NoError(t, os.MkdirAll("testdata", 0o750))

	//nolint:usetesting // t.TempDir is outside the module, where internal packages cannot be imported
	scratch, err := os.MkdirTemp("testdata", "seams-*")
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, os.RemoveAll(scratch))

		_ = os.Remove("testdata") // only when nothing else is in it
	})

	cases := []struct {
		name  string
		model string
		opts  serviceOptions
		test  string
	}{
		{"jsonone", jsonOneModel, serviceOptions{Unions: true, UnionShapes: []string{attributeValue}}, jsonOneTest},
		{"queryone", queryOneModel, serviceOptions{}, queryOneTest},
		{"validone", validOneModel, validOneOptions, validOneTest},
	}

	pkgs := make([]string, 0, len(cases))

	for _, c := range cases {
		svc, err := spec.ParseBytes([]byte(c.model))
		require.NoError(t, err)

		g, err := newModel(c.name, c.opts, svc, nil)
		require.NoError(t, err)

		files, err := g.files()
		require.NoError(t, err)

		dir := filepath.Join(scratch, c.name)
		require.NoError(t, os.MkdirAll(dir, 0o750))

		for name, src := range files {
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), src, 0o600))
		}

		require.NoError(t, os.WriteFile(filepath.Join(dir, "seam_test.go"), []byte(c.test), 0o600))

		pkgs = append(pkgs, "./"+filepath.ToSlash(dir))
	}

	args := slices.Concat([]string{"test", "-count=1"}, pkgs)
	cmd := exec.CommandContext(t.Context(), "go", args...)
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "GIT_") })

	var out bytes.Buffer

	cmd.Stdout, cmd.Stderr = &out, &out

	require.NoError(t, cmd.Run(), out.String())
}

func Test_newModel_UnionShapes(t *testing.T) {
	t.Parallel()

	svc, err := spec.ParseBytes([]byte(jsonOneModel))
	require.NoError(t, err)

	tests := []struct {
		name    string
		opts    serviceOptions
		want    []string
		wantErr string
	}{
		{name: "off", opts: serviceOptions{}, want: nil},
		{name: "model unions", opts: serviceOptions{Unions: true}, want: []string{"Choice"}},
		{name: "a named shape", opts: serviceOptions{UnionShapes: []string{attributeValue}}, want: []string{attributeValue}},
		{name: "an unknown shape", opts: serviceOptions{UnionShapes: []string{"Nope"}}, wantErr: "union shape Nope"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			g, err := newModel("jsonone", tt.opts, svc, nil)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)

				return
			}

			require.NoError(t, err)

			var got []string

			for _, st := range g.sortedStructs() {
				if st.union {
					got = append(got, st.goName)
				}
			}

			require.Equal(t, tt.want, got)
		})
	}
}
