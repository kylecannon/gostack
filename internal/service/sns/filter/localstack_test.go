package filter_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/kylecannon/gostack/internal/service/sns/filter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// localstackCase is one row of testdata/localstack-filter.json, which
// testdata/localstack-filter.py wrote by running LocalStack's own filter.py.
type localstackCase struct {
	Policy  string          `json:"policy"`
	Scope   filter.Scope    `json:"scope"`
	Message json.RawMessage `json:"message"`
	OK      *bool           `json:"ok"`
	Error   string          `json:"error"`
	Crash   string          `json:"crash"`
	Match   *bool           `json:"match"`
}

func loadLocalStack(t *testing.T) (validate, match []localstackCase) {
	t.Helper()
	data, err := os.ReadFile("testdata/localstack-filter.json")
	require.NoError(t, err)
	var cases struct {
		Validate []localstackCase `json:"validate"`
		Match    []localstackCase `json:"match"`
	}
	require.NoError(t, json.Unmarshal(data, &cases))
	return cases.Validate, cases.Match
}

// crashDecisions answers the policies LocalStack's validator fails on with an
// exception other than InvalidParameterException.
var crashDecisions = map[string]string{
	`5`:                                notObject,
	`[1]`:                              notObject,
	`"abc"`:                            notObject,
	`true`:                             notObject,
	`{"key": [{"numeric": 5}]}`:        numericNotArray,
	`{"key": [{"numeric": "abc"}]}`:    numericNotArray,
	`{"key": [{"numeric": {"a": 1}}]}`: numericNotArray,
	`{"$or": ["a", "b"]}`:              orNotObject,
	`{"$or": [{"a": ["x"]}, ["b"]]}`:   orNotObject,
}

const (
	notObject       = "FilterPolicy: Filter is not an object"
	numericNotArray = "FilterPolicy: Value of numeric must be an array."
	orNotObject     = "FilterPolicy: Only JSON object is allowed in array of $or relationship."
)

func Test_Parse_LocalStack(t *testing.T) {
	validate, _ := loadLocalStack(t)
	for _, tt := range validate {
		t.Run(tt.Policy, func(t *testing.T) {
			p, err := filter.Parse(tt.Policy, tt.Scope)
			want := tt.Error
			if tt.Crash != "" {
				want = crashDecisions[tt.Policy]
				require.NotEmpty(t, want, "LocalStack fails with %s and no decision is recorded", tt.Crash)
			}
			if want != "" {
				var ferr *filter.Error
				require.ErrorAs(t, err, &ferr, "want refusal %q", want)
				assert.Equal(t, want, ferr.Reason)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, *tt.OK, !p.Empty())
		})
	}
}

func Test_Policy_Match_LocalStack(t *testing.T) {
	_, match := loadLocalStack(t)
	for _, tt := range match {
		if tt.Crash != "" {
			continue // Test_Policy_Match_Decisions covers what LocalStack fails on
		}
		t.Run(tt.Policy+"/"+string(tt.Message), func(t *testing.T) {
			p, err := filter.Parse(tt.Policy, tt.Scope)
			require.NoError(t, err)
			want := *tt.Match
			if w, ok := awsOverrides[overrideKey{tt.Policy, string(tt.Message)}]; ok {
				want = w
			}
			assert.Equal(t, want, matchMessage(t, p, tt.Scope, tt.Message))
		})
	}
}

type overrideKey struct{ policy, message string }

// awsOverrides are the rows where gostack follows AWS's numeric-value-matching
// document and LocalStack does not: an anything-but number compares with a
// Number attribute by value.
var awsOverrides = map[overrideKey]bool{
	{`{"price": [{"anything-but": [100, 500]}]}`, `{"price":{"DataType":"Number","StringValue":"100"}}`}: false,
}

func matchMessage(t *testing.T, p *filter.Policy, scope filter.Scope, message json.RawMessage) bool {
	t.Helper()
	if scope == filter.ScopeMessageBody {
		var body string
		require.NoError(t, json.Unmarshal(message, &body))
		return p.MatchBody(filter.ParseBody(body))
	}
	var attrs map[string]filter.Attribute
	require.NoError(t, json.Unmarshal(message, &attrs))
	return p.MatchAttributes(attrs)
}
