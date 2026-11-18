package protocol_test

import (
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type pyInt struct {
	In string `json:"in"`
	Ok bool   `json:"ok"`
	V  int64  `json:"v"`
}

type pyFloat struct {
	In  string  `json:"in"`
	Ok  bool    `json:"ok"`
	V   float64 `json:"v"`
	NaN bool    `json:"nan"`
	Inf int     `json:"inf"`
}

type pyBase64 struct {
	In  string `json:"in"`
	Ok  bool   `json:"ok"`
	Hex string `json:"hex"`
}

type pyTable struct {
	Int    []pyInt    `json:"int"`
	Float  []pyFloat  `json:"float"`
	Base64 []pyBase64 `json:"base64"`
}

func loadPyTable(t *testing.T) pyTable {
	t.Helper()

	raw, err := os.ReadFile("testdata/python-parse.json")
	require.NoError(t, err)

	var table pyTable

	require.NoError(t, json.Unmarshal(raw, &table))

	return table
}

// Test_ParsePyInt_matches_Python runs every input of testdata/python-parse.py
// through int() in Python 3.13 and expects the same answer.
func Test_ParsePyInt_matches_Python(t *testing.T) {
	t.Parallel()

	for _, c := range loadPyTable(t).Int {
		got, ok := protocol.ParsePyInt(c.In)

		assert.Equal(t, c.Ok, ok, "%q", c.In)

		if c.Ok {
			assert.Equal(t, c.V, got, "%q", c.In)
		}
	}
}

func Test_PyIntDecimal_prints_what_Python_prints(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"3000000000":              "3000000000",
		"-3000000000":             "-3000000000",
		"+0003_000_000_000":       "3000000000",
		"99999999999999999999999": "99999999999999999999999",
		"-0":                      "0",
		"٣٠٠٠٠٠٠٠٠٠":              "3000000000",
	} {
		got, ok := protocol.PyIntDecimal(in)

		require.True(t, ok, in)
		assert.Equal(t, want, got, in)
	}

	for _, in := range []string{"", "5.0", "0x10", "1_", "abc"} {
		_, ok := protocol.PyIntDecimal(in)

		assert.False(t, ok, in)
	}
}

func Test_ParsePyFloat_matches_Python(t *testing.T) {
	t.Parallel()

	for _, c := range loadPyTable(t).Float {
		got, ok := protocol.ParsePyFloat(c.In)

		assert.Equal(t, c.Ok, ok, "%q", c.In)

		switch {
		case !c.Ok:
		case c.NaN:
			assert.True(t, math.IsNaN(got), "%q", c.In)
		case c.Inf != 0:
			assert.True(t, math.IsInf(got, c.Inf), "%q", c.In)
		default:
			assert.InDelta(t, c.V, got, 0, "%q", c.In)
		}
	}
}

func Test_ParsePyBase64_matches_Python(t *testing.T) {
	t.Parallel()

	table := loadPyTable(t).Base64
	require.Greater(t, len(table), 6000)

	for _, c := range table {
		got, ok := protocol.ParsePyBase64(c.In)

		assert.Equal(t, c.Ok, ok, "%q", c.In)

		if c.Ok {
			assert.Equal(t, c.Hex, hex.EncodeToString(got), "%q", c.In)
		}
	}
}
