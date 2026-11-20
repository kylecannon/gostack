package secretsmanager

import (
	"context"
	"time"

	api "github.com/kylecannon/gostack/internal/api/secretsmanager"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/request"
)

const (
	// maxLambdaARNLength is the longest RotationLambdaARN LocalStack accepts.
	maxLambdaARNLength = 2048
	minRotationDays    = 1
	maxRotationDays    = 1000

	msgNoLambda = "No Lambda rotation function ARN is associated with this secret."
	// msgCancelNoLambda is moto's text, the missing space before "and" too.
	msgCancelNoLambda = "You tried to enable rotation on a secret that doesn't already have a Lambda function " +
		"ARN configuredand you didn't include such an ARN as a parameter in this call."
	msgLambdaMissing   = "Lambda does not exist or could not be accessed"
	msgRotationPending = "Previous rotation request is still in progress."
	msgRotateDeleted   = "An error occurred (InvalidRequestException) when calling the RotateSecret operation: " +
		"You tried to perform the operation on a secret that's currently marked deleted."
)

// rotationRequest is what RotateSecret resolved before calling Lambda: the
// function, and the rotation period in days, 0 for none.
type rotationRequest struct {
	lambdaARN string
	days      int64
}

// RotateSecret is LocalStack's own rotate_secret, which replaces moto's. Every
// check, the call that confirms the function exists included, runs before the
// secret changes. Invoking the function's four steps belongs to Lambda; until
// Lambda is a service GetFunction fails, so the call ends with
// ResourceNotFoundException and the secret as it was.
func (p *Provider) RotateSecret(ctx context.Context, in *api.RotateSecretInput) (*api.RotateSecretOutput, error) {
	// The aws.validated snapshot answers InvalidParameterException where
	// LocalStack's provider raises InvalidRequestException.
	if in.ClientRequestToken == nil {
		return nil, api.NewInvalidParameterException(msgNoToken)
	}

	rc, err := callOf(ctx)
	if err != nil {
		return nil, err
	}

	secret, err := p.find(rc, in.SecretId)
	if err != nil {
		return nil, err
	}

	if secret.primary != nil {
		return nil, errReplica()
	}

	req, err := resolveRotation(secret, in)
	if err != nil {
		return nil, err
	}

	err = p.checkFunction(ctx, secret, req.lambdaARN)
	if err != nil {
		return nil, err
	}

	var out *api.RotateSecretOutput

	lockErr := locked(secret, func(secret *Secret) error {
		if secret.deleted() {
			return api.NewInvalidRequestException(msgRotateDeleted)
		}

		versionID, err := secret.startRotation(p.now(), req, deref(in.ClientRequestToken), in.RotateImmediately)
		if err != nil {
			return err
		}

		out = &api.RotateSecretOutput{ARN: new(secret.arn), Name: new(secret.name)}
		if versionID != "" {
			out.VersionId = new(versionID)
		}

		return nil
	})

	return out, lockErr
}

// locked runs fn with the secret's lock held, unless a forced delete or a
// reset took the secret away since it was found.
func locked(secret *Secret, fn func(*Secret) error) error {
	secret.mu.Lock()
	defer secret.mu.Unlock()

	if secret.gone {
		return errSecretNotFound()
	}

	return fn(secret)
}

