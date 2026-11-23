package sns

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/kylecannon/gostack/internal/service/sns/envelope"
)

// defaultHost is what LOCALSTACK_HOST defaults to, used when the settings name
// no host, so a Provider built without settings still hands out URLs.
const defaultHost = "localhost.localstack.cloud:4566"

// MountProtocol adds the signing certificate route to r. The certificate URL is
// a protocol path, embedded in every signed message, so it is mounted whatever
// GOSTACK_COMPAT_PATHS says; see docs/architecture.md, "Protocol and data-plane
// paths". It is not Registration.Mount: that hook lets a service's routes meet
// other services' requests, and a change to such a service selects every
// service's parity suite. The process hands this method to internalapi, which
// owns the always-on paths, as internalapi.Deps.Protocol.
//
// The handler is the Provider's own signer: the one that signs every message is
// the one whose certificate is served, and a second Signer would sign with a
// key no served certificate verifies.
//
// The developer endpoints under /_gostack/services/sns and /_aws/sns are not
// protocol paths: they are Registration.DeveloperMount (mountDeveloperAPI), and
// the gateway's GOSTACK_COMPAT_PATHS switch turns their /_aws names off. The
// certificate lives under /_aws/sns too and stays on.
func (p *Provider) MountProtocol(r chi.Router) {
	r.Method(http.MethodGet, envelope.CertPath, p.signer)
	r.Method(http.MethodHead, envelope.CertPath, p.signer)
}

// Envelope returns the signed JSON body of in, as it is delivered to a
// subscriber: the one place a message is signed, so every publisher uses the
// Provider's single Signer, and the SigningCertURL, SubscribeURL and
// UnsubscribeURL follow SNS_CERT_URL_HOST as it stands now.
func (p *Provider) Envelope(in *envelope.Message) ([]byte, error) {
	body, err := p.signer.Body(in, p.baseURL())
	if err != nil {
		return nil, fmt.Errorf("sign sns envelope: %w", err)
	}

	return body, nil
}

// baseURL is the scheme and host the URLs in an envelope start with. It reads
// the settings on each call, so a change to SNS_CERT_URL_HOST or LOCALSTACK_HOST
// through /_localstack/config applies to the next message.
func (p *Provider) baseURL() string {
	s := p.settings()

	host := s.Host
	if host == "" {
		host = defaultHost
	}

	return envelope.BaseURL(s.CertURLHost, "http://"+host)
}
