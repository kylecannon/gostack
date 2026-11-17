package gateway_test

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// seen is what a fake Registration saw of one call.
type seen struct {
	rc            *request.Context
	body          string
	authorization string
}

func recorder(calls *[]seen) func(http.ResponseWriter, *request.Context) {
	return func(w http.ResponseWriter, rc *request.Context) {
		body, _ := io.ReadAll(rc.Request.Body)
		*calls = append(*calls, seen{rc: rc, body: string(body), authorization: rc.Request.Header.Get(headerAuth)})

		_, _ = io.WriteString(w, "ok")
	}
}

func Test_Gateway_requestContext(t *testing.T) {
	const form = "Action=SendMessage&Version=2012-11-05&QueueUrl=http%3A%2F%2Fq&MessageBody=hi+there"

	tests := []struct {
		name        string
		give        func(*testing.T) *http.Request
		giveParity  bool
		wantService string
		wantOp      string
		wantProto   protocol.Kind
		wantAccount string
		wantRegion  string
		wantAuth    string
		wantBody    string
	}{
		{
			name:        "sqs json",
			give:        sendMessage,
			wantService: nameSQS,
			wantOp:      opSendMessage,
			wantProto:   protocol.KindJSON,
			wantAccount: "000000000000",
			wantRegion:  usEast1,
			wantAuth:    signed("test", usEast1, nameSQS),
			wantBody:    `{"QueueUrl":"q"}`,
		},
		{
			name: "sqs query reads the Action from the body and leaves the body for the service",
			give: func(t *testing.T) *http.Request {
				t.Helper()

				return newRequest(t, http.MethodPost, "http://localhost:4566/", form, map[string]string{
					headerAuth: signed("123456789012", "eu-west-1", nameSQS),
					headerType: formType,
				})
			},
			wantService: nameSQS,
			wantOp:      opSendMessage,
			wantProto:   protocol.KindQuery,
			wantAccount: "123456789012",
			wantRegion:  "eu-west-1",
			wantAuth:    signed("123456789012", "eu-west-1", nameSQS),
			wantBody:    form,
		},
		{
			name: "parity decodes an AKIA key",
			give: func(t *testing.T) *http.Request {
				t.Helper()

				return newRequest(t, http.MethodPost, "http://localhost:4566/", form, map[string]string{
					headerAuth: signed("AKIARZPUZDIKGB2VALC4", "ap-south-1", nameSQS),
					headerType: formType,
				})
			},
			giveParity:  true,
			wantService: nameSQS,
			wantOp:      opSendMessage,
			wantProto:   protocol.KindQuery,
			wantAccount: "123456789012",
			wantRegion:  "ap-south-1",
			wantAuth:    signed("AKIARZPUZDIKGB2VALC4", "ap-south-1", nameSQS),
			wantBody:    form,
		},
		{
			name: "an unknown region is rewritten, in the header too",
			give: func(t *testing.T) *http.Request {
				t.Helper()

				return newRequest(t, http.MethodPost, "http://localhost:4566/", "Action=GetCallerIdentity", map[string]string{
					headerAuth: signed("test", "xx-south-1", nameSTS),
					headerType: formType,
				})
			},
			wantService: nameSTS,
			wantOp:      "GetCallerIdentity",
			wantProto:   protocol.KindQuery,
			wantAccount: "000000000000",
			wantRegion:  usEast1,
			wantAuth:    signed("test", usEast1, nameSTS),
			wantBody:    "Action=GetCallerIdentity",
		},
		{
			name: "an unsigned call to the sts host gets the defaults",
			give: func(t *testing.T) *http.Request {
				t.Helper()

				return newRequest(t, http.MethodPost, "http://sts.localhost.localstack.cloud:4566/",
					"Action=AssumeRoleWithWebIdentity&Version=2011-06-15", map[string]string{headerType: formType})
			},
			wantService: nameSTS,
			wantOp:      "AssumeRoleWithWebIdentity",
			wantProto:   protocol.KindQuery,
			wantAccount: "000000000000",
			wantRegion:  usEast1,
			wantAuth:    "",
			wantBody:    "Action=AssumeRoleWithWebIdentity&Version=2011-06-15",
		},
		{
			name: "the Action may be in the query string",
			give: func(t *testing.T) *http.Request {
				t.Helper()

				return newRequest(t, http.MethodGet, "http://localhost:4566/?Action=GetCallerIdentity&Version=2011-06-15",
					"", map[string]string{headerAuth: signed("test", usEast1, nameSTS)})
			},
			wantService: nameSTS,
			wantOp:      "GetCallerIdentity",
			wantProto:   protocol.KindQuery,
			wantAccount: "000000000000",
			wantRegion:  usEast1,
			wantAuth:    signed("test", usEast1, nameSTS),
			wantBody:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []seen

			settings := newSettings(func(s *gateway.Settings) { s.ParityAWSAccessKeyID = tt.giveParity })
			g := newGateway(t, settings, []service.Registration{
				sqsRegistration(recorder(&calls)),
				stsRegistration(recorder(&calls)),
			})

			resp, body := call(t, g.Handler(), tt.give(t))
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Equal(t, "ok", body)
			require.Len(t, calls, 1)

			rc := calls[0].rc
			assert.Equal(t, tt.wantService, rc.Service)
			assert.Equal(t, tt.wantOp, rc.Operation)
			assert.Equal(t, tt.wantProto, rc.Protocol)
			assert.Equal(t, tt.wantAccount, rc.AccountID)
			assert.Equal(t, tt.wantRegion, rc.Region)
			assert.Regexp(t, uuidV4, rc.RequestID)
			assert.Equal(t, tt.wantAuth, calls[0].authorization)
			assert.Equal(t, tt.wantBody, calls[0].body)

			fromContext, ok := request.From(rc.Request.Context())
			require.True(t, ok)
			assert.Same(t, rc, fromContext)
		})
	}
}

