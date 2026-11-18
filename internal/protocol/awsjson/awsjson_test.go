package awsjson_test

import (
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const typeJSON11 = "application/x-amz-json-1.1"

type sample struct {
	Name    *string           `json:"Name,omitzero"`
	Count   *int32            `json:"count,omitzero"`
	Tags    []string          `json:"Tags,omitzero"`
	Attrs   map[string]string `json:"Attrs,omitzero"`
	Payload []byte            `json:"Payload,omitzero"`
	At      *protocol.Time    `json:"At,omitzero"`
}

func Test_Decode(t *testing.T) {
	t.Parallel()

	t.Run("reads members by their exact name", func(t *testing.T) {
		t.Parallel()

		got, err := awsjson.Decode[sample]([]byte(`{"Name":"a","name":"b","count":3,"At":1700000000.5}`))
		require.Nil(t, err)
		require.NotNil(t, got.Name)
		assert.Equal(t, "a", *got.Name, "a differently cased key is not the member")
		assert.Equal(t, int32(3), *got.Count)
		assert.Equal(t, time.Unix(1700000000, 500_000_000).UTC(), got.At.UTC())
	})

	t.Run("an empty body is the zero value", func(t *testing.T) {
		t.Parallel()

		got, err := awsjson.Decode[sample](nil)
		require.Nil(t, err)
		assert.Equal(t, &sample{}, got)
	})

	t.Run("keeps an empty list apart from a missing one", func(t *testing.T) {
		t.Parallel()

		got, err := awsjson.Decode[sample]([]byte(`{"Tags":[]}`))
		require.Nil(t, err)
		assert.NotNil(t, got.Tags)
		assert.Empty(t, got.Tags)
		assert.Nil(t, got.Attrs)
	})

	t.Run("rejects malformed json with a 400", func(t *testing.T) {
		t.Parallel()

		got, err := awsjson.Decode[sample]([]byte(`{"Name":`))
		require.NotNil(t, err)
		assert.Nil(t, got)
		assert.Equal(t, "SerializationException", err.Code)
		assert.Equal(t, http.StatusBadRequest, err.StatusCode)
	})

	t.Run("rejects invalid UTF-8 with a 400", func(t *testing.T) {
		t.Parallel()

		// A raw 0xFF byte inside a string, and a lone surrogate escape.
		for _, body := range []string{"{\"Name\":\"a\xffb\"}", `{"Name":"\ud800"}`} {
			got, err := awsjson.Decode[sample]([]byte(body))
			require.NotNil(t, err, body)
			assert.Nil(t, got, body)
			assert.Equal(t, "SerializationException", err.Code, body)
			assert.Equal(t, http.StatusBadRequest, err.StatusCode, body)
		}
	})

	t.Run("a repeated member keeps the last value and unknown members are ignored", func(t *testing.T) {
		t.Parallel()

		got, err := awsjson.Decode[sample]([]byte(`{"Name":"a","Other":{"x":[1]},"Name":"b"}`))
		require.Nil(t, err)
		require.NotNil(t, got.Name)
		assert.Equal(t, "b", *got.Name)
	})
}

func Test_Encode(t *testing.T) {
	t.Parallel()

	name := "n"
	zero := int32(0)
	at := &protocol.Time{Time: time.Unix(1700000000, 0)}

	t.Run("omits nil members and writes empty collections", func(t *testing.T) {
		t.Parallel()

		got, err := awsjson.Encode(&sample{Tags: []string{}, Attrs: map[string]string{}, Payload: []byte{}})
		require.Nil(t, err)
		assert.JSONEq(t, `{"Tags":[],"Attrs":{},"Payload":""}`, string(got))
	})

	t.Run("writes a nil slice or map inside a value as null", func(t *testing.T) {
		t.Parallel()

		type wrapper struct {
			Lists [][]string          `json:"Lists"`
			Maps  []map[string]string `json:"Maps"`
		}

		got, err := awsjson.Encode(&wrapper{Lists: [][]string{nil}, Maps: []map[string]string{nil}})
		require.Nil(t, err)
		assert.JSONEq(t, `{"Lists":[null],"Maps":[null]}`, string(got))
	})

	t.Run("writes zero values that are set", func(t *testing.T) {
		t.Parallel()

		got, err := awsjson.Encode(&sample{Name: &name, Count: &zero, Payload: []byte("hi"), At: at})
		require.Nil(t, err)
		assert.JSONEq(t, `{"Name":"n","count":0,"Payload":"aGk=","At":1700000000}`, string(got))
	})
}

func Test_WriteResponse(t *testing.T) {
	t.Parallel()

	name := "n"

	t.Run("sets the version content type and request id", func(t *testing.T) {
		t.Parallel()

		rec := httptest.NewRecorder()
		awsjson.WriteResponse(rec, "req-1", "1.0", &sample{Name: &name})

		hdr := map[string][]string(rec.Header())

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, []string{"application/x-amz-json-1.0"}, hdr["Content-Type"])
		assert.Equal(t, []string{"req-1"}, hdr["x-amzn-RequestId"])
		assert.JSONEq(t, `{"Name":"n"}`, rec.Body.String())
	})

	t.Run("writes an empty object for no output", func(t *testing.T) {
		t.Parallel()

		rec := httptest.NewRecorder()
		awsjson.WriteResponse(rec, "req-1", "1.1", nil)

		assert.Equal(t, []string{typeJSON11}, map[string][]string(rec.Header())["Content-Type"])
		assert.JSONEq(t, `{}`, rec.Body.String())
	})
}

