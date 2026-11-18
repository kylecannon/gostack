package awsjson

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"

	"github.com/kylecannon/gostack/internal/awserr"
)

// options are the encoding/json/v2 settings of every encode and decode here.
// Member names are case-sensitive and members a type does not have are ignored,
// which is v2's default and what AWS json needs. The rest:
//   - a nil slice or map inside a value is null, not [] or {};
//   - a repeated member keeps its last value;
//   - invalid UTF-8 is an error, on decode and on encode.
var options = json.JoinOptions(
	json.FormatNilSliceAsNull(true),
	json.FormatNilMapAsNull(true),
	jsontext.AllowDuplicateNames(true),
)

// Decode reads a json request body into a T. An empty body decodes to the
// zero T, and members the type does not have are ignored. A body that is not
// valid json gives a 400 SerializationException.
func Decode[T any](body []byte) (*T, *awserr.Error) {
	v := new(T)
	if len(bytes.TrimSpace(body)) == 0 {
		return v, nil
	}

	err := json.Unmarshal(body, v, options)
	if err != nil {
		return nil, &awserr.Error{
			Code:       "SerializationException",
			Message:    "Start of structure or map found where not expected.",
			StatusCode: http.StatusBadRequest,
			Fault:      awserr.FaultSender,
		}
	}

	return v, nil
}

// Marshal returns v as json with the same settings Encode uses.
func Marshal(v any) ([]byte, error) {
	b, err := json.Marshal(v, options)
	if err != nil {
		return nil, fmt.Errorf("marshal json: %w", err)
	}

	return b, nil
}

// Unmarshal reads json into v with the same settings Decode uses.
func Unmarshal(data []byte, v any) error {
	err := json.Unmarshal(data, v, options)
	if err != nil {
		return fmt.Errorf("unmarshal json: %w", err)
	}

	return nil
}

// MarshalTo writes v to enc with the same settings Encode uses. Generated
// MarshalJSONTo methods call it, and those return a plain error.
func MarshalTo(enc *jsontext.Encoder, v any) error {
	err := json.MarshalEncode(enc, v, options)
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}

	return nil
}

// UnmarshalFrom reads the next json value from dec into v with the same
// settings Decode uses. Generated UnmarshalJSONFrom methods call it, and those
// return a plain error.
func UnmarshalFrom(dec *jsontext.Decoder, v any) error {
	err := json.UnmarshalDecode(dec, v, options)
	if err != nil {
		return fmt.Errorf("unmarshal json: %w", err)
	}

	return nil
}

// Encode returns v as json.
func Encode(v any) ([]byte, *awserr.Error) {
	b, err := json.Marshal(v, options)
	if err != nil {
		return nil, awserr.Internal("response could not be encoded: " + err.Error())
	}

	return b, nil
}
