package gateway_test

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/restxml/fixturegen_test/in01"
	"github.com/kylecannon/gostack/internal/protocol/restxml/fixturegen_test/in34"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_Gateway_markDirty_Rest serves rest-xml services gostack-codegen
// generated for botocore's fixture groups through the gateway: BodyWithXmlName
// (in01), a write, and QueryPrecedence (in34), whose name reads as a query.
// The gateway leaves a rest call's operation for the service to find, so the
// service's generated router has to name it before the gateway decides
// whether the call changed state.
func Test_Gateway_markDirty_Rest(t *testing.T) {
	bodyWithXMLName := service.Registration{
		Metadata: in01.Metadata,
		Serve:    func(w http.ResponseWriter, rc *request.Context) { in01.Serve(&in01.Fixture{}, w, rc) },
	}
	queryPrecedence := service.Registration{
		Metadata: in34.Metadata,
		Serve:    func(w http.ResponseWriter, rc *request.Context) { in34.Serve(&in34.Fixture{}, w, rc) },
	}

	tests := []struct {
		name       string
		giveReg    service.Registration
		giveMethod string
		giveTarget string
		wantStatus int
		wantMarked []string
	}{
		{
			name: "a routed write", giveReg: bodyWithXMLName, giveMethod: http.MethodPut, giveTarget: "/BodyWithXmlName",
			wantStatus: http.StatusOK, wantMarked: []string{in01.Metadata.Name},
		},
		{
			name: "a routed read", giveReg: queryPrecedence, giveMethod: http.MethodPost,
			giveTarget: "/Precedence?bar=named&qux=alsoFromMap", wantStatus: http.StatusOK,
		},
		{
			name: "a call no operation binds", giveReg: bodyWithXMLName, giveMethod: http.MethodPut,
			giveTarget: "/Nowhere", wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dirty := &dirtyLog{}

			g, err := gateway.New(newSettings().get, []service.Registration{tt.giveReg}, nil, dirty)
			require.NoError(t, err)

			r := newRequest(t, tt.giveMethod, "http://localhost:4566"+tt.giveTarget,
				"<Ahoy><nested><name>Phreddy</name></nested></Ahoy>", map[string]string{
					headerAuth:     signed("test", usEast1, tt.giveReg.Metadata.SigningName),
					"Content-Type": "application/xml",
				})

			resp, body := call(t, g.Handler(), r)

			assert.Equal(t, tt.wantStatus, resp.StatusCode, body)
			assert.Equal(t, tt.wantMarked, dirty.marked())
		})
	}
}

// recordLog is a slog.Handler that keeps the records it is given.
type recordLog struct {
	mu      sync.Mutex
	records []slog.Record
}

func (*recordLog) Enabled(context.Context, slog.Level) bool { return true }

func (l *recordLog) Handle(_ context.Context, record slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.records = append(l.records, record.Clone())

	return nil
}

func (l *recordLog) WithAttrs([]slog.Attr) slog.Handler { return l }

func (l *recordLog) WithGroup(string) slog.Handler { return l }

// errorServices returns the service attribute of each error record.
func (l *recordLog) errorServices() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	var services []string

	for _, record := range l.records {
		if record.Level != slog.LevelError {
			continue
		}

		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "service" {
				services = append(services, attr.Value.String())
			}

			return true
		})
	}

	return services
}

// Test_Gateway_markDirty_RestWithoutOperation covers SQ-86's dead rest
// mutation: a rest service that answers a call with success without naming its
// operation never has its Mutating asked, so the change is not saved. The
// gateway logs that once per service, at error level, persistence on or off.
func Test_Gateway_markDirty_RestWithoutOperation(t *testing.T) {
	const nameRest = "restfake"

	answer := func(status int, operation string) func(http.ResponseWriter, *request.Context) {
		return func(w http.ResponseWriter, rc *request.Context) {
			rc.Operation = operation
			w.WriteHeader(status)
		}
	}
	always := func(string) bool { return true }

	tests := []struct {
		name         string
		giveServe    func(http.ResponseWriter, *request.Context)
		giveMutating func(string) bool
		giveDirty    bool
		wantLogged   []string
		wantMarked   []string
	}{
		{
			name:      "a success without an operation is logged once",
			giveServe: answer(http.StatusOK, ""), giveMutating: always, giveDirty: true,
			wantLogged: []string{nameRest},
		},
		{
			name:      "with persistence off too",
			giveServe: answer(http.StatusNoContent, ""), giveMutating: always,
			wantLogged: []string{nameRest},
		},
		{
			name:      "a call no operation binds is the client's error, not a bug",
			giveServe: answer(http.StatusNotFound, ""), giveMutating: always, giveDirty: true,
		},
		{
			name:      "a service without its own rule is not asked",
			giveServe: answer(http.StatusOK, ""), giveDirty: true,
		},
		{
			name:      "a named operation is marked",
			giveServe: answer(http.StatusOK, "PutThing"), giveMutating: always, giveDirty: true,
			wantMarked: []string{nameRest, nameRest},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := &recordLog{}
			previous := slog.Default()

			slog.SetDefault(slog.New(log))
			t.Cleanup(func() { slog.SetDefault(previous) })

			reg := service.Registration{
				Metadata: service.Metadata{
					Name: nameRest, SigningName: nameRest, Protocols: []protocol.Kind{protocol.KindRestXML},
				},
				Serve:    tt.giveServe,
				Mutating: tt.giveMutating,
			}

			dirty := &dirtyLog{}

			var eng gateway.Dirtier
			if tt.giveDirty {
				eng = dirty
			}

			g, err := gateway.New(newSettings().get, []service.Registration{reg}, nil, eng)
			require.NoError(t, err)

			for range 2 {
				call(t, g.Handler(), newRequest(t, http.MethodPut, "http://localhost:4566/thing", "",
					map[string]string{headerAuth: signed("test", usEast1, nameRest)}))
			}

			assert.Equal(t, tt.wantLogged, log.errorServices())
			assert.Equal(t, tt.wantMarked, dirty.marked())
		})
	}
}
