package sts_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/api/sts"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type provider struct {
	sts.Unimplemented

	assumeRole func(context.Context, *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error)
}

func (p *provider) AssumeRole(ctx context.Context, in *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
	return p.assumeRole(ctx, in)
}

func call(t *testing.T, serve func(sts.Handler, http.ResponseWriter, *request.Context), h sts.Handler,
	op string, kind protocol.Kind, body string,
) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	serve(h, rec, &request.Context{
		Service: "sts", Operation: op, Protocol: kind, AccountID: "000000000000",
		Region: "us-east-1", RequestID: "req-1", Request: req,
	})

	return rec
}

func Test_Metadata(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "sts", sts.Metadata.Name)
	assert.Equal(t, "sts", sts.Metadata.SigningName)
	assert.Empty(t, sts.Metadata.TargetPrefix)
	assert.Equal(t, "2011-06-15", sts.Metadata.APIVersion)
	assert.Equal(t, []protocol.Kind{protocol.KindQuery}, sts.Metadata.Protocols)
	assert.Len(t, sts.Operations, 11)
}

func Test_Serve_AssumeRole(t *testing.T) {
	t.Parallel()

	var got *sts.AssumeRoleInput

	expires := &protocol.Time{Time: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)}
	h := &provider{assumeRole: func(_ context.Context, in *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
		got = in

		return &sts.AssumeRoleOutput{
			Credentials: &sts.Credentials{
				AccessKeyId:     new("ASIAEXAMPLE"),
				SecretAccessKey: new(`s"3cr3t`),
				SessionToken:    new("token"),
				Expiration:      expires,
			},
			AssumedRoleUser:  &sts.AssumedRoleUser{Arn: new("arn:aws:sts::1:assumed-role/r/s"), AssumedRoleId: new("ID:s")},
			PackedPolicySize: new(int32(7)),
		}, nil
	}}

	vals := url.Values{
		"Action":                     {"AssumeRole"},
		"Version":                    {"2011-06-15"},
		"RoleArn":                    {"arn:aws:iam::1:role/r"},
		"RoleSessionName":            {"s"},
		"DurationSeconds":            {"900"},
		"PolicyArns.member.1.arn":    {"arn:aws:iam::aws:policy/A"},
		"PolicyArns.member.2.arn":    {"arn:aws:iam::aws:policy/B"},
		"Tags.member.1.Key":          {"env"},
		"Tags.member.1.Value":        {"dev"},
		"TransitiveTagKeys.member.1": {"env"},
	}

	rec := call(t, sts.Serve, h, "AssumeRole", protocol.KindQuery, vals.Encode())

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "arn:aws:iam::1:role/r", *got.RoleArn)
	assert.Equal(t, int32(900), *got.DurationSeconds)
	require.Len(t, got.PolicyArns, 2)
	assert.Equal(t, "arn:aws:iam::aws:policy/B", *got.PolicyArns[1].Arn)
	assert.Equal(t, "dev", *got.Tags[0].Value)
	assert.Equal(t, []string{"env"}, got.TransitiveTagKeys)

	body := rec.Body.String()
	assert.Contains(t, body, `<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult>`)
	assert.Contains(t, body, "<Expiration>2030-01-02T03:04:05Z</Expiration>")
	assert.Contains(t, body, `<SecretAccessKey>s"3cr3t</SecretAccessKey>`, "only SQS escapes quotes")
	assert.Contains(t, body, "<PackedPolicySize>7</PackedPolicySize>")
	assert.Equal(t, []string{"text/xml"}, map[string][]string(rec.Header())["Content-Type"])
}

func Test_Serve_Errors(t *testing.T) {
	t.Parallel()

	t.Run("an error takes its code and status from the spec", func(t *testing.T) {
		t.Parallel()

		h := &provider{assumeRole: func(context.Context, *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
			return nil, sts.NewIDPRejectedClaimException("denied")
		}}

		rec := call(t, sts.Serve, h, "AssumeRole", protocol.KindQuery, "Action=AssumeRole")

		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.Contains(t, rec.Body.String(), "<Code>IDPRejectedClaim</Code>")
		assert.Contains(t, rec.Body.String(), "<Message>denied</Message>")
		assert.Contains(t, rec.Body.String(), "<Type>Sender</Type>")
	})

	t.Run("a json call is refused, the service has no json protocol", func(t *testing.T) {
		t.Parallel()

		rec := call(t, sts.Serve, &provider{}, "AssumeRole", protocol.KindJSON, "{}")

		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("a call with no Action is a 400 MissingAction", func(t *testing.T) {
		t.Parallel()

		rec := call(t, sts.Serve, &provider{}, "", protocol.KindQuery, "Version=2011-06-15")

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "<Code>MissingAction</Code>")
		assert.Contains(t, rec.Body.String(), "<Type>Sender</Type>")
	})

	t.Run("an unknown Action stays InvalidAction", func(t *testing.T) {
		t.Parallel()

		rec := call(t, sts.Serve, &provider{}, "FooBar", protocol.KindQuery, "Action=FooBar")

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "<Code>InvalidAction</Code>")
	})

	t.Run("unimplemented operations answer 501", func(t *testing.T) {
		t.Parallel()

		rec := call(t, sts.Serve, &sts.Unimplemented{}, "GetCallerIdentity", protocol.KindQuery, "Action=GetCallerIdentity")

		assert.Equal(t, http.StatusNotImplemented, rec.Code)
	})
}

func Test_ServeValidated(t *testing.T) {
	t.Parallel()

	rec := call(t, sts.ServeValidated, &provider{}, "AssumeRole", protocol.KindQuery, "Action=AssumeRole&RoleArn=a")

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "<Code>ValidationError</Code>")
	assert.Contains(t, rec.Body.String(), "The request must contain the parameter RoleSessionName.")
}
