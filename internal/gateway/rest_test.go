package gateway_test

import (
	"net/http"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/protocol/restxml/fixturegen_test/in01"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_Gateway_markDirty_Rest serves a rest-xml service gostack-codegen
// generated, the one of botocore's BodyWithXmlName fixture group, through the
// gateway. The gateway leaves a rest call's operation for the service to find,
// so the service's generated router has to name it before the gateway decides
// whether the call changed state.
func Test_Gateway_markDirty_Rest(t *testing.T) {
	tests := []struct {
		name       string
		giveMethod string
		givePath   string
		wantStatus int
		wantMarked []string
	}{
		{
			name: "a routed mutating call", giveMethod: http.MethodPut, givePath: "/BodyWithXmlName",
			wantStatus: http.StatusOK, wantMarked: []string{in01.Metadata.Name},
		},
		{
			name: "a call no operation binds", giveMethod: http.MethodPut, givePath: "/Nowhere",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dirty := &dirtyLog{}
			reg := service.Registration{
				Metadata: in01.Metadata,
				Serve: func(w http.ResponseWriter, rc *request.Context) {
					in01.Serve(&in01.Fixture{}, w, rc)
				},
			}

			g, err := gateway.New(newSettings().get, []service.Registration{reg}, nil, dirty)
			require.NoError(t, err)

			r := newRequest(t, tt.giveMethod, "http://localhost:4566"+tt.givePath,
				"<Ahoy><nested><name>Phreddy</name></nested></Ahoy>", map[string]string{
					headerAuth:     signed("test", usEast1, in01.Metadata.SigningName),
					"Content-Type": "application/xml",
				})

			resp, body := call(t, g.Handler(), r)

			assert.Equal(t, tt.wantStatus, resp.StatusCode, body)
			assert.Equal(t, tt.wantMarked, dirty.marked())
		})
	}
}
