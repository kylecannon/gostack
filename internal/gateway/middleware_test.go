package gateway_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const internalParams = `{"source_arn": "arn:aws:sns:us-east-1:000000000000:t"}`

type hookKey struct{}

func Test_Gateway_middlewareOrder(t *testing.T) {
	var events []string

	reg := sqsRegistration(func(w http.ResponseWriter, _ *request.Context) {
		events = append(events, "serve")

		_, _ = io.WriteString(w, "ok")
	})
	reg.ClaimHost = func(string) bool {
		events = append(events, eventDataPlane)

		return false
	}

	g := newGateway(t, newSettings(), []service.Registration{reg}, func(r chi.Router) {
		r.Get("/_localstack/health", func(w http.ResponseWriter, _ *http.Request) {
			events = append(events, "health")

			_, _ = io.WriteString(w, "up")
		})
	})
	g.SetPreprocessHook(func(r *http.Request) *http.Request {
		events = append(events, eventPreprocess)

		return r
	})

	build := func(t *testing.T, headers map[string]string) *http.Request {
		t.Helper()

		r := sendMessage(t)
		for name, value := range headers {
			r.Header.Set(name, value)
		}

		return r
	}

	t.Run("a normal call runs the seams in order, then the service", func(t *testing.T) {
		events = nil

		resp, _ := call(t, g.Handler(), build(t, nil))

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, []string{eventDataPlane, eventPreprocess, "serve"}, events)
	})

	t.Run("CORS runs after both seams and ends a forbidden origin with no other header", func(t *testing.T) {
		events = nil

		resp, body := call(t, g.Handler(), build(t, map[string]string{headerOrigin: badOrigin}))

		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		assert.Empty(t, body)
		assert.Equal(t, []string{eventDataPlane, eventPreprocess}, events)
		assert.Empty(t, rawHeader(resp.Header, "x-localstack"), "LocalStack terminates the chain, so no response header")
	})

	g.BeginShutdown()

	t.Run("the shutdown guard runs before the seams and answers 503 with the identification headers", func(t *testing.T) {
		events = nil

		resp, _ := call(t, g.Handler(), build(t, nil))

		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.Empty(t, events)
		assert.Equal(t, []string{valueTrue}, rawHeader(resp.Header, "x-localstack"))
	})

	t.Run("an internal call is marked before the guard and still served", func(t *testing.T) {
		events = nil

		resp, _ := call(t, g.Handler(), build(t, map[string]string{"X-Localstack-Data": internalParams}))

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, []string{eventDataPlane, eventPreprocess, "serve"}, events)
	})

	t.Run("a header that is not a json object does not mark the call", func(t *testing.T) {
		for _, value := range []string{"not json", "[1]", "null", "7"} {
			resp, _ := call(t, g.Handler(), build(t, map[string]string{"X-Localstack-Data": value}))

			assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, value)
		}
	})

	t.Run("the developer paths stay up", func(t *testing.T) {
		events = nil

		r := newRequest(t, http.MethodGet, "http://localhost:4566/_localstack/health", "", nil)

		resp, body := call(t, g.Handler(), r)

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "up", body)
		assert.Equal(t, []string{eventDataPlane, eventPreprocess, "health"}, events)
	})
}

func Test_Gateway_hooksReplaceTheRequest(t *testing.T) {
	var got any

	g := newGateway(t, newSettings(), []service.Registration{
		sqsRegistration(func(w http.ResponseWriter, rc *request.Context) {
			got = rc.Request.Context().Value(hookKey{})

			_, _ = io.WriteString(w, "ok")
		}),
	})

	g.SetPreprocessHook(func(r *http.Request) *http.Request {
		return r.WithContext(context.WithValue(r.Context(), hookKey{}, "marked"))
	})
	call(t, g.Handler(), sendMessage(t))
	assert.Equal(t, "marked", got)

	g.SetPreprocessHook(func(*http.Request) *http.Request { return nil })

	got = nil

	call(t, g.Handler(), sendMessage(t))
	assert.Nil(t, got, "a nil result keeps the request as it was")
}

