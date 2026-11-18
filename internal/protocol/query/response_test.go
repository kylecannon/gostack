package query_test

import (
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/query"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_Writer(t *testing.T) {
	t.Parallel()

	at := &protocol.Time{Time: time.Date(2000, 1, 2, 20, 34, 56, 123000000, time.UTC)}

	tests := []struct {
		name      string
		giveOpts  query.Options
		write     func(*query.Writer)
		wantBytes string
	}{
		{
			name:      "escapes markup and the carriage return",
			write:     func(w *query.Writer) { w.String("M", "a&b<c>d\"e\rf") },
			wantBytes: "<M>a&amp;b&lt;c&gt;d\"e&#xD;f</M>",
		},
		{
			name:      "escapes quotes on request",
			giveOpts:  query.Options{EscapeQuotes: true},
			write:     func(w *query.Writer) { w.String("M", `say "hi"`) },
			wantBytes: "<M>say &quot;hi&quot;</M>",
		},
		{
			name:      "writes numbers and booleans",
			write:     func(w *query.Writer) { w.Int("I", -3); w.Bool("B", false); w.Float("F", 5.5) },
			wantBytes: "<I>-3</I><B>false</B><F>5.5</F>",
		},
		{
			name:      "writes the special floats the way botocore reads them",
			write:     func(w *query.Writer) { w.Float("A", math.NaN()); w.Float("B", math.Inf(1)); w.Float("C", math.Inf(-1)) },
			wantBytes: "<A>NaN</A><B>Infinity</B><C>-Infinity</C>",
		},
		{
			name:      "writes a blob in base64",
			write:     func(w *query.Writer) { w.Blob("D", []byte("value")) },
			wantBytes: "<D>dmFsdWU=</D>",
		},
		{
			name: "writes a timestamp in each format",
			write: func(w *query.Writer) {
				w.Time("A", at, "")
				w.Time("B", at, query.TimeUnix)
				w.Time("C", at, query.TimeRFC822)
			},
			wantBytes: "<A>2000-01-02T20:34:56.123000Z</A><B>946845296.123</B><C>Sun, 02 Jan 2000 20:34:56 GMT</C>",
		},
		{
			name:      "nests elements",
			write:     func(w *query.Writer) { w.Open("O"); w.String("I", "x"); w.Close("O") },
			wantBytes: "<O><I>x</I></O>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w := query.NewWriter(tt.giveOpts)
			tt.write(w)

			assert.Equal(t, tt.wantBytes, string(w.Bytes()))
		})
	}
}

func Test_WriteResponse(t *testing.T) {
	t.Parallel()

	t.Run("wraps the result and adds the metadata", func(t *testing.T) {
		t.Parallel()

		rec := httptest.NewRecorder()
		env := &query.Envelope{
			Action:    "SendMessage",
			Namespace: "http://queue.amazonaws.com/doc/2012-11-05/",
			Result:    "SendMessageResult",
			RequestID: "req-1",
		}
		query.WriteResponse(rec, env, func(w *query.Writer) { w.String("MessageId", "m-1") })

		hdr := map[string][]string(rec.Header())

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, []string{"text/xml"}, hdr["Content-Type"])
		assert.Equal(t, []string{"req-1"}, hdr["x-amzn-RequestId"])
		assert.Equal(t,
			"<?xml version='1.0' encoding='utf-8'?>\n"+
				`<SendMessageResponse xmlns="http://queue.amazonaws.com/doc/2012-11-05/">`+
				"<SendMessageResult><MessageId>m-1</MessageId></SendMessageResult>"+
				"<ResponseMetadata><RequestId>req-1</RequestId></ResponseMetadata>"+
				"</SendMessageResponse>",
			rec.Body.String())
	})

	t.Run("an operation without output writes only the metadata", func(t *testing.T) {
		t.Parallel()

		rec := httptest.NewRecorder()
		query.WriteResponse(rec, &query.Envelope{Action: "PurgeQueue", RequestID: "req-2"}, nil)

		assert.Equal(t,
			"<?xml version='1.0' encoding='utf-8'?>\n"+
				"<PurgeQueueResponse><ResponseMetadata><RequestId>req-2</RequestId></ResponseMetadata></PurgeQueueResponse>",
			rec.Body.String())
	})

	t.Run("an output with no members writes an empty result", func(t *testing.T) {
		t.Parallel()

		rec := httptest.NewRecorder()
		query.WriteResponse(rec, &query.Envelope{Action: "A", Result: "AResult", RequestID: "r"}, nil)

		assert.Contains(t, rec.Body.String(), "<AResult/>")
	})
}

func Test_WriteError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		giveErr    *awserr.Error
		wantStatus int
		wantBody   string
	}{
		{
			name: "a sender fault carries its type and the query code",
			giveErr: &awserr.Error{
				Code: "QueueDoesNotExist", Message: `no "q"`, StatusCode: 400,
				Fault: awserr.FaultSender, QueryCode: "AWS.SimpleQueueService.NonExistentQueue",
			},
			wantStatus: 400,
			wantBody: `<ErrorResponse xmlns="ns"><Error><Code>AWS.SimpleQueueService.NonExistentQueue</Code>` +
				`<Message>no &quot;q&quot;</Message><Type>Sender</Type></Error><RequestId>req-3</RequestId></ErrorResponse>`,
		},
		{
			name:       "a receiver fault has no type, and an empty message is left out",
			giveErr:    &awserr.Error{Code: "InternalError", StatusCode: 500, Fault: awserr.FaultReceiver},
			wantStatus: 500,
			wantBody: `<ErrorResponse xmlns="ns"><Error><Code>InternalError</Code></Error>` +
				`<RequestId>req-3</RequestId></ErrorResponse>`,
		},
		{
			name:       "a zero status is 500",
			giveErr:    &awserr.Error{Code: "Boom", Message: "x"},
			wantStatus: 500,
			wantBody: `<ErrorResponse xmlns="ns"><Error><Code>Boom</Code><Message>x</Message></Error>` +
				`<RequestId>req-3</RequestId></ErrorResponse>`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			query.WriteError(rec, &query.Envelope{Namespace: "ns", RequestID: "req-3", EscapeQuotes: true}, tt.giveErr)

			hdr := map[string][]string(rec.Header())

			require.Equal(t, tt.wantStatus, rec.Code)
			assert.Equal(t, []string{"text/xml"}, hdr["Content-Type"])
			assert.Equal(t, []string{"req-3"}, hdr["x-amzn-RequestId"])
			assert.Equal(t, "<?xml version='1.0' encoding='utf-8'?>\n"+tt.wantBody, rec.Body.String())
		})
	}
}
