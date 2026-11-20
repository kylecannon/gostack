package secretsmanager

import (
	"net/http"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/secretsmanager"
	"github.com/kylecannon/gostack/internal/awserr"
)

// Every error Secrets Manager answers is a 400, ResourceNotFoundException
// included: LocalStack sets moto's 404 to 400. errBatchNoValue is the one
// exception, which reaches the client as moto made it.
const (
	msgSecretNotFound = "Secrets Manager can't find the specified secret."
	msgValueNotFound  = "Secrets Manager can't find the specified secret value for "
	msgInvalidName    = "Invalid name. Must be a valid name containing alphanumeric characters, " +
		"or any of the following: -/_+=.@!"
	msgNoToken              = "You must provide a ClientRequestToken value. We recommend a UUID-type value."
	msgTokenLength          = "ClientRequestToken must be 32-64 characters long."
	msgMarkedDelete         = "You can't perform this operation on the secret because it was marked for deletion."
	msgMarkedDeletedAlready = "You tried to perform the operation on a secret that's currently marked deleted."
	// msgUpdateDeleted is the text LocalStack copies from a moto exception,
	// prefix and all, for UpdateSecret and ListSecretVersionIds.
	msgUpdateDeleted = "An error occurred (InvalidRequestException) when calling the UpdateSecret operation: " +
		msgMarkedDelete
	// msgBatchDeleted is moto's own get_secret_value text for a deleted secret,
	// which BatchGetSecretValue reaches without LocalStack's override. The run
	// of spaces is moto's: a backslash continuation inside its string literal.
	msgBatchDeleted = "An error occurred (InvalidRequestException) when calling the GetSecretValue operation: " +
		"You tried to                 perform the operation on a secret that's currently marked deleted."
	msgCrossAccount = "You can't access a secret from a different AWS account if you encrypt the secret " +
		"with the default KMS service key."
	msgNoValue = "You must provide either SecretString or SecretBinary."

	// bypassTag is the tag key with which another LocalStack provider creates a
	// secret whose name would fail the name check. The tag is stored like any
	// other.
	bypassTag = "BYPASS_SECRET_ID_VALIDATION"
)

func errSecretNotFound() *awserr.Error {
	return api.NewResourceNotFoundException(msgSecretNotFound)
}

// errBatchNoValue is what BatchGetSecretValue answers for a secret with no
// version to read: moto's own ResourceNotFoundException, which its batch call
// does not catch as it does SecretNotFoundException, so it fails the whole call
// with moto's 404, which LocalStack does not turn into a 400 here. moto asks
// for the VersionId "AWSCURRENT" because it falls back to that name.
func errBatchNoValue() *awserr.Error {
	err := api.NewResourceNotFoundException(msgValueNotFound + "VersionId: " + stageCurrent)
	err.StatusCode = http.StatusNotFound

	return err
}

func errValidation(msg string) *awserr.Error {
	return &awserr.Error{
		Code: "ValidationException", Message: msg, StatusCode: http.StatusBadRequest, Fault: awserr.FaultSender,
	}
}

// checkSecretID is LocalStack's _raise_if_invalid_secret_id: a SecretId that
// does not start with "arn:" must be a valid name.
func checkSecretID(id string) *awserr.Error {
	if strings.HasPrefix(id, "arn:") {
		return nil
	}

	if !validName(id) {
		return errValidation(msgInvalidName)
	}

	return nil
}

// validName matches ^[A-Za-z0-9/_+=.@-]+\Z.
func validName(name string) bool {
	if name == "" {
		return false
	}

	for i := range len(name) {
		c := name[i]

		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case c == '/', c == '_', c == '+', c == '=', c == '.', c == '@', c == '-':
		default:
			return false
		}
	}

	return true
}

// checkTokenPresent is LocalStack's _raise_if_missing_client_req_token. An
// absent token and a json null are the same to the decoder, and to LocalStack.
func checkTokenPresent(token *string) *awserr.Error {
	if token == nil {
		return api.NewInvalidRequestException(msgNoToken)
	}

	return nil
}

// checkWrite is the first checks of PutSecretValue and UpdateSecret: the
// SecretId, then the token's presence.
func checkWrite(secretID, token *string) *awserr.Error {
	err := checkSecretID(deref(secretID))
	if err != nil {
		return err
	}

	return checkTokenPresent(token)
}

// versionIDFrom is moto's _from_client_request_token: a given token, which must
// be 32 to 64 characters, or a fresh id for an empty one.
func versionIDFrom(token string) (string, *awserr.Error) {
	if token == "" {
		return newVersionID(), nil
	}

	if n := len([]rune(token)); n < 32 || n > 64 {
		return "", api.NewInvalidParameterException(msgTokenLength)
	}

	return token, nil
}

// checkKMSAccess is LocalStack's _raise_if_default_kms_key: a secret under the
// default key cannot be read or written from another account.
func checkKMSAccess(accountID string, secret *Secret) *awserr.Error {
	if secret.kmsKeyID == nil && accountID != secret.account {
		return api.NewInvalidRequestException(msgCrossAccount)
	}

	return nil
}
