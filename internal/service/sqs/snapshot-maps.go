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
//
// A null where a struct belongs is an error. The decoder would make a zero
// struct of it, and a snapshot never has one: restore would take a zero message
// or a queue with no contents for a real one.
var restoreOptions = json.JoinOptions(
	snapshotOptions,
	json.WithUnmarshalers(json.JoinUnmarshalers(
		json.UnmarshalFromFunc(rejectNull[snapshotFile]),
		json.UnmarshalFromFunc(rejectNull[snapshotData]),
		json.UnmarshalFromFunc(rejectNull[stateData]),
		json.UnmarshalFromFunc(rejectNull[queueData]),
		json.UnmarshalFromFunc(rejectNull[contentsData]),
		json.UnmarshalFromFunc(rejectNull[entryData]),
		json.UnmarshalFromFunc(rejectNull[messageData]),
		json.UnmarshalFromFunc(rejectNull[dedupData]),
		json.UnmarshalFromFunc(rejectNull[moveTaskData]),
		json.UnmarshalFromFunc(unmarshalAttributes),
		json.UnmarshalFromFunc(unmarshalStrings),
	)),
)

// rejectNull is the unmarshaler of a struct of a snapshot: it fails on a null,
// and for anything else leaves the decoding to the decoder, which is what
// errors.ErrUnsupported asks for.
func rejectNull[T any](dec *jsontext.Decoder, _ *T) error {
	if dec.PeekKind() == 'n' {
		return errors.New("sqs snapshot has a null where an object belongs")
	}

	return errors.ErrUnsupported
}

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

		if dec.PeekKind() == 'n' {
			return fmt.Errorf("message attribute %q is null", name)
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