func Test_Gateway_internalMarking(t *testing.T) {
	var internal bool

	g := newGateway(t, newSettings(), []service.Registration{
		sqsRegistration(func(w http.ResponseWriter, rc *request.Context) {
			internal = gateway.IsInternal(rc.Request.Context())

			_, _ = io.WriteString(w, "ok")
		}),
	})

	call(t, g.Handler(), sendMessage(t))
	assert.False(t, internal)

	r := sendMessage(t)
	r.Header.Set("X-Localstack-Data", internalParams)
	call(t, g.Handler(), r)
	assert.True(t, internal)
}

func Test_Gateway_responseHeaders(t *testing.T) {
	tests := []struct {
		name   string
		give   func(*gateway.Settings)
		want   []string
		wantGo []string
	}{
		{name: "on", give: func(*gateway.Settings) {}, want: []string{valueTrue}, wantGo: []string{valueTrue}},
		{name: "off", give: func(s *gateway.Settings) { s.ResponseHeaderEnabled = false }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := newSettings(tt.give)
			g := newGateway(t, settings, []service.Registration{sqsRegistration(okServe)}, func(r chi.Router) {
				r.Get("/_localstack/health", func(http.ResponseWriter, *http.Request) {})
			})

			paths := map[string]*http.Request{
				"aws call":        sendMessage(t),
				"internal route":  newRequest(t, http.MethodGet, "http://localhost:4566/_localstack/health", "", nil),
				"nothing matches": newRequest(t, http.MethodGet, "http://localhost:4566/none", "", nil),
				"preflight":       newRequest(t, http.MethodOptions, "http://localhost:4566/", "", nil),
			}

			for name, r := range paths {
				resp, _ := call(t, g.Handler(), r)

				assert.Equal(t, tt.want, rawHeader(resp.Header, "x-localstack"), name)
				assert.Equal(t, tt.wantGo, rawHeader(resp.Header, "x-gostack"), name)
			}
		})
	}
}

func Test_Gateway_responseHeadersFollowTheSettings(t *testing.T) {
	settings := newSettings()
	g := newGateway(t, settings, []service.Registration{sqsRegistration(okServe)})

	resp, _ := call(t, g.Handler(), sendMessage(t))
	assert.Equal(t, []string{valueTrue}, rawHeader(resp.Header, "x-localstack"))

	settings.update(func(s *gateway.Settings) { s.ResponseHeaderEnabled = false })

	resp, _ = call(t, g.Handler(), sendMessage(t))
	assert.Empty(t, rawHeader(resp.Header, "x-localstack"))
}

func Test_Gateway_contentDecoding(t *testing.T) {
	var zipped bytes.Buffer

	zw := gzip.NewWriter(&zipped)
	_, err := zw.Write([]byte(`{"QueueUrl":"decoded"}`))
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	var (
		body     string
		encoding string
		length   int64
		readErr  error
	)

	g := newGateway(t, newSettings(), []service.Registration{
		sqsRegistration(func(w http.ResponseWriter, rc *request.Context) {
			data, err := io.ReadAll(rc.Request.Body)
			body, readErr = string(data), err
			encoding, length = rc.Request.Header.Get("Content-Encoding"), rc.Request.ContentLength

			_, _ = io.WriteString(w, "ok")
		}),
	})

	t.Run("gzip", func(t *testing.T) {
		r := newRequest(t, http.MethodPost, "http://localhost:4566/", zipped.String(), map[string]string{
			headerAuth: signed("test", usEast1, nameSQS), headerType: jsonType,
			headerTarget: targetSendMessage, "Content-Encoding": "GZIP",
		})

		resp, _ := call(t, g.Handler(), r)

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.NoError(t, readErr)
		assert.JSONEq(t, `{"QueueUrl":"decoded"}`, body)
		assert.Equal(t, "identity", encoding)
		assert.Equal(t, int64(-1), length)
	})

	t.Run("a body that is not gzip fails when it is read", func(t *testing.T) {
		r := sendMessage(t)
		r.Header.Set("Content-Encoding", "gzip")

		resp, _ := call(t, g.Handler(), r)

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Error(t, readErr)
	})

	t.Run("no encoding leaves the body alone", func(t *testing.T) {
		resp, _ := call(t, g.Handler(), sendMessage(t))

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.NoError(t, readErr)
		assert.JSONEq(t, `{"QueueUrl":"q"}`, body)
	})
}

