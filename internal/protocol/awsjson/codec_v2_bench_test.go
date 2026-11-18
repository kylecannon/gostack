//go:build go1.27

package awsjson_test

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
)

// benchV2Options make encoding/json/v2 write and read what sonic does: nil
// slices and maps inside a value are null, a repeated member keeps the last
// value, and invalid UTF-8 is accepted (v2 replaces it with U+FFFD where sonic
// copies the bytes through). Member names are case-sensitive and HTML is not
// escaped by default in both.
var benchV2Options = jsonv2.JoinOptions(
	jsonv2.FormatNilSliceAsNull(true),
	jsonv2.FormatNilMapAsNull(true),
	jsontext.AllowDuplicateNames(true),
	jsontext.AllowInvalidUTF8(true),
)

func init() {
	benchCodecs = append(benchCodecs, benchCodec{
		name:      "v2",
		marshal:   func(v any) ([]byte, error) { return jsonv2.Marshal(v, benchV2Options) },
		unmarshal: func(data []byte, v any) error { return jsonv2.Unmarshal(data, v, benchV2Options) },
	})
}
