package kms_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	fieldGrantID           = "GrantId"
	fieldGrantToken        = "GrantToken"
	fieldGranteePrincipal  = "GranteePrincipal"
	fieldRetiringPrincipal = "RetiringPrincipal"
	fieldOperations        = "Operations"
	fieldGrants            = "Grants"
	fieldNextMarker        = "NextMarker"
	fieldName              = "Name"
	fieldLimit             = "Limit"
	opDecrypt              = "Decrypt"
	opEncrypt              = "Encrypt"
	grantName              = "TestGrantName"
	invalidKey             = "invalid"

	codeInvalidGrantID    = "InvalidGrantIdException"
	codeInvalidGrantToken = "InvalidGrantTokenException"
	codeGrantValidation   = "ValidationError"

	userARN       = "arn:aws:iam::000000000000:user/test"
	otherAccount  = "111122223333"
	uuidPattern   = `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[0-9a-f]{4}-[0-9a-f]{12}$`
	enumMessageOp = "Member must satisfy constraint: [Member must satisfy enum value set: [Decrypt, Encrypt, " +
		"GenerateDataKey, GenerateDataKeyWithoutPlaintext, ReEncryptFrom, ReEncryptTo, Sign, Verify, GetPublicKey, " +
		"CreateGrant, RetireGrant, DescribeKey, GenerateDataKeyPair, GenerateDataKeyPairWithoutPlaintext, " +
		"GenerateMac, VerifyMac, DeriveSharedSecret]]"
)

// callAs is call from the account whose id is the access key, which the
// gateway takes as that account.
func (s *stack) callAsOther(operation string, input map[string]any) reply {
	s.t.Helper()

	body, err := json.Marshal(input)
	require.NoError(s.t, err)

	req := httptest.NewRequestWithContext(s.t.Context(), http.MethodPost, "/", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", jsonType)
	req.Header.Set("X-Amz-Target", "TrentService."+operation)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+otherAccount+"/20130524/"+usEast1+
		"/kms/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc")

	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)

	resp := rec.Result()

	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(s.t, err)

	out := reply{status: resp.StatusCode, header: resp.Header, body: map[string]any{}}
	if len(raw) > 0 {
		require.NoError(s.t, json.Unmarshal(raw, &out.body), string(raw))
	}

	return out
}

// grant makes a grant on key for userARN with extra members and returns its id
// and token.
func (s *stack) grant(key string, extra map[string]any) (string, string) {
	s.t.Helper()

	input := map[string]any{
		fieldKeyID: key, fieldGranteePrincipal: userARN, fieldOperations: []string{opDecrypt, opEncrypt},
	}
	maps.Copy(input, extra)

	out := s.ok("CreateGrant", input)

	id, ok := out[fieldGrantID].(string)
	require.True(s.t, ok)

	token, ok := out[fieldGrantToken].(string)
	require.True(s.t, ok)

	return id, token
}

// grantsOf lists the grants of key.
func (s *stack) grantsOf(key string) []map[string]any {
	s.t.Helper()

	return entries(s.t, s.ok("ListGrants", map[string]any{fieldKeyID: key}))
}

func entries(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()

	list, ok := body[fieldGrants].([]any)
	require.True(t, ok, "%v", body)

	out := make([]map[string]any, 0, len(list))

	for _, e := range list {
		m, ok := e.(map[string]any)
		require.True(t, ok)

		out = append(out, m)
	}

	return out
}

func ids(grants []map[string]any) []string {
	out := make([]string, 0, len(grants))
	for _, g := range grants {
		id, _ := g[fieldGrantID].(string)
		out = append(out, id)
	}

	return out
}