func Test_Gateway_panics(t *testing.T) {
	boom := func(http.ResponseWriter, *request.Context) { panic("boom") }

	tests := []struct {
		name       string
		give       func(*testing.T) *http.Request
		giveFail   bool
		wantStatus int
		wantType   string
		wantBody   string
		wantHeader string
	}{
		{
			name:       "json service",
			give:       sendMessage,
			wantStatus: http.StatusInternalServerError,
			wantType:   contentAmzJSON,
			wantBody:   `{"__type":"InternalError","message":"exception while calling sqs.SendMessage: boom"}`,
			wantHeader: "InternalError",
		},
		{
			name:       "json service under FAIL_FAST",
			give:       sendMessage,
			giveFail:   true,
			wantStatus: http.StatusNotImplemented,
			wantType:   contentAmzJSON,
			wantBody:   `{"__type":"InternalError","message":"exception while calling sqs.SendMessage: boom"}`,
			wantHeader: "InternalError",
		},
		{
			name: "query service",
			give: func(t *testing.T) *http.Request {
				t.Helper()

				return newRequest(t, http.MethodPost, "http://localhost:4566/", "Action=SendMessage&Version=2012-11-05",
					map[string]string{headerAuth: signed("test", usEast1, nameSQS), headerType: formType})
			},
			wantStatus: http.StatusInternalServerError,
			wantType:   contentXML,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := newSettings(func(s *gateway.Settings) { s.FailFast = tt.giveFail })
			g := newGateway(t, settings, []service.Registration{sqsRegistration(boom)})

			server := httptest.NewServer(g.Handler())
			t.Cleanup(server.Close)

			for range 2 {
				r := tt.give(t)
				r.RequestURI = ""
				r.URL = mustParse(t, server.URL+"/")

				resp, err := server.Client().Do(r.WithContext(t.Context()))
				require.NoError(t, err)

				data, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())

				assert.Equal(t, tt.wantStatus, resp.StatusCode)
				assert.Equal(t, tt.wantType, resp.Header.Get(headerType))
				assert.Equal(t, tt.wantHeader, resp.Header.Get("X-Amzn-Errortype"))
				assert.NotEmpty(t, resp.Header.Get("X-Amzn-Requestid"))

				if tt.wantType == contentXML {
					assert.Equal(t, "<?xml version='1.0' encoding='utf-8'?>\n<ErrorResponse><Error><Code>InternalError</Code>"+
						"<Message>exception while calling sqs.SendMessage: boom</Message></Error>"+
						"<RequestId>"+resp.Header.Get("X-Amzn-Requestid")+"</RequestId></ErrorResponse>", string(data))
				} else {
					assert.JSONEq(t, tt.wantBody, string(data))
				}
			}
		})
	}
}

func Test_Gateway_panicOutsideAService(t *testing.T) {
	g := newGateway(t, newSettings(func(s *gateway.Settings) { s.FailFast = true }),
		[]service.Registration{sqsRegistration(okServe)},
		func(r chi.Router) {
			r.Get("/_localstack/boom", func(http.ResponseWriter, *http.Request) { panic("kaboom") })
		})

	resp, body := call(t, g.Handler(), newRequest(t, http.MethodGet, "http://localhost:4566/_localstack/boom", "", nil))

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, "FAIL_FAST applies to service errors only")
	assert.JSONEq(t, `{"error":"Unexpected exception","message":"kaboom","type":"string"}`, body)
	assert.Equal(t, []string{valueTrue}, rawHeader(resp.Header, "x-localstack"))
}

func Test_Gateway_panicAfterTheResponseStarted(t *testing.T) {
	reg := sqsRegistration(func(w http.ResponseWriter, _ *request.Context) {
		_, _ = io.WriteString(w, "partial")
		_ = http.NewResponseController(w).Flush()

		panic("late")
	})
	reg.Metadata.Protocols = []protocol.Kind{protocol.KindJSON, protocol.KindQuery}

	g := newGateway(t, newSettings(), []service.Registration{reg})
	server := httptest.NewServer(g.Handler())
	t.Cleanup(server.Close)

	r := sendMessage(t)
	r.RequestURI = ""
	r.URL = mustParse(t, server.URL+"/")

	resp, err := server.Client().Do(r.WithContext(t.Context()))
	require.NoError(t, err)

	_, err = io.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.Error(t, err, "the client sees the connection break instead of a second status line")
}
