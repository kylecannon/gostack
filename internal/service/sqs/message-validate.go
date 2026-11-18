package sqs

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/awserr"
)

// Limits of a message and of a batch.
const (
	// maxBatchEntries is the largest batch an operation takes.
	maxBatchEntries = 10
	// maxBatchBytes is the largest total size of the bodies and attributes of a
	// batch. It is a constant, not the queue's MaximumMessageSize.
	maxBatchBytes = 1048576
	// maxAttributeName is the length from which a message attribute name or type
	// is too long.
	maxAttributeName = 256
	// maxFIFOIDLength is the length of a MessageGroupId or a
	// MessageDeduplicationId at most.
	maxFIFOIDLength = 128
	// maxBatchIDLength is the length of a batch entry id at most.
	maxBatchIDLength = 80

	typeString = "String"

	messageInvalidContents = "Invalid characters found. Valid unicode characters are " +
		"#x9 | #xA | #xD | #x20 to #xD7FF | #xE000 to #xFFFD | #x10000 to #x10FFFF"
	messageBatchEntryID = "A batch entry id can only contain alphanumeric characters, hyphens and underscores. " +
		"It can be at most 80 letters long."

	// fifoIDPunctuation are the characters other than 0-9 and A-z that a FIFO id
	// takes.
	fifoIDPunctuation = "!\"#$%&'()*+,./:;<=>?@[\\]^_`{|}~-"
)

// sendRequest is what SendMessage and an entry of SendMessageBatch have in
// common.
type sendRequest struct {
	body       *string
	delay      *int32
	attributes map[string]sqsapi.MessageAttributeValue
	system     map[string]sqsapi.MessageSystemAttributeValue
	dedupID    *string
	groupID    *string
}

// validateSend runs LocalStack's checks of a message in its order: size,
// contents, attributes, then the FIFO ids.
func validateSend(req *sendRequest, maxSize int) *awserr.Error {
	body := deref(req.body)
	if body == "" {
		return missingParameter("MessageBody")
	}

	if len(body)+attributesSize(req.attributes) > maxSize {
		return invalidParameterValue("One or more parameters are invalid. Reason: Message must be shorter than " +
			strconv.Itoa(maxSize) + " bytes.")
	}

	err := checkContent(body)
	if err != nil {
		return err
	}

	err = checkAttributes(req.attributes)
	if err != nil {
		return err
	}

	err = checkAttributes(systemAsUser(req.system))
	if err != nil {
		return err
	}

	err = checkFIFOID(req.dedupID, "MessageDeduplicationId")
	if err != nil {
		return err
	}

	return checkFIFOID(req.groupID, "MessageGroupId")
}

// attributesSize is the size a message's attributes count for against the
// limit: the names and every value, in bytes.
func attributesSize(attrs map[string]sqsapi.MessageAttributeValue) int {
	size := 0

	for name, value := range attrs {
		size += len(name) + len(deref(value.DataType)) + len(deref(value.StringValue)) + len(value.BinaryValue)
	}

	return size
}

// checkContent accepts the characters that XML 1.0 allows in text. Bytes that
// are not UTF-8 are refused: ranging over them reads each one as U+FFFD, which
// the XML rules allow, and a message stored with them cannot be received again.
func checkContent(text string) *awserr.Error {
	if !utf8.ValidString(text) {
		return sqsapi.NewInvalidMessageContents(messageInvalidContents)
	}

	for _, r := range text {
		valid := r == '\t' || r == '\n' || r == '\r' || (r >= 0x20 && r <= 0xD7FF) ||
			(r >= 0xE000 && r <= 0xFFFD) || (r >= 0x10000 && r <= unicode.MaxRune)
		if !valid {
			return sqsapi.NewInvalidMessageContents(messageInvalidContents)
		}
	}

	return nil
}

// checkAttributes validates the names and types of message attributes in name
// order, and the value of a "String" one.
func checkAttributes(attrs map[string]sqsapi.MessageAttributeValue) *awserr.Error {
	for _, name := range slices.Sorted(maps.Keys(attrs)) {
		err := checkAttributeName(name)
		if err != nil {
			return err
		}

		err = checkAttributeType(name, attrs[name])
		if err != nil {
			return err
		}
	}

	return nil
}