// resolveRotation is RotateSecret's checks of the secret and the request: not
// deleted, a function from the request or stored, its length, and the period.
func resolveRotation(secret *Secret, in *api.RotateSecretInput) (rotationRequest, *awserr.Error) {
	secret.mu.Lock()
	defer secret.mu.Unlock()

	if secret.gone {
		return rotationRequest{}, errSecretNotFound()
	}

	if secret.deleted() {
		return rotationRequest{}, api.NewInvalidRequestException(msgRotateDeleted)
	}

	req := rotationRequest{lambdaARN: deref(in.RotationLambdaARN)}
	if req.lambdaARN == "" {
		req.lambdaARN = secret.rotation.lambdaARN
	}

	if req.lambdaARN == "" {
		return rotationRequest{}, api.NewInvalidRequestException(msgNoLambda)
	}

	if len(req.lambdaARN) > maxLambdaARNLength {
		return rotationRequest{}, api.NewInvalidParameterException("RotationLambdaARN must <= 2048 characters long.")
	}

	if secret.rotation.afterDays != nil {
		req.days = *secret.rotation.afterDays
	}

	if in.RotationRules != nil && in.RotationRules.AutomaticallyAfterDays != nil {
		req.days = *in.RotationRules.AutomaticallyAfterDays
		if req.days < minRotationDays || req.days > maxRotationDays {
			return rotationRequest{}, api.NewInvalidParameterException(
				"RotationRules.AutomaticallyAfterDays must be within 1-1000.")
		}
	}

	return req, nil
}

// checkFunction is LocalStack's GetFunction call: any failure means the
// function cannot be used. The secret's lock is not held, so a lookup that
// calls back into this service cannot deadlock.
func (p *Provider) checkFunction(ctx context.Context, secret *Secret, lambdaARN string) *awserr.Error {
	if !p.functionExists(ctx, secret.account, secret.region, lambdaARN) {
		return api.NewResourceNotFoundException(msgLambdaMissing)
	}

	return nil
}

// noFunction is the function lookup while Lambda is not served: no function
// exists.
func noFunction(context.Context, string, string, string) bool {
	return false
}

// startRotation stores the rotation configuration and, when the rotation runs
// now, adds the AWSPENDING version the function is to fill in. It returns the
// version id the response names. The caller holds the secret's lock.
func (s *Secret) startRotation(
	now time.Time, req rotationRequest, token string, immediately *bool,
) (string, *awserr.Error) {
	pending := s.versionWithStage(stagePending)
	if pending != nil && pending.hasStage(stageCurrent) {
		pending = nil
	}

	runNow := immediately == nil || *immediately

	// LocalStack asks the function to finish a pending version and reports
	// its failure as this error, after storing the configuration. Without
	// Lambda the call always fails, so it is refused before anything changes.
	if runNow && pending != nil {
		return "", api.NewInvalidRequestException(msgRotationPending)
	}

	// LocalStack fails on an unbound variable when the rotation does not run
	// now; the current version is the one it leaves in place.
	versionID := s.current

	if runNow {
		id, err := versionIDFrom(token)
		if err != nil {
			return "", err
		}

		versionID = id
	}

	s.rotation.lambdaARN = req.lambdaARN
	s.rotation.afterDays = new(req.days)

	if req.days > 0 {
		s.rotation.next = now.Truncate(time.Second).Add(time.Duration(req.days) * day)
		s.rotation.enabled = true
		s.rotation.requested = true
	}

	if runNow {
		// LocalStack adds the version with a placeholder value, which moto
		// needs to store it, and then removes the value so the function makes
		// a new one.
		v := &version{id: versionID, stages: []string{stagePending}, str: new("")}
		s.write(now, v)
		v.str = nil
	}

	s.rotation.lastRotation = now.Truncate(time.Second)

	return versionID, nil
}

// CancelRotateSecret turns automatic rotation off. A secret that never had a
// rotation function answers moto's error.
func (p *Provider) CancelRotateSecret(
	ctx context.Context, in *api.CancelRotateSecretInput,
) (*api.CancelRotateSecretOutput, error) {
	var out *api.CancelRotateSecretOutput

	err := p.withSecret(ctx, in.SecretId, func(_ *request.Context, secret *Secret) error {
		if secret.deleted() {
			return api.NewInvalidRequestException(msgMarkedDeletedAlready)
		}

		if secret.rotation.lambdaARN == "" {
			return api.NewInvalidRequestException(msgCancelNoLambda)
		}

		secret.rotation.enabled = false
		out = &api.CancelRotateSecretOutput{ARN: new(secret.arn), Name: new(secret.name)}

		if secret.current != "" {
			out.VersionId = new(secret.current)
		}

		return nil
	})

	return out, err
}