func Test_CreateGrant(t *testing.T) {
	s := newStack(t)
	meta := s.createKey(nil)
	key := keyID(t, meta)

	id, token := s.grant(key, map[string]any{
		fieldName: "one", fieldRetiringPrincipal: "arn:aws:iam::000000000000:role/retire",
		"Constraints": map[string]any{"EncryptionContextSubset": map[string]string{"a": "b"}},
	})

	require.Regexp(t, uuidPattern, id)

	// The token is the account, the region and a UUID in base64, as LocalStack makes it.
	decoded, err := base64.StdEncoding.DecodeString(token)
	require.NoError(t, err)
	require.Regexp(t, `^000000000000:us-east-1:[0-9a-f-]{36}$`, string(decoded))

	grants := s.grantsOf(key)
	require.Len(t, grants, 1)

	g := grants[0]
	require.Equal(t, id, g[fieldGrantID])
	require.Equal(t, arn(t, meta), g[fieldKeyID])
	require.Equal(t, "one", g[fieldName])
	require.Equal(t, userARN, g[fieldGranteePrincipal])
	require.Equal(t, "arn:aws:iam::000000000000:role/retire", g[fieldRetiringPrincipal])
	require.Equal(t, "arn:aws:iam::000000000000:root", g["IssuingAccount"])
	require.Equal(t, []any{opDecrypt, opEncrypt}, g[fieldOperations])
	require.Equal(t, map[string]any{"EncryptionContextSubset": map[string]any{"a": "b"}}, g["Constraints"])
	require.Contains(t, g, "CreationDate")

	t.Run("a grant without a name or retiring principal", func(t *testing.T) {
		s.grant(key, nil)

		last := s.grantsOf(key)[1]
		require.Empty(t, last[fieldName])
		require.NotContains(t, last, fieldRetiringPrincipal)
		require.NotContains(t, last, "Constraints")
	})

	t.Run("every grant is a new one without a name", func(t *testing.T) {
		before := len(s.grantsOf(key))

		first, _ := s.grant(key, nil)
		second, _ := s.grant(key, nil)

		require.NotEqual(t, first, second)
		require.Len(t, s.grantsOf(key), before+2)
	})

	t.Run("any form of the key id", func(t *testing.T) {
		s.ok("CreateAlias", map[string]any{fieldAliasName: aliasA, fieldTargetKeyID: key})

		for _, name := range []string{arn(t, meta), aliasA} {
			id, _ := s.grant(name, nil)
			require.Contains(t, ids(s.grantsOf(key)), id)
		}
	})

	t.Run("a grant made through the GenerateMac and VerifyMac operations", func(_ *testing.T) {
		s.grant(key, map[string]any{fieldOperations: []string{"GenerateMac", "VerifyMac", "DeriveSharedSecret"}})
	})
}

func Test_CreateGrant_name(t *testing.T) {
	s := newStack(t)
	first := keyID(t, s.createKey(nil))
	second := keyID(t, s.createKey(nil))

	id, token := s.grant(first, map[string]any{fieldName: grantName})
	again, againToken := s.grant(first, map[string]any{fieldName: grantName})

	// The same grant, with a token of its own that names it as well.
	require.Equal(t, id, again)
	require.NotEqual(t, token, againToken)
	require.Len(t, s.grantsOf(first), 1)

	// The name belongs to a key: another key gets a grant of its own.
	other, _ := s.grant(second, map[string]any{fieldName: grantName})
	require.NotEqual(t, id, other)
	require.Len(t, s.grantsOf(second), 1)

	// Retiring by either token removes the grant and the other token with it.
	s.ok("RetireGrant", map[string]any{fieldGrantToken: againToken})
	require.Empty(t, s.grantsOf(first))
	s.fails("RetireGrant", map[string]any{fieldGrantToken: token}, "NotFoundException",
		"Unable to find grant token "+token)
}

