package sts

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	api "github.com/kylecannon/gostack/internal/api/sts"
	"github.com/kylecannon/gostack/internal/awserr"
)

// The bounds of the members the botocore sts model constrains. They are the
// min and max of the shapes roleSessionNameType, userNameType,
// roleDurationSecondsType, durationSecondsType, arnType, accessKeyIdType,
// tagKeyType, tagValueType, tagListType and tagKeyListType.
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

	minRoleARNLength = 20
	maxRoleARNLength = 2048

	minAccessKeyLength = 16
	maxAccessKeyLength = 128

	maxTags           = 50
	minTagKeyLength   = 1
	maxTagKeyLength   = 128
	maxTagValueLength = 256

	// namePatternText is the pattern the model names for session and user names.
	namePatternText = `[\w+=,.@-]*`
	// accessKeyPatternText, tagKeyPatternText and tagValuePatternText are the
	// patterns the model names for an access key ID, a tag key and a tag value.
	accessKeyPatternText = `[\w]*`
	tagKeyPatternText    = `[\p{L}\p{Z}\p{N}_.:/=+\-@]+`
	tagValuePatternText  = `[\p{L}\p{Z}\p{N}_.:/=+\-@]*`
)

var (
	// accessKeyPattern is the model's pattern with AWS's \w, the ASCII class.
	accessKeyPattern = regexp.MustCompile(`^[\w]*$`)
	tagKeyPattern    = regexp.MustCompile(`^[\p{L}\p{Z}\p{N}_.:/=+\-@]+$`)
	tagValuePattern  = regexp.MustCompile(`^[\p{L}\p{Z}\p{N}_.:/=+\-@]*$`)
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

	v.length(member, *value, minNameLength, maxLength)
	v.pattern(member, *value, sessionNamePattern, namePatternText)
}

// roleARN checks a RoleArn against the model's length. The model's pattern
// admits every printable character, so only the length can fail.
func (v *violations) roleARN(value *string) {
	if value == nil {
		return
	}

	v.length("roleArn", *value, minRoleARNLength, maxRoleARNLength)
}

// accessKeyID checks the AccessKeyId of GetAccessKeyInfo against the model's
// length and pattern.
func (v *violations) accessKeyID(value *string) {
	if value == nil {
		return
	}

	v.length("accessKeyId", *value, minAccessKeyLength, maxAccessKeyLength)
	v.pattern("accessKeyId", *value, accessKeyPattern, accessKeyPatternText)
}

// tags checks the session tags of AssumeRole: at most 50, each key 1 to 128
// characters and each value 0 to 256, both matching the model's pattern.
func (v *violations) tags(tags []api.Tag) {
	if len(tags) > maxTags {
		keys := make([]string, len(tags))
		for i, tag := range tags {
			keys[i] = deref(tag.Key)
		}

		v.tooMany(keys, "tags")
	}

	for i, tag := range tags {
		prefix := "tags." + strconv.Itoa(i+1) + ".member."

		v.tagPart(prefix+"key", tag.Key, minTagKeyLength, maxTagKeyLength, tagKeyPattern, tagKeyPatternText)
		v.tagPart(prefix+"value", tag.Value, 0, maxTagValueLength, tagValuePattern, tagValuePatternText)
	}
}

// transitiveKeys checks TransitiveTagKeys: at most 50 keys, each a valid tag
// key.
func (v *violations) transitiveKeys(keys []string) {
	if len(keys) > maxTags {
		v.tooMany(keys, "transitiveTagKeys")
	}

	for i, key := range keys {
		member := "transitiveTagKeys." + strconv.Itoa(i+1) + ".member"

		v.length(member, key, minTagKeyLength, maxTagKeyLength)
		v.pattern(member, key, tagKeyPattern, tagKeyPatternText)
	}
}

// tagPart checks one member of a tag; a member that is absent is a null.
func (v *violations) tagPart(
	member string, value *string, minLength, maxLength int, pattern *regexp.Regexp, patternText string,
) {
	if value == nil {
		*v = append(*v, "Value null at '"+member+"' failed to satisfy constraint: Member must not be null")

		return
	}

	v.length(member, *value, minLength, maxLength)
	v.pattern(member, *value, pattern, patternText)
}

// tooMany reports a list over the model's 50 entries. AWS words the bound of a
// list as a length.
func (v *violations) tooMany(keys []string, member string) {
	v.add("["+strings.Join(keys, ", ")+"]", member,
		"Member must have length less than or equal to "+strconv.Itoa(maxTags))
}

// length checks the number of characters of value.
func (v *violations) length(member, value string, minLength, maxLength int) {
	switch length := utf8.RuneCountInString(value); {
	case length < minLength:
		v.add(value, member, "Member must have length greater than or equal to "+strconv.Itoa(minLength))
	case length > maxLength:
		v.add(value, member, "Member must have length less than or equal to "+strconv.Itoa(maxLength))
	default:
	}
}

func (v *violations) pattern(member, value string, pattern *regexp.Regexp, text string) {
	if !pattern.MatchString(value) {
		v.add(value, member, "Member must satisfy regular expression pattern: "+text)
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

func deref(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}
