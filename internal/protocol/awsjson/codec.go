package awsjson

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"net/http"

	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
)

// strict are the encoding/json/v2 settings of every encode and decode here.
// Member names are case-sensitive and members a type does not have are ignored,
// which is v2's default and what AWS json needs. The rest:
//   - a nil slice or map inside a value is null, not [] or {};
//   - a repeated member keeps its last value;
//   - invalid UTF-8 is an error, on decode and on encode.
var strict = json.JoinOptions(
	json.FormatNilSliceAsNull(true),
	json.FormatNilMapAsNull(true),
	jsontext.AllowDuplicateNames(true),
)

// lenient adds to strict what AWS json and LocalStack take beyond v2's
// defaults: NaN and the infinities are the strings "NaN", "Infinity" and
// "-Infinity", as AWS json writes them, and a scalar is read as Python reads it
// (lenient.go).
//
// A caller-supplied marshaler or unmarshaler costs a lookup on every value
// encoded or decoded, whatever its type. Each of these takes everything the
// default encoding takes and gives the same result for it, so every encode and
// decode runs with strict first and pays for lenient only when strict refuses.
var lenient = json.JoinOptions(
	strict,
	json.WithMarshalers(json.MarshalToFunc(marshalNonFinite)),
	json.WithUnmarshalers(json.JoinUnmarshalers(
		json.UnmarshalFromFunc(unmarshalNonFinite),
		json.UnmarshalFromFunc(unmarshalInt32),
		json.UnmarshalFromFunc(unmarshalInt64),
		json.UnmarshalFromFunc(unmarshalBool),
		json.UnmarshalFromFunc(unmarshalBlob),
	)),
)

// marshalNonFinite writes NaN and the infinities as their names, and leaves
// every other float to the default encoding.
func marshalNonFinite(enc *jsontext.Encoder, f float64) error {
	var name string

	switch {
	case math.IsNaN(f):
		name = "NaN"
	case math.IsInf(f, 1):
		name = "Infinity"
	case math.IsInf(f, -1):
		name = "-Infinity"
	default:
		return errors.ErrUnsupported
	}

	return enc.WriteToken(jsontext.String(name)) //nolint:wrapcheck // a marshaler returns the encoder's error as is
}

// unmarshalNonFinite reads a float written as a string, and leaves a number to
// the default decoding. A string is read as Python's float() reads it, which is
// what LocalStack does: "NaN", "Infinity" and "-Infinity", but also "5" and
// "1_0".
func unmarshalNonFinite(dec *jsontext.Decoder, f *float64) error {
	if dec.PeekKind() != '"' {
		return errors.ErrUnsupported
	}

	tok, err := dec.ReadToken()
	if err != nil {
		return err //nolint:wrapcheck // an unmarshaler returns the decoder's error as is
	}

	v, ok := protocol.ParsePyFloat(tok.String())
	if !ok {
		return fmt.Errorf("float written as %q", tok.String())
	}

	*f = v

	return nil
}

// Decode reads a json request body into a T. An empty body decodes to the
// zero T, and members the type does not have are ignored. A body that is not
// valid json gives a 400 SerializationException.
func Decode[T any](body []byte) (*T, *awserr.Error) {
	v, _, err := DecodeLenient[T](body)

	return v, err
}

// DecodeLenient is Decode, and also reports whether the body needed the
// Python-style readers of lenient.go: only then can an integer have been
// clamped to the int32 range, so only then does a caller that echoes the
// number sent need the body afterwards.
func DecodeLenient[T any](body []byte) (v *T, usedLenient bool, err *awserr.Error) {
	v = new(T)
	if len(bytes.TrimSpace(body)) == 0 {
		return v, false, nil
	}

	if json.Unmarshal(body, v, strict) == nil {
		return v, false, nil
	}

	// What the strict pass set is dropped: it can have stopped part way.
	v = new(T)
	if json.Unmarshal(body, v, lenient) != nil {
		return nil, true, &awserr.Error{
			Code:       "SerializationException",
			Message:    "Start of structure or map found where not expected.",
			StatusCode: http.StatusBadRequest,
			Fault:      awserr.FaultSender,
		}
	}

	return v, true, nil
}

// Marshal returns v as json with the same settings Encode uses.
func Marshal(v any) ([]byte, error) {
	b, err := marshalStrictFirst(v)
	if err != nil {
		return nil, fmt.Errorf("marshal json: %w", err)
	}

	return b, nil
}

func marshalStrictFirst(v any) ([]byte, error) {
	b, err := json.Marshal(v, strict)
	if err == nil {
		return b, nil
	}

	return json.Marshal(v, lenient) //nolint:wrapcheck // both callers wrap it
}

// Unmarshal reads json into v with the same settings Decode uses. When the
// strict pass refuses the data part way, the lenient pass reads every member
// the strict one set again, so v ends as the lenient pass alone leaves it.
func Unmarshal(data []byte, v any) error {
	if json.Unmarshal(data, v, strict) == nil {
		return nil
	}

	err := json.Unmarshal(data, v, lenient)
	if err != nil {
		return fmt.Errorf("unmarshal json: %w", err)
	}

	return nil
}

// MarshalTo writes v to enc with the same settings Encode uses. Generated
// MarshalJSONTo methods call it, and those return a plain error. It keeps the
// marshalers of the call it runs inside, so a strict pass stays strict.
func MarshalTo(enc *jsontext.Encoder, v any) error {
	err := json.MarshalEncode(enc, v, strict)
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}

	return nil
}

// UnmarshalFrom reads the next json value from dec into v with the same
// settings Decode uses. Generated UnmarshalJSONFrom methods call it, and those
// return a plain error. It keeps the unmarshalers of the call it runs inside,
// so a strict pass stays strict.
func UnmarshalFrom(dec *jsontext.Decoder, v any) error {
	err := json.UnmarshalDecode(dec, v, strict)
	if err != nil {
		return fmt.Errorf("unmarshal json: %w", err)
	}

	return nil
}

// Encode returns v as json.
func Encode(v any) ([]byte, *awserr.Error) {
	b, err := marshalStrictFirst(v)
	if err != nil {
		return nil, awserr.Internal("response could not be encoded: " + err.Error())
	}

	return b, nil
}
