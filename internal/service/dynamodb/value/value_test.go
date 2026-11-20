package value_test

import (
	"errors"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The cases below were answered by gostack-parity-localstack:8b9a79f; the
// probes are under the SQ-214 verification directory.

const notANumber = "A value provided cannot be converted into a number"

func message(t *testing.T, err error) string {
	t.Helper()

	awsErr, ok := errors.AsType[*awserr.Error](err)
	require.True(t, ok, "not an *awserr.Error: %v", err)
	require.Equal(t, "ValidationException", awsErr.Code)

	return awsErr.Message
}

func Test_ParseNumber(t *testing.T) {
	tests := []struct {
		give    string
		want    string
		wantErr string
	}{
		{give: "1.50", want: "1.5"},
		{give: "-0.000E+5", want: "0"},
		{give: "-0", want: "0"},
		{give: "0.0", want: "0"},
		{give: "1e5", want: "100000"},
		{give: ".5", want: "0.5"},
		{give: "5.", want: "5"},
		{give: "+5", want: "5"},
		{give: "00012.3400", want: "12.34"},
		{give: "1.23E-3", want: "0.00123"},
		{give: "1E+2", want: "100"},
		{give: "-1E-130", want: "-0." + zeros(129) + "1"},
		{
			give: "12345678901234567890123456789012345678E+87",
			want: "12345678901234567890123456789012345678" + zeros(87),
		},
		{
			give: "9.9999999999999999999999999999999999999E+125",
			want: "99999999999999999999999999999999999999" + zeros(88),
		},
		{give: "1234567890123456789012345678901234567800", want: "1234567890123456789012345678901234567800"},
		{give: "1.2345678901234567890123456789012345678", want: "1.2345678901234567890123456789012345678"},
		{give: "123456789012345678901234567890123456789", wantErr: "DynamoDB only supports precision up to 38 digits"},
		{
			give:    "1E126",
			wantErr: "Number overflow. Attempting to store a number with magnitude larger than supported range",
		},
		{
			give:    "1E-131",
			wantErr: "Number underflow. Attempting to store a number with magnitude smaller than supported range",
		},
		{
			give:    "1E999999999999999999999",
			wantErr: "Number overflow. Attempting to store a number with magnitude larger than supported range",
		},
		{give: "  12", wantErr: notANumber},
		{give: "", wantErr: notANumber},
		{give: "Infinity", wantErr: notANumber},
		{give: "0x10", wantErr: notANumber},
		{give: "E5", wantErr: notANumber},
		{give: "1e", wantErr: notANumber},
		{give: "1.2.3", wantErr: notANumber},
		{give: "-", wantErr: notANumber},
	}

	for _, tt := range tests {
		t.Run(tt.give, func(t *testing.T) {
			got, err := value.ParseNumber(tt.give)
			if tt.wantErr != "" {
				assert.Equal(t, tt.wantErr, message(t, err))

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got.String())
		})
	}
}

func zeros(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '0'
	}

	return string(b)
}

func Test_Number_Compare(t *testing.T) {
	order := []string{"-1E10", "-12.5", "-1", "-0.001", "0", "0.001", "0.5", "1", "1.5", "2", "10", "100", "1E30"}

	for i, a := range order {
		for j, b := range order {
			x, err := value.ParseNumber(a)
			require.NoError(t, err)
			y, err := value.ParseNumber(b)
			require.NoError(t, err)

			want := 0

			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			default:
			}

			assert.Equal(t, want, x.Compare(y), "%s vs %s", a, b)
		}
	}
}

func Test_Validate(t *testing.T) {
	tests := []struct {
		name    string
		give    api.AttributeValue
		wantErr string
	}{
		{
			name:    "empty value",
			give:    api.AttributeValue{},
			wantErr: "Supplied AttributeValue is empty, must contain exactly one of the supported datatypes",
		},
		{
			name: "two members",
			give: api.AttributeValue{S: new("a"), N: new("1")},
			wantErr: "Supplied AttributeValue has more than one datatypes set, " +
				"must contain exactly one of the supported datatypes",
		},
		{
			name:    "empty string set",
			give:    api.AttributeValue{SS: []string{}},
			wantErr: "One or more parameter values were invalid: An string set  may not be empty",
		},
		{
			name:    "empty number set",
			give:    api.AttributeValue{NS: []string{}},
			wantErr: "One or more parameter values were invalid: An number set  may not be empty",
		},
		{
			name:    "empty binary set",
			give:    api.AttributeValue{BS: [][]byte{}},
			wantErr: "One or more parameter values were invalid: Binary sets should not be empty",
		},
		{
			name:    "duplicate strings",
			give:    api.AttributeValue{SS: []string{"a", "a"}},
			wantErr: "One or more parameter values were invalid: Input collection [a, a] contains duplicates",
		},
		{
			name:    "numbers equal once canonical",
			give:    api.AttributeValue{NS: []string{"1", "1.0"}},
			wantErr: "Input collection contains duplicates",
		},
		{
			name:    "duplicate binaries",
			give:    api.AttributeValue{BS: [][]byte{{1}, {1}}},
			wantErr: "One or more parameter values were invalid: Input collection of type BS contains duplicates.",
		},
		{
			name:    "null false",
			give:    api.AttributeValue{NULL: new(false)},
			wantErr: "One or more parameter values were invalid: Null attribute value types must have the value of true",
		},
		{
			name:    "bad number",
			give:    api.AttributeValue{N: new("abc")},
			wantErr: notANumber,
		},
		{
			name:    "empty name in a map",
			give:    api.AttributeValue{M: value.Item{"": {S: new("x")}}},
			wantErr: "Empty attribute name",
		},
		{
			name:    "empty value in a map",
			give:    api.AttributeValue{M: value.Item{"a": {}}},
			wantErr: "Supplied AttributeValue is empty, must contain exactly one of the supported datatypes",
		},
		{
			name:    "empty set in a list",
			give:    api.AttributeValue{L: []api.AttributeValue{{SS: []string{}}}},
			wantErr: "One or more parameter values were invalid: An string set  may not be empty",
		},
		{name: "empty string", give: api.AttributeValue{S: new("")}},
		{name: "empty binary", give: api.AttributeValue{B: []byte{}}},
		{
			name: "empty map and list",
			give: api.AttributeValue{L: []api.AttributeValue{{M: value.Item{}}, {L: []api.AttributeValue{}}}},
		},
		{name: "false", give: api.AttributeValue{BOOL: new(false)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := value.Validate(&tt.give)
			if tt.wantErr == "" {
				require.NoError(t, err)

				return
			}

			assert.Equal(t, tt.wantErr, message(t, err))
		})
	}
}