func Test_CreateGrant_errors(t *testing.T) {
	s := newStack(t)
	key := keyID(t, s.createKey(nil))

	base := func(extra map[string]any) map[string]any {
		in := map[string]any{fieldKeyID: key, fieldGranteePrincipal: userARN, fieldOperations: []string{opDecrypt}}
		maps.Copy(in, extra)

		return in
	}

	t.Run("a key that is not there", func(_ *testing.T) {
		s.fails("CreateGrant", base(map[string]any{fieldKeyID: invalidKey}), "NotFoundException",
			"Invalid keyId 'invalid'")
	})

	t.Run("no grantee principal or operations", func(_ *testing.T) {
		msg := "Grant ID, key ID and grantee principal must be specified"

		in := base(nil)
		delete(in, fieldGranteePrincipal)
		s.fails("CreateGrant", in, codeGrantValidation, msg)

		in = base(nil)
		delete(in, fieldOperations)
		s.fails("CreateGrant", in, codeGrantValidation, msg)
	})

	t.Run("an operation that cannot be granted", func(t *testing.T) {
		s.fails("CreateGrant", base(map[string]any{fieldOperations: []string{opDecrypt, "CreateKey"}}),
			codeGrantValidation,
			"1 validation error detected: Value '[Decrypt, CreateKey]' at 'operations' failed to satisfy constraint: "+
				enumMessageOp)
		s.fails("CreateGrant", base(map[string]any{fieldOperations: []string{"decrypt"}}),
			codeGrantValidation,
			"1 validation error detected: Value '[decrypt]' at 'operations' failed to satisfy constraint: "+
				enumMessageOp)
		require.Empty(t, s.grantsOf(key))
	})

	t.Run("a disabled key and one pending deletion", func(t *testing.T) {
		disabled := keyID(t, s.createKey(nil))
		s.ok("DisableKey", map[string]any{fieldKeyID: disabled})
		r := s.call(usEast1, "CreateGrant", base(map[string]any{fieldKeyID: disabled}))
		require.Equal(t, "DisabledException", r.errorCode())

		doomed := keyID(t, s.createKey(nil))
		s.ok("ScheduleKeyDeletion", map[string]any{fieldKeyID: doomed})
		require.Equal(t, "KMSInvalidStateException",
			s.call(usEast1, "CreateGrant", base(map[string]any{fieldKeyID: doomed})).errorCode())
	})
}

func Test_ListGrants(t *testing.T) {
	s := newStack(t)
	key := keyID(t, s.createKey(nil))
	other := keyID(t, s.createKey(nil))

	made := make([]string, 0, 5)

	for range 5 {
		id, _ := s.grant(key, nil)
		made = append(made, id)
	}

	s.grant(other, nil)

	t.Run("only the grants of the key, in order", func(t *testing.T) {
		out := s.ok("ListGrants", map[string]any{fieldKeyID: key})
		require.Equal(t, made, ids(entries(t, out)))
		require.Equal(t, false, out["Truncated"])
		require.NotContains(t, out, fieldNextMarker)
	})

	t.Run("pages by grant id", func(t *testing.T) {
		var got []string

		input := map[string]any{fieldKeyID: key, fieldLimit: 2}

		for pages := 0; ; pages++ {
			require.Less(t, pages, 5)

			out := s.ok("ListGrants", input)
			got = append(got, ids(entries(t, out))...)

			next, more := out[fieldNextMarker].(string)
			if !more {
				require.Equal(t, false, out["Truncated"])

				break
			}

			require.Equal(t, true, out["Truncated"])
			require.Equal(t, made[len(got)], next)

			input["Marker"] = next
		}

		require.Equal(t, made, got)
	})

	t.Run("one grant by id", func(t *testing.T) {
		out := s.ok("ListGrants", map[string]any{fieldKeyID: key, fieldGrantID: made[2]})
		require.Equal(t, []string{made[2]}, ids(entries(t, out)))

		s.fails("ListGrants", map[string]any{fieldKeyID: key, fieldGrantID: "nope"}, codeInvalidGrantID, "")
	})

	t.Run("by grantee principal", func(t *testing.T) {
		mine, _ := s.grant(key, map[string]any{fieldGranteePrincipal: "arn:aws:iam::000000000000:user/other"})

		out := s.ok("ListGrants", map[string]any{
			fieldKeyID: key, fieldGranteePrincipal: "arn:aws:iam::000000000000:user/other",
		})
		require.Equal(t, []string{mine}, ids(entries(t, out)))

		out = s.ok("ListGrants", map[string]any{
			fieldKeyID: key, fieldGranteePrincipal: "arn:aws:iam::000000000000:user/none",
		})
		require.Empty(t, entries(t, out))
	})

	t.Run("errors", func(_ *testing.T) {
		s.fails("ListGrants", map[string]any{fieldKeyID: invalidKey}, "NotFoundException", "Invalid keyId 'invalid'")
		s.fails("ListGrants", map[string]any{fieldKeyID: ""}, codeGrantValidation,
			"Required input parameter KeyId not specified")
	})

	t.Run("a key in any state", func(t *testing.T) {
		s.ok("DisableKey", map[string]any{fieldKeyID: key})
		require.Len(t, s.grantsOf(key), 6)

		s.ok("ScheduleKeyDeletion", map[string]any{fieldKeyID: key})
		require.Len(t, s.grantsOf(key), 6)
	})
}

