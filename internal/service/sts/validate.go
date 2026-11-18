package sts

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kylecannon/gostack/internal/awserr"
)

// The bounds of the members the botocore sts model constrains. They are the
// min and max of the shapes roleSessionNameType, userNameType,
// roleDurationSecondsType and durationSecondsType.
const (
	minDurationSeconds = 900
	// maxRoleDurationSeconds is the model's ceiling for the role operations. AWS
	// also caps AssumeRole at the role's MaxSessionDuration, which gostack
	// cannot know without IAM.
	maxRoleDurationSeconds  = 43200
	maxTokenDurationSeconds = 129600

	minNameLength        = 2
	maxSessionNameLength = 64
	maxUserNameLength    = 32

	// namePatternText is the pattern the model names for session and user names.
	namePatternText = `[\w+=,.@-]*`
)

// violations collects the constraint failures of one call. AWS reports them
// all in one ValidationError.
type violations []string

// duration checks DurationSeconds against the model's range for the operation.
func (v *violations) duration(value *int32, maxSeconds int32) {
	if value == nil {
		return
	}

	switch {
	case *value < minDurationSeconds:
		v.add(strconv.FormatInt(int64(*value), 10), "durationSeconds",
			"Member must have value greater than or equal to "+strconv.Itoa(minDurationSeconds))
	case *value > maxSeconds:
		v.add(strconv.FormatInt(int64(*value), 10), "durationSeconds",
			"Member must have value less than or equal to "+strconv.FormatInt(int64(maxSeconds), 10))
	default:
	}
}

// name checks a RoleSessionName or a federated user Name against the model's
// length and pattern.
func (v *violations) name(member string, value *string, maxLength int) {
	if value == nil {
		return
	}

	switch length := utf8.RuneCountInString(*value); {
	case length < minNameLength:
		v.add(*value, member, "Member must have length greater than or equal to "+strconv.Itoa(minNameLength))
	case length > maxLength:
		v.add(*value, member, "Member must have length less than or equal to "+strconv.Itoa(maxLength))
	default:
	}

	if !sessionNamePattern.MatchString(*value) {
		v.add(*value, member, "Member must satisfy regular expression pattern: "+namePatternText)
	}
}

func (v *violations) add(value, member, constraint string) {
	*v = append(*v, "Value '"+value+"' at '"+member+"' failed to satisfy constraint: "+constraint)
}

// err returns the ValidationError that reports every violation, or nil.
func (v *violations) err() *awserr.Error {
	switch len(*v) {
	case 0:
		return nil
	case 1:
		return validationError("1 validation error detected: " + (*v)[0])
	default:
		return validationError(strconv.Itoa(len(*v)) + " validation errors detected: " + strings.Join(*v, "; "))
	}
}
