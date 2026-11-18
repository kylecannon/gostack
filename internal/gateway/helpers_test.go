package gateway_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/require"
)

const (
	formType = "application/x-www-form-urlencoded; charset=utf-8"
	jsonType = "application/x-amz-json-1.0"

	nameSQS = "sqs"
	nameSTS = "sts"

	headerAuth   = "Authorization"
	headerType   = "Content-Type"
	headerTarget = "X-Amz-Target"
	headerOrigin = "Origin"

	targetSendMessage = "AmazonSQS.SendMessage"
	opSendMessage     = "SendMessage"

	contentJSON         = "application/json"
	contentAmzJSON      = "application/x-amz-json-1.0"
	contentXML          = "text/xml"
	opGetCallerIdentity = "GetCallerIdentity"
	valueTrue           = "true"
	headerReferer       = "Referer"
	headerRequestMethod = "Access-Control-Request-Method"
	protoHTTP1          = "http1"
	eventDataPlane      = "dataPlane"
	eventPreprocess     = "preprocess"

	allowOrigin = "Access-Control-Allow-Origin"
	appOrigin   = "https://app.localstack.cloud"
	badOrigin   = "http://attacker.com"
	usEast1     = "us-east-1"
)

func TestMain(m *testing.M) {
	// The panic tests would print a stack trace each.
	slog.SetDefault(slog.New(slog.DiscardHandler))

	m.Run()
}

// liveSettings is a Settings that a test can change while the gateway runs.
type liveSettings struct {
	current atomic.Pointer[gateway.Settings]
}

func newSettings(mutate ...func(*gateway.Settings)) *liveSettings {
	s := &liveSettings{}
	s.update(func(settings *gateway.Settings) {
		settings.ResponseHeaderEnabled = true
		settings.Listen = []string{"0.0.0.0:4566"}
	})
	s.update(mutate...)

	return s
}

func (s *liveSettings) get() gateway.Settings {
	return *s.current.Load()
}

func (s *liveSettings) update(mutate ...func(*gateway.Settings)) {
	var next gateway.Settings
	if current := s.current.Load(); current != nil {
		next = *current
	}

	for _, m := range mutate {
		m(&next)
	}

	s.current.Store(&next)
}

// dirtyLog is a Dirtier that remembers what it was told.
type dirtyLog struct {
	mu       sync.Mutex
	services []string
}

func (d *dirtyLog) MarkDirty(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.services = append(d.services, name)
}

func (d *dirtyLog) marked() []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]string(nil), d.services...)
}

// sqsRegistration is a fake service with sqs's metadata: signing name,
// X-Amz-Target prefix and endpoint prefix, speaking json and query.
func sqsRegistration(serve func(http.ResponseWriter, *request.Context)) service.Registration {
	return service.Registration{
		Metadata: service.Metadata{
			Name: nameSQS, SigningName: nameSQS, TargetPrefix: "AmazonSQS", EndpointPrefix: nameSQS,
			APIVersion: "2012-11-05", Protocols: []protocol.Kind{protocol.KindJSON, protocol.KindQuery},
		},
		Serve: serve,
	}
}

// stsRegistration is a fake service with sts's metadata.
func stsRegistration(serve func(http.ResponseWriter, *request.Context)) service.Registration {
	return service.Registration{
		Metadata: service.Metadata{
			Name: nameSTS, SigningName: nameSTS, EndpointPrefix: nameSTS,
			APIVersion: "2011-06-15", Protocols: []protocol.Kind{protocol.KindQuery},
		},
		Serve: serve,
	}
}

// okServe answers 200 with the body "ok".
func okServe(w http.ResponseWriter, _ *request.Context) {
	_, _ = io.WriteString(w, "ok")
}

// newGateway builds a gateway over regs with live settings.
func newGateway(
	t *testing.T,
	settings *liveSettings,
	regs []service.Registration,
	mounts ...func(chi.Router),
) *gateway.Gateway {
	t.Helper()

	var internal func(chi.Router)
	if len(mounts) > 0 {
		internal = mounts[0]
	}

	g, err := gateway.New(settings.get, regs, internal, nil)
	require.NoError(t, err)

	return g
}

// signed returns a SigV4 Authorization header for the signing name.
func signed(accessKey, region, signingName string) string {
	return "AWS4-HMAC-SHA256 Credential=" + accessKey + "/20130524/" + region + "/" + signingName +
		"/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc"
}

// reply is what a handler answered: the status and headers, with the body
// returned beside it.
type reply struct {
	StatusCode int
	Header     http.Header
}

// call sends a request through h and returns the response with its body read.
func call(t *testing.T, h http.Handler, r *http.Request) (reply, string) {
	t.Helper()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return reply{StatusCode: resp.StatusCode, Header: resp.Header}, string(body)
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()

	parsed, err := url.Parse(raw)
	require.NoError(t, err)

	return parsed
}

// rawHeader returns the values stored under name exactly as written, without
// canonicalizing the key, which is how a handler's header casing is checked.
func rawHeader(h http.Header, name string) []string {
	for key, values := range h {
		if key == name {
			return values
		}
	}

	return nil
}

// newRequest builds a request for the handler tests.
func newRequest(t *testing.T, method, target, body string, headers map[string]string) *http.Request {
	t.Helper()

	r := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	for name, value := range headers {
		r.Header.Set(name, value)
	}

	return r
}

// sendMessage is a json SendMessage request to sqs.
func sendMessage(t *testing.T) *http.Request {
	t.Helper()

	return newRequest(t, http.MethodPost, "http://localhost:4566/", `{"QueueUrl":"q"}`, map[string]string{
		headerAuth:   signed("test", usEast1, nameSQS),
		headerType:   jsonType,
		headerTarget: targetSendMessage,
	})
}