func Test_ListRetirableGrants(t *testing.T) {
	s := newStack(t)
	key := keyID(t, s.createKey(nil))
	other := keyID(t, s.createKey(nil))

	right := "arn:aws:kms:eu-central-1:123456789876:key/198a5a78-52c3-489f-ac70-000000000001"
	wrong := "arn:aws:kms:eu-central-1:123456789876:key/198a5a78-52c3-489f-ac70-000000000002"

	want := make([]string, 0, 3)

	for _, k := range []string{key, other, key} {
		id, _ := s.grant(k, map[string]any{fieldRetiringPrincipal: right})
		want = append(want, id)
	}

	s.grant(key, map[string]any{fieldRetiringPrincipal: wrong})
	s.grant(key, nil)

	t.Run("the grants of a retiring principal across keys", func(t *testing.T) {
		out := s.ok("ListRetirableGrants", map[string]any{fieldRetiringPrincipal: right})
		require.Equal(t, want, ids(entries(t, out)))
	})

	t.Run("pages", func(t *testing.T) {
		first := s.ok("ListRetirableGrants", map[string]any{fieldRetiringPrincipal: right, fieldLimit: 2})
		require.Equal(t, want[:2], ids(entries(t, first)))
		require.Equal(t, want[2], first[fieldNextMarker])

		second := s.ok("ListRetirableGrants", map[string]any{
			fieldRetiringPrincipal: right, fieldLimit: 2, "Marker": first[fieldNextMarker],
		})
		require.Equal(t, want[2:], ids(entries(t, second)))
		require.NotContains(t, second, fieldNextMarker)
	})

	t.Run("a principal nothing names", func(t *testing.T) {
		require.Empty(t, entries(t, s.ok("ListRetirableGrants", map[string]any{fieldRetiringPrincipal: "nobody"})))
	})

	t.Run("no principal", func(_ *testing.T) {
		s.fails("ListRetirableGrants", map[string]any{}, codeGrantValidation,
			"Required input parameter 'RetiringPrincipal' not specified")
	})
}

