package kms

import (
	"crypto/rand"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/stretchr/testify/require"
)

// enumShape is a botocore enum shape.
type enumShape struct {
	Enum []string `json:"enum"`
}

// botocoreModel is the part of a botocore service model the test reads.
type botocoreModel struct {
	Shapes map[string]enumShape `json:"shapes"`
}

// modelKeySpecs is the KeySpec enum of the vendored botocore model.
func modelKeySpecs(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile("../../../specs/botocore/kms/2014-11-01/service-2.json")
	require.NoError(t, err)

	var model botocoreModel

	require.NoError(t, json.Unmarshal(raw, &model))
	require.NotEmpty(t, model.Shapes["KeySpec"].Enum)

	return model.Shapes["KeySpec"].Enum
}

// codeUnsupported is the code of the error for a spec no key is made of.
const codeUnsupported = "UnsupportedOperationException"

// Test_newMaterial_everySpec calls newMaterial with each name of the API's
// KeySpec enum, read from the vendored botocore model, and a few names that are
// not in it. A spec it can make gets its material; any other is an error. None
// panics: ECC_SECG_P256K1, which checkSpec accepts, has no standard-library
// curve, and the next callers (GenerateDataKeyPair, ReplicateKey) do not run
// checkSpec first.
func Test_newMaterial_everySpec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		giveSpec string
		wantCode string // of the error, or "" when there is material
		wantHTTP int
	}{
		{giveSpec: "SYMMETRIC_DEFAULT"},
		{giveSpec: "HMAC_224"},
		{giveSpec: "HMAC_256"},
		{giveSpec: "HMAC_384"},
		{giveSpec: "HMAC_512"},
		{giveSpec: "RSA_2048"},
		{giveSpec: "RSA_3072"},
		{giveSpec: "RSA_4096"},
		{giveSpec: "ECC_NIST_P256"},
		{giveSpec: "ECC_NIST_P384"},
		{giveSpec: "ECC_NIST_P521"},
		{giveSpec: "ECC_SECG_P256K1", wantCode: "InternalFailure", wantHTTP: http.StatusNotImplemented},
		{giveSpec: "SM2", wantCode: codeUnsupported, wantHTTP: http.StatusBadRequest},
		{giveSpec: "ML_DSA_44", wantCode: codeUnsupported, wantHTTP: http.StatusBadRequest},
		{giveSpec: "ML_DSA_65", wantCode: codeUnsupported, wantHTTP: http.StatusBadRequest},
		{giveSpec: "ML_DSA_87", wantCode: codeUnsupported, wantHTTP: http.StatusBadRequest},
		{giveSpec: "ECC_NIST_EDWARDS25519", wantCode: codeUnsupported, wantHTTP: http.StatusBadRequest},
		{giveSpec: "RSA_1024", wantCode: codeUnsupported, wantHTTP: http.StatusBadRequest},
		{giveSpec: "ECC_BOGUS", wantCode: codeUnsupported, wantHTTP: http.StatusBadRequest},
		{giveSpec: "", wantCode: codeUnsupported, wantHTTP: http.StatusBadRequest},
	}

	// Every name of the model's enum has a row, so a spec a regeneration adds
	// fails here until someone says what newMaterial does with it.
	listed := make([]string, 0, len(tests))
	for _, tt := range tests {
		listed = append(listed, tt.giveSpec)
	}

	for _, spec := range modelKeySpecs(t) {
		require.Contains(t, listed, spec)
	}

	for _, tt := range tests {
		t.Run(tt.giveSpec, func(t *testing.T) {
			t.Parallel()

			m, err := newMaterial(tt.giveSpec, nil, rand.Reader)

			if tt.wantCode != "" {
				var awsErr *awserr.Error

				require.ErrorAs(t, err, &awsErr)
				require.Equal(t, tt.wantCode, awsErr.Code)
				require.Equal(t, tt.wantHTTP, awsErr.StatusCode)

				return
			}

			require.NoError(t, err)
			require.NotEmpty(t, m.symmetric)

			asymmetric := strings.HasPrefix(tt.giveSpec, "RSA") || strings.HasPrefix(tt.giveSpec, "ECC")
			require.Equal(t, asymmetric, len(m.private) > 0 && len(m.public) > 0)

			if hmac, ok := hmacLens[tt.giveSpec]; ok {
				require.GreaterOrEqual(t, len(m.symmetric), hmac[0])
				require.LessOrEqual(t, len(m.symmetric), hmac[1])
			}
		})
	}
}

// Test_newMaterial_custom holds the material a request supplies to the checks
// that CreateKey's tests reach through the wire, on the function itself.
func Test_newMaterial_custom(t *testing.T) {
	t.Run("an empty slice is no material", func(t *testing.T) {
		m, err := newMaterial(specSymmetric, []byte{}, rand.Reader)
		require.NoError(t, err)
		require.Len(t, m.symmetric, symmetricLen)
	})

	t.Run("an odd length is refused for a symmetric key", func(t *testing.T) {
		_, err := newMaterial(specSymmetric, make([]byte, 20), rand.Reader)

		var awsErr *awserr.Error

		require.ErrorAs(t, err, &awsErr)
		require.Equal(t, "ValidationException", awsErr.Code)
	})
}
