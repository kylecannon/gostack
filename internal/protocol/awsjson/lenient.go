package awsjson

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/kylecannon/gostack/internal/protocol"
)

// LocalStack reads a json scalar with Python: an integer member is int() of the
// value, so "5", " 5", "1_0", 5.7 and true all give a number; a blob is
// b64decode() of the string. These unmarshalers take the same values. What
// Python refuses (and LocalStack answers with a 500) stays an error here, and an
// error is a 400 SerializationException.
//
// A json null leaves the value as it was, which is how the default decoding
// treats it.

var errNotScalar = errors.New("json value is not a scalar of the member's type")

// readInteger reads one json value as Python's int() would. skip is true for a
// null.
func readInteger(dec *jsontext.Decoder) (n int64, skip bool, err error) {
	switch dec.PeekKind() {
	case 'n':
		_, err = dec.ReadToken()

		return 0, true, wrap(err)
	case 't', 'f':
		tok, err := dec.ReadToken()
		if err != nil {
			return 0, false, wrap(err)
		}

		if tok.Bool() {
			return 1, false, nil
		}

		return 0, false, nil
	case '"':
		tok, err := dec.ReadToken()
		if err != nil {
			return 0, false, wrap(err)
		}

		v, ok := protocol.ParsePyInt(tok.String())
		if !ok {
			return 0, false, fmt.Errorf("%q is not an integer", tok.String())
		}

		return v, false, nil
	case '0':
		val, err := dec.ReadValue()
		if err != nil {
			return 0, false, wrap(err)
		}

		return numberToInt(string(val))
	default:
		return 0, false, errNotScalar
	}
}

func wrap(err error) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("read json value: %w", err)
}

// numberToInt is int(float(text)) for a json number, truncating toward zero,
// and the integer itself when there is no fraction or exponent.
func numberToInt(text string) (int64, bool, error) {
	if !strings.ContainsAny(text, ".eE") {
		v, _ := protocol.ParsePyInt(text)

		return v, false, nil
	}

	f, ok := protocol.ParsePyFloat(text)
	if !ok || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false, fmt.Errorf("%s is not a finite number", text)
	}

	f = math.Trunc(f)

	switch {
	case f >= math.MaxInt64:
		return math.MaxInt64, false, nil
	case f <= math.MinInt64:
		return math.MinInt64, false, nil
	default:
		return int64(f), false, nil
	}
}

func unmarshalInt64(dec *jsontext.Decoder, v *int64) error {
	n, skip, err := readInteger(dec)
	if err != nil || skip {
		return err
	}

	*v = n

	return nil
}

// unmarshalInt32 clamps a value past the int32 range to the nearest bound, so a
// range check on the member still refuses it.
func unmarshalInt32(dec *jsontext.Decoder, v *int32) error {
	n, skip, err := readInteger(dec)
	if err != nil || skip {
		return err
	}

	*v = int32(max(math.MinInt32, min(math.MaxInt32, n)))

	return nil
}

// unmarshalBool takes true and false, and the strings "true" and "false" in any
// case, as the query protocol does.
func unmarshalBool(dec *jsontext.Decoder, v *bool) error {
	switch dec.PeekKind() {
	case 'n':
		_, err := dec.ReadToken()

		return wrap(err)
	case 't', 'f':
		tok, err := dec.ReadToken()
		if err != nil {
			return wrap(err)
		}

		*v = tok.Bool()

		return nil
	case '"':
		tok, err := dec.ReadToken()
		if err != nil {
			return wrap(err)
		}

		switch strings.ToLower(tok.String()) {
		case "true":
			*v = true
		case "false":
			*v = false
		default:
			return fmt.Errorf("%q is not a boolean", tok.String())
		}

		return nil
	default:
		return errNotScalar
	}
}

// unmarshalBlob reads a blob the way Python's b64decode does.
func unmarshalBlob(dec *jsontext.Decoder, v *[]byte) error {
	switch dec.PeekKind() {
	case 'n':
		_, err := dec.ReadToken()
		*v = nil

		return wrap(err)
	case '"':
		tok, err := dec.ReadToken()
		if err != nil {
			return wrap(err)
		}

		b, ok := protocol.ParsePyBase64(tok.String())
		if !ok {
			return fmt.Errorf("%q is not base64", tok.String())
		}

		*v = b

		return nil
	default:
		return errNotScalar
	}
}