func Test_Validate_canonicalizesNumbers(t *testing.T) {
	item := value.Item{
		"n":  {N: new("01.50")},
		"ns": {NS: []string{"1e2", "-0.0"}},
		"m":  {M: value.Item{"deep": {L: []api.AttributeValue{{N: new("2E0")}}}}},
	}

	require.NoError(t, value.ValidateItem(item))
	assert.Equal(t, "1.5", *item["n"].N)
	assert.Equal(t, []string{"100", "0"}, item["ns"].NS)
	assert.Equal(t, "2", *item["m"].M["deep"].L[0].N)
}

func Test_ValidateItem_emptyName(t *testing.T) {
	assert.Equal(t, "Empty attribute name", message(t, value.ValidateItem(value.Item{"": {S: new("x")}})))
}

func Test_Compare(t *testing.T) {
	tests := []struct {
		name string
		a, b api.AttributeValue
		want int
	}{
		{name: "strings by bytes", a: api.AttributeValue{S: new("B")}, b: api.AttributeValue{S: new("a")}, want: -1},
		{name: "numbers by value", a: api.AttributeValue{N: new("10")}, b: api.AttributeValue{N: new("9")}, want: 1},
		{
			name: "binaries by bytes",
			a:    api.AttributeValue{B: []byte{0x90}},
			b:    api.AttributeValue{B: []byte{0x10, 0xff}},
			want: 1,
		},
		{name: "equal numbers", a: api.AttributeValue{N: new("-1.5")}, b: api.AttributeValue{N: new("-1.5")}, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, value.Compare(&tt.a, &tt.b))
		})
	}
}

func Test_Equal(t *testing.T) {
	a := api.AttributeValue{M: value.Item{
		"ss": {SS: []string{"a", "b"}},
		"l":  {L: []api.AttributeValue{{N: new("1")}, {NULL: new(true)}}},
		"bs": {BS: [][]byte{{1}, {2}}},
	}}
	b := api.AttributeValue{M: value.Item{
		"ss": {SS: []string{"b", "a"}},
		"l":  {L: []api.AttributeValue{{N: new("1")}, {NULL: new(true)}}},
		"bs": {BS: [][]byte{{2}, {1}}},
	}}

	assert.True(t, value.Equal(&a, &b), "sets compare in any order")

	b.M["l"] = api.AttributeValue{L: []api.AttributeValue{{NULL: new(true)}, {N: new("1")}}}
	assert.False(t, value.Equal(&a, &b), "lists compare in order")
	assert.False(t, value.Equal(&api.AttributeValue{S: new("1")}, &api.AttributeValue{N: new("1")}), "types differ")
}

func Test_ItemSize(t *testing.T) {
	item := value.Item{
		"s":  {S: new("hello")},                            // 1 + 5
		"n":  {N: new("12345")},                            // 1 + 3 + 1
		"b":  {B: []byte{1, 2}},                            // 1 + 2
		"t":  {BOOL: new(true)},                            // 1 + 1
		"ss": {SS: []string{"ab", "c"}},                    // 2 + 3
		"m":  {M: value.Item{"k": {S: new("v")}}},          // 1 + 3 + 1 + (1 + 1)
		"l":  {L: []api.AttributeValue{{NULL: new(true)}}}, // 1 + 3 + 1 + 1
	}

	assert.Equal(t, 6+5+3+2+5+7+6, value.ItemSize(item))
}

// Test_AppendKeyString checks it appends what KeyString returns.
func Test_AppendKeyString(t *testing.T) {
	str, num := "a\x00b", "-1.5"

	for _, v := range []api.AttributeValue{{S: &str}, {N: &num}, {B: []byte{0, 1}}, {B: []byte{}}, {}} {
		assert.Equal(t, "x"+value.KeyString(&v), string(value.AppendKeyString([]byte("x"), &v)))
	}
}
