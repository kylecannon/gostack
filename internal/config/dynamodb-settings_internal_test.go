package config

import (
	"math"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

func dynamoLookup(name, value string) lookup {
	return func(n string) string {
		if n == name {
			return value
		}

		return ""
	}
}

// Test_parseDynamoDB_probability: the probabilities read as LocalStack's
// config.py:928 does, float(value.strip() or 0.0). What Python's float()
// accepts starts the service, and what it refuses stops it.
func Test_parseDynamoDB_probability(t *testing.T) {
	tests := []struct {
		name    string
		give    string
		want    float64
		wantErr bool
	}{
		{name: "empty is 0.0", give: "", want: 0},
		{name: "white space alone strips to empty, so 0.0", give: " \t ", want: 0},
		{name: "plain number", give: "0.75", want: 0.75},
		{name: "surrounding white space", give: " 0.25 ", want: 0.25},
		{name: "information separators, which str.strip() strips", give: "\x1c\x1f0.5\x1d\x1e", want: 0.5},
		{name: "underscores between digits", give: "1_0", want: 10},
		{name: "underscores in the fraction and exponent", give: "0.2_5e-1_0", want: 0.25e-10},
		{name: "leading dot", give: ".5", want: 0.5},
		{name: "trailing dot", give: "5.", want: 5},
		{name: "exponent", give: "5E-1", want: 0.5},
		{name: "inf", give: "+Inf", want: math.Inf(1)},
		{name: "signed infinity in any case", give: "-InFiNiTy", want: math.Inf(-1)},
		{name: "nan", give: "NaN", want: math.NaN()},
		{name: "hex float, which Python refuses", give: "0x1p-1", wantErr: true},
		{name: "doubled underscore", give: "2__0", wantErr: true},
		{name: "leading underscore", give: "_1", wantErr: true},
		{name: "trailing underscore", give: "1_", wantErr: true},
		{name: "a unit", give: "0.5f", wantErr: true},
		{name: "words", give: "often", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, name := range []string{
				varDynamoDBErrorProbability, varDynamoDBReadErrorProbability, varDynamoDBWriteErrorProbability,
			} {
				got := &Settings{}
				err := parseDynamoDB(dynamoLookup(name, tt.give), got)

				if tt.wantErr {
					require.ErrorContains(t, err, name)

					continue
				}

				require.NoError(t, err, name)

				var have float64

				switch name {
				case varDynamoDBErrorProbability:
					have = got.DynamoDBErrorProbability
				case varDynamoDBReadErrorProbability:
					have = got.DynamoDBReadErrorProbability
				default:
					have = got.DynamoDBWriteErrorProbability
				}

				if math.IsNaN(tt.want) {
					require.True(t, math.IsNaN(have), name)
				} else {
					require.InDelta(t, tt.want, have, 0, name)
				}
			}
		})
	}
}

// Test_parseDynamoDB_shareDB: DYNAMODB_SHARE_DB reads as LocalStack's
// config.py:940 does, int(value or 0). The value is not stripped first, so one
// of white space alone is int("  "), which Python refuses.
func Test_parseDynamoDB_shareDB(t *testing.T) {
	tests := []struct {
		name    string
		give    string
		want    int
		wantErr bool
	}{
		{name: "empty is 0", give: "", want: 0},
		{name: "plain", give: "2", want: 2},
		{name: "underscores between digits", give: "1_0", want: 10},
		{name: "sign", give: "+3", want: 3},
		{name: "surrounding white space", give: " 7 ", want: 7},
		{name: "Unicode digits", give: "١٢", want: 12},
		{name: "white space alone", give: "  ", wantErr: true},
		{name: "doubled underscore", give: "3__0", wantErr: true},
		{name: "trailing underscore", give: "1_", wantErr: true},
		{name: "hex", give: "0x10", wantErr: true},
		{name: "a float", give: "1.0", wantErr: true},
		{name: "a word", give: "true", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := &Settings{}
			err := parseDynamoDB(dynamoLookup(varDynamoDBShareDB, tt.give), got)

			if tt.wantErr {
				require.ErrorContains(t, err, varDynamoDBShareDB)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, got.DynamoDBShareDB)
		})
	}
}

// Test_newConfig_dynamoDBValuesAsSet: the binary reads the DYNAMODB_* values
// through Config.build, whose lookup trims every other variable. They reach
// parseDynamoDB as set: DYNAMODB_SHARE_DB of white space alone refuses to
// start, as int("  ") does, and a probability wrapped in U+001C..U+001F reads
// as str.strip() leaves it (SQ-363 findings 4 and 5).
func Test_newConfig_dynamoDBValuesAsSet(t *testing.T) {
	start := func(t *testing.T, name, value string) (*Config, error) {
		t.Helper()
		isolate(t)
		t.Setenv(name, value)

		return newConfig(nil, dockerProbe{fsys: fstest.MapFS{}})
	}

	_, err := start(t, varDynamoDBShareDB, "  ")
	require.ErrorContains(t, err, varDynamoDBShareDB)

	cfg, err := start(t, varDynamoDBShareDB, " 1 ")
	require.NoError(t, err)
	require.Equal(t, 1, cfg.Settings().DynamoDBShareDB)

	cfg, err = start(t, varDynamoDBErrorProbability, "\x1c0.5\x1c")
	require.NoError(t, err)
	require.InDelta(t, 0.5, cfg.Settings().DynamoDBErrorProbability, 0)
}