func Test_WriteError(t *testing.T) {
	t.Parallel()

	queueGone := &awserr.Error{
		Code:       "QueueDoesNotExist",
		Message:    "The specified queue does not exist.",
		StatusCode: http.StatusForbidden,
		Fault:      awserr.FaultSender,
		QueryCode:  "AWS.SimpleQueueService.NonExistentQueue",
	}

	tests := []struct {
		name       string
		giveErr    *awserr.Error
		giveHeader map[string]string
		giveOpts   awsjson.ErrorOptions
		wantStatus int
		wantQuery  []string
		wantBody   string
	}{
		{
			name:       "plain error keeps its status and sends no query header",
			giveErr:    &awserr.Error{Code: "InternalError", Message: "boom", StatusCode: http.StatusInternalServerError},
			wantStatus: http.StatusInternalServerError,
			wantBody:   `{"__type":"InternalError","message":"boom"}`,
		},
		{
			name:       "query compatible error clamps a 4xx to 400 and names the query code",
			giveErr:    queueGone,
			giveOpts:   awsjson.ErrorOptions{QueryCompatible: true},
			wantStatus: http.StatusBadRequest,
			wantQuery:  []string{"AWS.SimpleQueueService.NonExistentQueue;Sender"},
			wantBody:   `{"__type":"QueueDoesNotExist","message":"The specified queue does not exist."}`,
		},
		{
			name:       "query mode keeps the declared status",
			giveErr:    queueGone,
			giveHeader: map[string]string{"x-amzn-query-mode": "true"},
			giveOpts:   awsjson.ErrorOptions{QueryCompatible: true},
			wantStatus: http.StatusForbidden,
			wantQuery:  []string{"AWS.SimpleQueueService.NonExistentQueue;Sender"},
			wantBody:   `{"__type":"QueueDoesNotExist","message":"The specified queue does not exist."}`,
		},
		{
			name:       "an empty message is left out and a zero status becomes 500",
			giveErr:    &awserr.Error{Code: "Boom"},
			wantStatus: http.StatusInternalServerError,
			wantBody:   `{"__type":"Boom"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil)
			for k, v := range tt.giveHeader {
				req.Header.Set(k, v)
			}

			rec := httptest.NewRecorder()
			awsjson.WriteError(rec, req, "req-2", tt.giveErr, tt.giveOpts)

			hdr := map[string][]string(rec.Header())

			assert.Equal(t, tt.wantStatus, rec.Code)
			assert.Equal(t, []string{"application/x-amz-json-1.0"}, hdr["Content-Type"])
			assert.Equal(t, []string{"req-2"}, hdr["x-amzn-RequestId"])
			assert.Equal(t, []string{tt.giveErr.Code}, hdr["X-Amzn-Errortype"])
			assert.Equal(t, tt.wantQuery, hdr["x-amzn-query-error"])
			assert.JSONEq(t, tt.wantBody, rec.Body.String())
		})
	}

	t.Run("the content type is the one in the options", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil)
		rec := httptest.NewRecorder()
		opts := awsjson.ErrorOptions{ContentType: typeJSON11}
		awsjson.WriteError(rec, req, "req-3", queueGone, opts)

		assert.Equal(t, []string{typeJSON11}, map[string][]string(rec.Header())["Content-Type"])
	})
}

// stamped is the shape the generator writes for a struct with a timestamp that
// is not in epoch seconds: it swaps the field for a format type while
// encoding/json/v2 encodes or decodes the struct, using an embedded alias and a
// field that shadows the embedded one.
//
//nolint:recvcheck // the generator writes a value receiver for the marshaler, as it must for a value in a map
type stamped struct {
	Created *protocol.Time `json:"Created,omitzero"`
	Name    *string        `json:"Name,omitzero"`
}

func (s stamped) MarshalJSONTo(enc *jsontext.Encoder) error {
	type plain stamped

	err := awsjson.MarshalTo(enc, struct {
		*plain

		Created *awsjson.ISO8601 `json:"Created,omitzero"`
	}{
		plain:   (*plain)(&s),
		Created: (*awsjson.ISO8601)(s.Created),
	})
	if err != nil {
		return fmt.Errorf("marshal stamped: %w", err)
	}

	return nil
}

func (s *stamped) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	type plain stamped

	aux := struct {
		*plain

		Created *awsjson.ISO8601 `json:"Created,omitzero"`
	}{plain: (*plain)(s)}

	err := awsjson.UnmarshalFrom(dec, &aux)
	if err != nil {
		return fmt.Errorf("unmarshal stamped: %w", err)
	}

	if aux.Created != nil {
		s.Created = (*protocol.Time)(aux.Created)
	}

	return nil
}

func Test_TimeFormats(t *testing.T) {
	t.Parallel()

	name := "n"
	at := &protocol.Time{Time: time.Date(2000, 1, 2, 20, 34, 56, 0, time.UTC)}

	t.Run("a struct swaps its timestamp for the ISO 8601 type", func(t *testing.T) {
		t.Parallel()

		got, err := awsjson.Encode(&stamped{Created: at, Name: &name})
		require.Nil(t, err)
		assert.JSONEq(t, `{"Created":"2000-01-02T20:34:56Z","Name":"n"}`, string(got))
	})

	t.Run("a struct reads the ISO 8601 form back, offsets included", func(t *testing.T) {
		t.Parallel()

		got, err := awsjson.Decode[stamped]([]byte(`{"Created":"2019-12-16T22:48:18-01:00","Name":"n"}`))
		require.Nil(t, err)
		require.NotNil(t, got.Created)
		assert.Equal(t, int64(1576540098), got.Created.Unix())
		assert.Equal(t, "n", *got.Name)
	})

	t.Run("a missing timestamp stays nil", func(t *testing.T) {
		t.Parallel()

		got, err := awsjson.Decode[stamped]([]byte(`{"Name":"n"}`))
		require.Nil(t, err)
		assert.Nil(t, got.Created)

		out, encErr := awsjson.Encode(got)
		require.Nil(t, encErr)
		assert.JSONEq(t, `{"Name":"n"}`, string(out))
	})

	t.Run("the HTTP date type round-trips", func(t *testing.T) {
		t.Parallel()

		type holder struct {
			At *awsjson.RFC822 `json:"At,omitzero"`
		}

		got, err := awsjson.Decode[holder]([]byte(`{"At":"Sun, 02 Jan 2000 20:34:56 GMT"}`))
		require.Nil(t, err)
		assert.Equal(t, int64(946845296), got.At.Unix())

		out, encErr := awsjson.Encode(got)
		require.Nil(t, encErr)
		assert.JSONEq(t, `{"At":"Sun, 02 Jan 2000 20:34:56 GMT"}`, string(out))
	})
}