func Test_RevokeGrant(t *testing.T) {
	s := newStack(t)
	key := keyID(t, s.createKey(nil))
	other := keyID(t, s.createKey(nil))

	id, _ := s.grant(key, nil)
	keep, _ := s.grant(key, nil)

	t.Run("a grant of another key", func(t *testing.T) {
		s.fails("RevokeGrant", map[string]any{fieldKeyID: other, fieldGrantID: id}, codeGrantValidation,
			"Invalid KeyId="+other+" specified for grant "+id)
		require.Len(t, s.grantsOf(key), 2)
	})

	t.Run("a grant that is not there", func(_ *testing.T) {
		s.fails("RevokeGrant", map[string]any{fieldKeyID: key, fieldGrantID: "nope"}, codeInvalidGrantID, "")
		s.fails("RevokeGrant", map[string]any{fieldKeyID: key}, codeInvalidGrantID, "")
		s.fails("RevokeGrant", map[string]any{fieldKeyID: invalidKey, fieldGrantID: id}, "NotFoundException",
			"Invalid keyId 'invalid'")
	})

	t.Run("removes the grant and its tokens", func(t *testing.T) {
		s.ok("RevokeGrant", map[string]any{fieldKeyID: key, fieldGrantID: id})
		require.Equal(t, []string{keep}, ids(s.grantsOf(key)))
		s.fails("RevokeGrant", map[string]any{fieldKeyID: key, fieldGrantID: id}, codeInvalidGrantID, "")
	})

	t.Run("a key in any state", func(t *testing.T) {
		s.ok("DisableKey", map[string]any{fieldKeyID: key})
		s.ok("RevokeGrant", map[string]any{fieldKeyID: key, fieldGrantID: keep})
		require.Empty(t, s.grantsOf(key))
	})
}

func Test_RetireGrant(t *testing.T) {
	s := newStack(t)
	key := keyID(t, s.createKey(nil))
	other := keyID(t, s.createKey(nil))

	t.Run("by token", func(t *testing.T) {
		_, token := s.grant(key, nil)
		keep, _ := s.grant(key, nil)

		s.ok("RetireGrant", map[string]any{fieldGrantToken: token})
		require.Equal(t, []string{keep}, ids(s.grantsOf(key)))

		s.fails("RetireGrant", map[string]any{fieldGrantToken: token}, "NotFoundException",
			"Unable to find grant token "+token)
	})

	t.Run("by grant id and key id", func(t *testing.T) {
		id, _ := s.grant(key, nil)
		before := len(s.grantsOf(key))

		s.ok("RetireGrant", map[string]any{fieldGrantID: id, fieldKeyID: key})
		require.Len(t, s.grantsOf(key), before-1)

		s.fails("RetireGrant", map[string]any{fieldGrantID: id, fieldKeyID: key}, codeInvalidGrantID, "")
	})

	t.Run("by token with the key named", func(_ *testing.T) {
		id, token := s.grant(key, nil)

		s.fails("RetireGrant", map[string]any{fieldGrantToken: token, fieldKeyID: other}, codeGrantValidation,
			"Invalid KeyId="+other+" specified for grant "+id)
		s.ok("RetireGrant", map[string]any{fieldGrantToken: token, fieldKeyID: key})
	})

	t.Run("a grant of another key", func(_ *testing.T) {
		id, _ := s.grant(key, nil)

		s.fails("RetireGrant", map[string]any{fieldGrantID: id, fieldKeyID: other}, codeGrantValidation,
			"Invalid KeyId="+other+" specified for grant "+id)
	})

	t.Run("not enough to name a grant", func(_ *testing.T) {
		msg := "Grant token OR (grant ID, key ID) must be specified"
		s.fails("RetireGrant", map[string]any{}, codeValidation, msg)
		s.fails("RetireGrant", map[string]any{fieldGrantID: "x"}, codeValidation, msg)
		s.fails("RetireGrant", map[string]any{fieldKeyID: key}, codeValidation, msg)
	})

	t.Run("a token that is not one", func(t *testing.T) {
		for _, token := range []string{
			"not base64!",
			base64.StdEncoding.EncodeToString([]byte("one:two")),
			base64.StdEncoding.EncodeToString([]byte("one:two:three:four")),
		} {
			require.Equal(t, codeInvalidGrantToken,
				s.call(usEast1, "RetireGrant", map[string]any{fieldGrantToken: token}).errorCode(), token)
		}

		// Well formed, and nothing is there.
		token := base64.StdEncoding.EncodeToString([]byte("999999999999:eu-west-1:abc"))
		s.fails("RetireGrant", map[string]any{fieldGrantToken: token}, "NotFoundException",
			"Unable to find grant token "+token)
	})
}

