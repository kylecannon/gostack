package awsjson

import (
	"bytes"
	"fmt"
	"net/http"

	"github.com/bytedance/sonic"
	"github.com/kylecannon/gostack/internal/awserr"
)

// api is sonic with case-sensitive member names: AWS json members are exact,
// where encoding/json would match them in any case.
var api = sonic.Config{CaseSensitive: true}.Froze()

// Decode reads a json request body into a T. An empty body decodes to the
// zero T, and members the type does not have are ignored. A body that is not
// valid json gives a 400 SerializationException.
func Decode[T any](body []byte) (*T, *awserr.Error) {
	v := new(T)
	if len(bytes.TrimSpace(body)) == 0 {
		return v, nil
	}

	err := api.Unmarshal(body, v)
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

// Marshal returns v as json with the same settings Encode uses. Generated
// marshalers call it, and those return a plain error.
func Marshal(v any) ([]byte, error) {
	b, err := api.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal json: %w", err)
	}

	return b, nil
}

// Unmarshal reads json into v with the same settings Decode uses. Generated
// marshalers call it, and those return a plain error.
func Unmarshal(data []byte, v any) error {
	err := api.Unmarshal(data, v)
	if err != nil {
		return fmt.Errorf("unmarshal json: %w", err)
	}

	return nil
}

// Encode returns v as json.
func Encode(v any) ([]byte, *awserr.Error) {
	b, err := api.Marshal(v)
	if err != nil {
		return nil, awserr.Internal("response could not be encoded: " + err.Error())
	}

	return b, nil
}