func Test_Gateway_requestIDs(t *testing.T) {
	var calls []seen

	g := newGateway(t, newSettings(), []service.Registration{sqsRegistration(recorder(&calls))})

	for range 3 {
		call(t, g.Handler(), sendMessage(t))
	}

	require.Len(t, calls, 3)
	assert.NotEqual(t, calls[0].rc.RequestID, calls[1].rc.RequestID)
	assert.NotEqual(t, calls[1].rc.RequestID, calls[2].rc.RequestID)
}

func Test_Gateway_markDirty(t *testing.T) {
	failing := func(w http.ResponseWriter, _ *request.Context) { w.WriteHeader(http.StatusBadRequest) }
	panicking := func(http.ResponseWriter, *request.Context) { panic("boom") }
	custom := func(operation string) bool { return operation == "Purge" }

	operation := func(op string) func(*testing.T) *http.Request {
		return func(t *testing.T) *http.Request {
			t.Helper()

			r := sendMessage(t)
			r.Header.Set(headerTarget, "AmazonSQS."+op)

			return r
		}
	}

	tests := []struct {
		name         string
		giveServe    func(http.ResponseWriter, *request.Context)
		giveMutating func(string) bool
		giveRequest  func(*testing.T) *http.Request
		wantMarked   []string
	}{
		{name: "send", giveServe: okServe, giveRequest: operation("SendMessage"), wantMarked: []string{nameSQS}},
		{name: "create", giveServe: okServe, giveRequest: operation("CreateQueue"), wantMarked: []string{nameSQS}},
		{name: "get", giveServe: okServe, giveRequest: operation("GetQueueUrl")},
		{name: "list", giveServe: okServe, giveRequest: operation("ListQueues")},
		{name: "describe", giveServe: okServe, giveRequest: operation("DescribeThings")},
		{name: "query", giveServe: okServe, giveRequest: operation("QueryThings")},
		{name: "head", giveServe: okServe, giveRequest: operation("HeadThing")},
		{name: "receive", giveServe: okServe, giveRequest: operation("ReceiveMessage")},
		{name: "an error response", giveServe: failing, giveRequest: operation("SendMessage")},
		{name: "a panic", giveServe: panicking, giveRequest: operation("SendMessage")},
		{name: "no operation", giveServe: okServe, giveRequest: operation("")},
		{
			name: "own rule marks", giveServe: okServe, giveMutating: custom,
			giveRequest: operation("Purge"), wantMarked: []string{nameSQS},
		},
		{
			name: "own rule can say no for a write verb", giveServe: okServe, giveMutating: custom,
			giveRequest: operation("SendMessage"),
		},
		{
			name: "own rule can say yes for a read verb", giveServe: okServe,
			giveMutating: func(string) bool { return true }, giveRequest: operation("ListQueues"),
			wantMarked: []string{nameSQS},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dirty := &dirtyLog{}
			reg := sqsRegistration(tt.giveServe)
			reg.Mutating = tt.giveMutating

			g, err := gateway.New(newSettings().get, []service.Registration{reg}, nil, dirty)
			require.NoError(t, err)

			call(t, g.Handler(), tt.giveRequest(t))

			assert.Equal(t, tt.wantMarked, dirty.marked())
		})
	}
}

