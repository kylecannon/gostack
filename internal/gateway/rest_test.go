package gateway_test

import (
	"net/http"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
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