func checkAttributeName(name string) *awserr.Error {
	if utf8.RuneCountInString(name) >= maxAttributeName {
		return invalidParameterValue("Message (user) attribute names must be shorter than 256 Bytes")
	}

	lower := strings.ToLower(name)

	for _, r := range lower {
		if !isAttributeNameRune(r) {
			return invalidParameterValue("Message (user) attributes name can only contain upper and lower score " +
				"characters, digits, periods, hyphens and underscores. ")
		}
	}

	reserved := strings.HasPrefix(lower, "aws.") || strings.HasPrefix(lower, "amazon.") ||
		strings.HasPrefix(lower, ".") || strings.HasSuffix(lower, ".")
	if reserved {
		return invalidParameterValue("You can't use message attribute names beginning with 'AWS.' or 'Amazon.'. " +
			"These strings are reserved for internal use. Additionally, they cannot start or end with '.'.")
	}

	return nil
}

// isAttributeNameRune accepts ASCII letters and digits, "_", ".", "-" and, as
// LocalStack does, the Latin letters with diacritics.
func isAttributeNameRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
		r == '_' || r == '.' || r == '-' || (r >= 0x00C0 && r <= 0x017F)
}

func checkAttributeType(name string, value sqsapi.MessageAttributeValue) *awserr.Error {
	dataType := deref(value.DataType)
	if dataType == "" {
		return invalidParameterValue("Missing required parameter DataType")
	}

	known := strings.HasPrefix(dataType, typeString) || strings.HasPrefix(dataType, "Number") ||
		strings.HasPrefix(dataType, "Binary")
	if !known {
		return invalidParameterValue("Type for parameter MessageAttributes.Attribute_name.DataType must be prefixed" +
			`with "String", "Binary", or "Number", but was: ` + dataType)
	}

	if utf8.RuneCountInString(dataType) >= maxAttributeName {
		return invalidParameterValue("Message (user) attribute types must be shorter than 256 Bytes")
	}

	if dataType != typeString {
		return nil
	}

	text := deref(value.StringValue)
	if text == "" {
		return invalidParameterValue("Message (user) attribute '" + name +
			"' must contain a non-empty value of type 'String'.")
	}

	if checkContent(text) != nil {
		return invalidParameterValue(messageInvalidContents)
	}

	return nil
}

// systemAsUser views system attributes as user ones, which have the same shape
// and are checked by the same rules.
func systemAsUser(attrs map[string]sqsapi.MessageSystemAttributeValue) map[string]sqsapi.MessageAttributeValue {
	if attrs == nil {
		return nil
	}

	converted := make(map[string]sqsapi.MessageAttributeValue, len(attrs))
	for name, value := range attrs {
		converted[name] = sqsapi.MessageAttributeValue(value)
	}

	return converted
}

// checkFIFOID validates a MessageGroupId or a MessageDeduplicationId when one is
// given, whatever the type of the queue.
func checkFIFOID(id *string, parameter string) *awserr.Error {
	if id == nil {
		return nil
	}

	valid := *id != "" && utf8.RuneCountInString(*id) <= maxFIFOIDLength
	for _, r := range *id {
		valid = valid && ((r >= '0' && r <= '9') || (r >= 'A' && r <= 'z') || strings.ContainsRune(fifoIDPunctuation, r))
	}

	if !valid {
		return invalidParameterValue("Value " + *id + " for parameter " + parameter + " is invalid. Reason: " +
			parameter + " can only include alphanumeric and punctuation characters. 1 to 128 in length.")
	}

	return nil
}

// validBatchID reports whether id is a batch entry id: 1 to 80 letters, digits,
// underscores and hyphens.
func validBatchID(id string) bool {
	if id == "" || utf8.RuneCountInString(id) > maxBatchIDLength {
		return false
	}

	for _, r := range id {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' {
			return false
		}
	}

	return true
}

// batchSizeError is LocalStack's check of the batch limit.
func batchTooLong(size int) *awserr.Error {
	return sqsapi.NewBatchRequestTooLong("Batch requests cannot be longer than " + strconv.Itoa(maxBatchBytes) +
		" bytes. You have sent " + strconv.Itoa(size) + " bytes.")
}

// checkBatch is LocalStack's _assert_batch for a batch of ids: the batch is not
// empty or too long, and every id is valid and unique. limit is the most entries
// the batch can have.
func checkBatch(ids []string, limit int) *awserr.Error {
	if len(ids) == 0 {
		return sqsapi.NewEmptyBatchRequest("")
	}

	if len(ids) > limit {
		return sqsapi.NewTooManyEntriesInBatchRequest("Maximum number of entries per request are " +
			strconv.Itoa(limit) + ". You have sent " + strconv.Itoa(len(ids)) + ".")
	}

	seen := make(map[string]struct{}, len(ids))

	for _, id := range ids {
		if !validBatchID(id) {
			return sqsapi.NewInvalidBatchEntryId(messageBatchEntryID)
		}

		if _, dup := seen[id]; dup {
			return sqsapi.NewBatchEntryIdsNotDistinct("")
		}

		seen[id] = struct{}{}
	}

	return nil
}
