package sqs

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
)

// The maps of a message in a snapshot decode themselves. The generic map
// decoder of encoding/json/v2 builds each map through reflection, which
// allocates more than the finished map holds, and a restore reads one or two of
// these maps for every message of every queue.
var restoreOptions = json.JoinOptions(
	snapshotOptions,
	json.WithUnmarshalers(json.JoinUnmarshalers(
		json.UnmarshalFromFunc(unmarshalAttributes),
		json.UnmarshalFromFunc(unmarshalStrings),
	)),
)

// unmarshalAttributes reads the message attributes of a message.
func unmarshalAttributes(dec *jsontext.Decoder, m *map[string]sqsapi.MessageAttributeValue) error {
	isNull, err := openObject(dec)
	if err != nil || isNull {
		*m = nil

		return err
	}

	out := make(map[string]sqsapi.MessageAttributeValue)

	for dec.PeekKind() != '}' {
		name, err := readString(dec)
		if err != nil {
			return err
		}

		var value sqsapi.MessageAttributeValue

		err = json.UnmarshalDecode(dec, &value)
		if err != nil {
			return fmt.Errorf("message attribute %q: %w", name, err)
		}

		out[name] = value
	}

	*m = out

	return closeObject(dec)
}

// unmarshalStrings reads a map of strings: the system attributes of a message,
// the attributes and the tags of a queue.
func unmarshalStrings(dec *jsontext.Decoder, m *map[string]string) error {
	isNull, err := openObject(dec)
	if err != nil || isNull {
		*m = nil

		return err
	}

	out := make(map[string]string)

	for dec.PeekKind() != '}' {
		name, err := readString(dec)
		if err != nil {
			return err
		}

		value, err := readString(dec)
		if err != nil {
			return err
		}

		out[name] = value
	}

	*m = out

	return closeObject(dec)
}

// openObject reads the start of an object. It reports whether the value was null
// instead, which a map takes as no map.
func openObject(dec *jsontext.Decoder) (isNull bool, _ error) {
	tok, err := dec.ReadToken()
	if err != nil {
		return false, fmt.Errorf("read object: %w", err)
	}

	switch tok.Kind() {
	case 'n':
		return true, nil
	case '{':
		return false, nil
	default:
		return false, fmt.Errorf("read object: found %s", tok.Kind())
	}
}

// closeObject reads the end of an object, which PeekKind said is next.
func closeObject(dec *jsontext.Decoder) error {
	_, err := dec.ReadToken()
	if err != nil {
		return fmt.Errorf("read end of object: %w", err)
	}

	return nil
}

// readString reads the next token, which has to be a string.
func readString(dec *jsontext.Decoder) (string, error) {
	tok, err := dec.ReadToken()
	if err != nil {
		return "", fmt.Errorf("read string: %w", err)
	}

	if tok.Kind() != '"' {
		return "", errors.New("read string: found " + tok.Kind().String())
	}

	return tok.String(), nil
}