// Test_Grants_crossAccount follows test_cross_accounts_access: a caller in
// another account grants, lists, retires and revokes on a key it names by ARN.
// The grants are the caller's, as LocalStack keeps them.
func Test_Grants_crossAccount(t *testing.T) {
	s := newStack(t)
	meta := s.createKey(nil)
	key := arn(t, meta)

	created := s.callAsOther("CreateGrant", map[string]any{
		fieldKeyID: key, fieldGranteePrincipal: userARN, fieldOperations: []string{opDecrypt, opEncrypt},
	})
	require.Equal(t, http.StatusOK, created.status, "%v", created.body)

	token, ok := created.body[fieldGrantToken].(string)
	require.True(t, ok)

	decoded, err := base64.StdEncoding.DecodeString(token)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(decoded), otherAccount+":"+usEast1+":"), string(decoded))

	// The caller's own list has the grant; the key's owner's has none.
	listed := s.callAsOther("ListGrants", map[string]any{fieldKeyID: key})
	require.Len(t, entries(t, listed.body), 1)
	require.Equal(t, key, entries(t, listed.body)[0][fieldKeyID])
	require.Equal(t, "arn:aws:iam::"+otherAccount+":root", entries(t, listed.body)[0]["IssuingAccount"])
	require.Empty(t, s.grantsOf(keyID(t, meta)))

	second := s.callAsOther("CreateGrant", map[string]any{
		fieldKeyID: key, fieldGranteePrincipal: userARN, fieldOperations: []string{"Sign", "Verify"},
	})

	retired := s.callAsOther("RetireGrant", map[string]any{fieldGrantToken: token})
	require.Equal(t, http.StatusOK, retired.status, "%v", retired.body)

	revoked := s.callAsOther("RevokeGrant", map[string]any{
		fieldKeyID: key, fieldGrantID: second.body[fieldGrantID],
	})
	require.Equal(t, http.StatusOK, revoked.status, "%v", revoked.body)

	require.Empty(t, entries(t, s.callAsOther("ListGrants", map[string]any{fieldKeyID: key}).body))

	// A bare key id does not reach the other account's key.
	missing := s.callAsOther("CreateGrant", map[string]any{
		fieldKeyID: keyID(t, meta), fieldGranteePrincipal: userARN, fieldOperations: []string{opDecrypt},
	})
	require.Equal(t, "NotFoundException", missing.errorCode())
}

func Test_Grants_readOnly(t *testing.T) {
	s := newStack(t)
	key := keyID(t, s.createKey(nil))

	// ListGrants and ListRetirableGrants change nothing.
	before := s.changes.Load()
	s.grantsOf(key)
	s.ok("ListRetirableGrants", map[string]any{fieldRetiringPrincipal: "x"})
	require.Equal(t, before, s.changes.Load())
}

// Test_CreateGrant_concurrentName makes many grants with one name at once: one
// grant, and a token for each call that names it.
func Test_CreateGrant_concurrentName(t *testing.T) {
	s := newStack(t)
	key := keyID(t, s.createKey(nil))

	const calls = 16

	var wg sync.WaitGroup

	replies := make([]reply, calls)

	for i := range calls {
		wg.Go(func() {
			replies[i] = s.call(usEast1, "CreateGrant", map[string]any{
				fieldKeyID: key, fieldGranteePrincipal: userARN, fieldOperations: []string{opDecrypt}, fieldName: "same",
			})
		})
	}

	wg.Wait()

	tokens := map[any]bool{}

	for _, r := range replies {
		require.Equal(t, http.StatusOK, r.status, "%v", r.body)
		require.Equal(t, replies[0].body[fieldGrantID], r.body[fieldGrantID])

		tokens[r.body[fieldGrantToken]] = true
	}

	require.Len(t, tokens, calls)
	require.Len(t, s.grantsOf(key), 1)
}