func Test_Gateway_noPersistence(t *testing.T) {
	g, err := gateway.New(newSettings().get, []service.Registration{sqsRegistration(okServe)}, nil, nil)
	require.NoError(t, err)

	resp, _ := call(t, g.Handler(), sendMessage(t))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func Test_Gateway_noService(t *testing.T) {
	g := newGateway(t, newSettings(), []service.Registration{sqsRegistration(okServe), stsRegistration(okServe)})

	tests := []struct {
		name string
		give *http.Request
	}{
		{name: "root", give: newRequest(t, http.MethodGet, "http://localhost:4566/", "", nil)},
		{name: "unknown path", give: newRequest(t, http.MethodGet, "http://localhost:4566/nothing/here", "", nil)},
		{
			name: "an unregistered service",
			give: newRequest(t, http.MethodPost, "http://localhost:4566/", "", map[string]string{
				headerAuth: signed("test", usEast1, "s3"),
			}),
		},
		{
			name: "an Action nobody can claim",
			give: newRequest(t, http.MethodPost, "http://localhost:4566/", "Action=X&Version=1999-01-01",
				map[string]string{headerType: formType}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := call(t, g.Handler(), tt.give)

			assert.Equal(t, http.StatusNotFound, resp.StatusCode)
			assert.JSONEq(t, `{"message": "Not Found"}`, body)
			assert.Equal(t, "application/json", resp.Header.Get(headerType))
			assert.Equal(t, []string{"true"}, rawHeader(resp.Header, "x-localstack"))
		})
	}
}

func Test_Gateway_ambiguous(t *testing.T) {
	twin := stsRegistration(okServe)
	twin.Metadata.Name = "sts-twin"

	for _, order := range [][]service.Registration{
		{stsRegistration(okServe), twin},
		{twin, stsRegistration(okServe)},
	} {
		g := newGateway(t, newSettings(), order)

		resp, body := call(t, g.Handler(), newRequest(t, http.MethodPost, "http://localhost:4566/", "", map[string]string{
			headerAuth: signed("test", usEast1, nameSTS),
		}))

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Contains(t, body, "ambiguous request: claimed by sts, sts-twin")
	}
}

func Test_New_rejectsBadRegistrations(t *testing.T) {
	tests := []struct {
		name string
		give []service.Registration
	}{
		{name: "same name twice", give: []service.Registration{sqsRegistration(okServe), sqsRegistration(okServe)}},
		{
			name: "no name",
			give: []service.Registration{{Metadata: service.Metadata{Protocols: []protocol.Kind{protocol.KindQuery}}}},
		},
		{name: "no protocol", give: []service.Registration{{Metadata: service.Metadata{Name: "x"}}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := gateway.New(newSettings().get, tt.give, nil, nil)
			require.Error(t, err)
		})
	}
}

func Test_Gateway_mounts(t *testing.T) {
	served := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, name) }
	}

	reg := sqsRegistration(okServe)
	reg.Mount = func(r chi.Router) {
		r.Get("/{account}/{queue}", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "queue "+chi.URLParam(r, "account")+" "+chi.URLParam(r, "queue"))
		})
	}

	g := newGateway(t, newSettings(), []service.Registration{reg}, func(r chi.Router) {
		r.Get("/_localstack/health", served("health"))
	})

	tests := []struct {
		name     string
		giveMeth string
		givePath string
		want     string
	}{
		{name: "internal route", giveMeth: http.MethodGet, givePath: "/_localstack/health", want: "health"},
		{
			name: "registration route", giveMeth: http.MethodGet,
			givePath: "/000000000000/my-queue", want: "queue 000000000000 my-queue",
		},
		{name: "AWS call falls through to detection", giveMeth: http.MethodPost, givePath: "/", want: "ok"},
		{
			name: "same path, other method falls through", giveMeth: http.MethodPost,
			givePath: "/000000000000/my-queue", want: "ok",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRequest(t, tt.giveMeth, "http://localhost:4566"+tt.givePath, "", map[string]string{
				headerAuth: signed("test", usEast1, nameSQS),
			})

			resp, body := call(t, g.Handler(), r)
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, tt.want, body)
		})
	}
}

func Test_Gateway_pathsAreNotRewritten(t *testing.T) {
	var path, rawPath string

	reg := sqsRegistration(func(w http.ResponseWriter, rc *request.Context) {
		path, rawPath = rc.Request.URL.Path, rc.Request.URL.EscapedPath()
		_, _ = io.WriteString(w, "ok")
	})
	g := newGateway(t, newSettings(), []service.Registration{reg})

	for _, target := range []string{"/a//b/", "/a/../b", "/a%2Fb/"} {
		r := newRequest(t, http.MethodPost, "http://localhost:4566"+target, "", map[string]string{
			headerAuth: signed("test", usEast1, nameSQS),
		})

		resp, _ := call(t, g.Handler(), r)
		require.Equal(t, http.StatusOK, resp.StatusCode, target)
		assert.Equal(t, r.URL.Path, path, target)
		assert.Equal(t, r.URL.EscapedPath(), rawPath, target)
		assert.NotEqual(t, http.StatusMovedPermanently, resp.StatusCode)
	}

	require.True(t, strings.HasSuffix(path, "/"), "the trailing slash survives")
}
