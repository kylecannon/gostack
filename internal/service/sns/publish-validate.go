package sns

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/sns"
	"github.com/kylecannon/gostack/internal/awserr"
)

const (
	// maxPublishSize is the most bytes a message and its attributes hold
	// together, LocalStack's MAXIMUM_MESSAGE_LENGTH.
	maxPublishSize = 262144
	// maxAttributeNameSize is the longest message attribute name.
	maxAttributeNameSize = 256

	structureJSON = "json"
	defaultKey    = "default"

	typeString = "String"
	typeNumber = "Number"
	typeBinary = "Binary"
)

// checkAttributes is LocalStack's _validate_message_attributes. LocalStack
// checks the attributes in request order; the request holds them in a map, so
// they are checked here in name order. position is the 1-based batch entry,
// 0 for Publish.
func checkAttributes(attrs map[string]api.MessageAttributeValue, position int) error {
	for _, name := range slices.Sorted(maps.Keys(attrs)) {
		err := checkMessageAttribute(name, attrs[name], position)
		if err != nil {
			return err
		}
	}

	return nil
}

func checkMessageAttribute(name string, attr api.MessageAttributeValue, position int) error {
	if len(name) > maxAttributeNameSize {
		return api.NewInvalidParameterValueException("Length of message attribute name must be less than 256 bytes.")
	}

	err := checkAttributeName(name)
	if err != nil {
		return err
	}

	if attr.DataType == nil {
		at := "messageAttributes." + name + ".member.dataType"
		if position > 0 {
			at = "publishBatchRequestEntries." + strconv.Itoa(position) + ".member." + at
		}

		return &awserr.Error{
			Code: "ValidationError",
			Message: "1 validation error detected: Value null at '" + at +
				"' failed to satisfy constraint: Member must not be null",
			StatusCode: http.StatusBadRequest,
			Fault:      awserr.FaultSender,
		}
	}

	dataType := *attr.DataType
	if !validDataType(dataType) {
		return api.NewInvalidParameterValueException("The message attribute '" + name +
			"' has an invalid message attribute type, the set of supported type prefixes is Binary, Number, and String.")
	}

	empty := api.NewInvalidParameterValueException("The message attribute '" + name +
		"' must contain non-empty message attribute value for message attribute type '" + dataType + "'.")

	if attr.StringValue == nil && attr.BinaryValue == nil {
		return empty
	}

	if strings.HasPrefix(dataType, typeBinary) {
		switch {
		case attr.BinaryValue == nil:
			return mustUse(name, dataType, typeBinary)
		case len(attr.BinaryValue) == 0:
			return empty
		}

		return nil
	}

	switch {
	case attr.StringValue == nil:
		return mustUse(name, dataType, typeString)
	case *attr.StringValue == "":
		return empty
	}

	return nil
}

func mustUse(name, dataType, field string) error {
	return api.NewInvalidParameterValueException("The message attribute '" + name + "' with type '" + dataType +
		"' must use field '" + field + "'.")
}

// validDataType is String, Number or Binary, or one of them with a custom
// suffix after a dot (ATTR_TYPE_REGEX).
func validDataType(dataType string) bool {
	for _, prefix := range []string{typeString, typeNumber, typeBinary} {
		rest, ok := strings.CutPrefix(dataType, prefix)
		if ok && (rest == "" || len(rest) > 1 && rest[0] == '.') {
			return true
		}
	}

	return false
}

// checkAttributeName is LocalStack's _validate_message_attribute_name:
// letters, digits, '_', '-' and '.', with no leading, trailing or doubled dot.
func checkAttributeName(name string) error {
	if name == "" {
		// LocalStack fails on name[0] here; AWS's rule is that a name is 1 to
		// 256 characters.
		return api.NewInvalidParameterValueException("Length of message attribute name must be at least 1 byte.")
	}

	if name[0] == '.' {
		return api.NewInvalidParameterValueException(
			"Invalid message attribute name starting with character '.' was found.")
	}

	if name[len(name)-1] == '.' {
		return api.NewInvalidParameterValueException("Invalid message attribute name ending with character '.' was found.")
	}

	previous := rune(0)

	for _, r := range name {
		if !isAttributeNameRune(r) {
			return api.NewInvalidParameterValueException("Invalid non-alphanumeric character '#x" +
				strings.ToUpper(strconv.FormatInt(int64(r), 16)) +
				"' was found in the message attribute name. Can only include alphanumeric characters, hyphens, " +
				"underscores, or dots.")
		}

		if r == '.' && previous == '.' {
			return api.NewInvalidParameterValueException("Message attribute name can not have successive '.' character.")
		}

		previous = r
	}

	return nil
}

func isAttributeNameRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.'
}

// publishSize is LocalStack's _get_total_publish_size: the message's bytes
// and, for each attribute, its name, data type and value.
func publishSize(message string, attrs map[string]api.MessageAttributeValue) int {
	size := len(message)

	for name, attr := range attrs {
		size += len(name) + len(deref(attr.DataType)) + len(deref(attr.StringValue)) + len(attr.BinaryValue)
	}

	return size
}

// parseStructure is the body of a MessageStructure=json message: its string
// members by protocol, other members dropped as LocalStack drops them. A
// default member is required unless the message goes to a platform endpoint.
func parseStructure(message string) (map[string]string, error) {
	var members map[string]json.RawMessage

	// A JSON value that is not an object fails LocalStack on .items(); it is
	// refused here as a body that does not parse.
	err := json.Unmarshal([]byte(message), &members)
	if err != nil || members == nil {
		return nil, api.NewInvalidParameterException(
			"Invalid parameter: Message Structure - JSON message body failed to parse")
	}

	bodies := make(map[string]string, len(members))

	for key, raw := range members {
		var s string
		if json.Unmarshal(raw, &s) == nil && len(raw) > 0 && raw[0] == '"' {
			bodies[key] = s
		}
	}

	return bodies, nil
}
