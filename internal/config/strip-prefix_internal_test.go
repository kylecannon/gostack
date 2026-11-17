package config

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func Test_strippedVariables(t *testing.T) {
	tests := []struct {
		name string
		give []string
		want []bareVariable
	}{
		{
			name: "prefix is stripped and the value kept",
			give: []string{"LOCALSTACK_DEBUG=1", "LOCALSTACK_AWS_DEFAULT_REGION=eu-west-1", "PATH=/bin"},
			want: []bareVariable{{name: "DEBUG", value: "1"}, {name: "AWS_DEFAULT_REGION", value: "eu-west-1"}},
		},
		{
			name: "LOCALSTACK_HOST and LOCALSTACK_HOSTNAME are never mapped",
			give: []string{"LOCALSTACK_HOST=a", "LOCALSTACK_HOSTNAME=b", "LOCALSTACK_HOST_X=c"},
			want: nil,
		},
		{
			name: "a digit after the prefix is never mapped",
			give: []string{"LOCALSTACK_1X=a", "LOCALSTACK_X1=b"},
			want: []bareVariable{{name: "X1", value: "b"}},
		},
		{
			name: "a value may hold equals signs and may be empty",
			give: []string{"LOCALSTACK_A=b=c", "LOCALSTACK_B="},
			want: []bareVariable{{name: "A", value: "b=c"}, {name: "B", value: ""}},
		},
		{
			name: "names the shell could not export are skipped",
			give: []string{"LOCALSTACK_=x", "LOCALSTACK_A-B=x", "LOCALSTACK", "LOCALSTACK_NOEQUALS"},
			want: nil,
		},
		{
			name: "only the exact prefix counts",
			give: []string{"GOSTACK_DEBUG=1", "XLOCALSTACK_DEBUG=1", "localstack_debug=1"},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, strippedVariables(tt.give))
		})
	}
}

func Test_exportStripped(t *testing.T) {
	t.Run("sets each bare name and replaces an existing bare value", func(t *testing.T) {
		got := map[string]string{}
		err := exportStripped([]string{"LOCALSTACK_A=1", "A=0", "LOCALSTACK_B=2"}, func(key, value string) error {
			got[key] = value

			return nil
		})
		require.NoError(t, err)
		require.Equal(t, map[string]string{"A": "1", "B": "2"}, got)
	})

	t.Run("a failing setenv stops the export", func(t *testing.T) {
		errBoom := errors.New("boom")
		err := exportStripped([]string{"LOCALSTACK_A=1"}, func(string, string) error { return errBoom })
		require.ErrorIs(t, err, errBoom)
	})
}
