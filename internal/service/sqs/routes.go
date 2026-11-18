package sqs

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/kylecannon/gostack/internal/account"
	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/query"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/route"
)

const (
	// queryNamespace is the xmlns of query responses. It names the schema and is
	// never fetched.
	//nolint:revive // unsecure-url-scheme: an XML namespace.
	queryNamespace  = "http://queue.amazonaws.com/doc/2012-11-05/"
	jsonContentType = "application/x-amz-json-1.0"

	// credentialScopeRegion is the index of the region in the "/" separated
	// X-Amz-Credential value: key, date, region, service, terminator.
	credentialScopeRegion = 2
	// uuidSize is the number of random bytes of a request id.
	uuidSize = 16
	// The bits of a version 4 UUID, RFC 4122 variant: the version occupies the
	// high nibble of byte 6, the variant the two high bits of byte 8.
	versionKeep    = 0x0f
	version4       = 0x40
	variantKeep    = 0x3f
	variantRFC4122 = 0x80
)

// mount adds the routes that make a queue URL an endpoint. A call to
// /<account>/<queue> or /queue/<region>/<account>/<queue>, by GET or POST, is a
// query call on that queue whose QueueUrl defaults to the URL it was sent to.
// The patterns match only a 12-digit account and a queue name, so no other path
// is taken. The developer API routes come with them (see mountDeveloperAPI).
func (p *Provider) mount(r chi.Router) {
	const (
		accountPattern = `{account:[0-9]{12}}`
		queuePattern   = `{queue:[a-zA-Z0-9_-]+(?:\.fifo)?}`
	)

	for _, pattern := range []string{
		"/" + accountPattern + "/" + queuePattern,
		"/queue/{region:[a-z0-9-]+}/" + accountPattern + "/" + queuePattern,
	} {
		r.Get(pattern, p.serveQueueURL)
		r.Post(pattern, p.serveQueueURL)
	}

	p.mountDeveloperAPI(r)
}

// serveQueueURL answers a call sent to a queue URL. It builds the request
// context that the gateway builds for every other call.
func (p *Provider) serveQueueURL(w http.ResponseWriter, r *http.Request) {
	settings := p.settings()
	authorization := r.Header.Get("Authorization")
	identity := account.Resolve(authorization, settings.ParityAWSAccessKeyID)

	if authorization != identity.Authorization {
		r.Header.Set("Authorization", identity.Authorization)
	}

	rc := &request.Context{
		Service:   sqsapi.Metadata.Name,
		Protocol:  protocol.KindQuery,
		AccountID: identity.AccountID,
		Region:    queueURLRegion(r, identity.Region),
		RequestID: newRequestID(),
	}

	// A panic below is answered by the gateway, in the protocol of this call.
	gateway.Record(r, rc)

	if strings.EqualFold(r.Header.Get("Content-Type"), jsonContentType) {
		// Some SDKs send the json protocol to the queue URL. The call carries
		// its QueueUrl in the body.
		rc.Protocol = protocol.KindJSON
		rc.Operation = route.Operation(r, protocol.KindJSON, nil)
		rc.Request = r.WithContext(request.With(r.Context(), rc))
		sqsapi.Serve(p, w, rc) //nolint:contextcheck // the generated Serve takes the context from rc.Request.
		p.markChanged(rc.Operation)

		return
	}

	// The form is read as werkzeug reads it, which also keeps invalid UTF-8 out
	// of the call.
	form, formErr := query.RequestForm(r)
	if formErr != nil {
		writeQueryError(w, rc, query.MalformedQueryString())

		return
	}

	rc.Operation = form.Get("Action")
	if rc.Operation == "" {
		http.Error(w, "<UnknownOperationException/>", http.StatusNotFound)

		return
	}

	err := checkQueueAction(rc.Operation)
	if err != nil {
		writeQueryError(w, rc, err)

		return
	}

	// A QueueUrl in the form wins over the URL the call was sent to.
	if !form.Has("QueueUrl") {
		form.Set("QueueUrl", baseURL(r))
	}

	rc.Request = r.WithContext(request.With(r.Context(), rc))

	held := &heldResponse{header: http.Header{}}
	sqsapi.ServeValidated(p, held, rc) //nolint:contextcheck // the generated Serve takes the context from rc.Request.
	held.markSenderFault()
	held.sendAs(w, negotiateMime(r))
	p.markChanged(rc.Operation)
}

// markChanged tells persistence that a call made to a queue URL may have
// changed state. It does not wait to learn whether the call succeeded: a failed
// one costs a flush of a state that did not change, which is harmless.
func (p *Provider) markChanged(operation string) {
	if operation != "" && mutating(operation) {
		p.notifyChanged()
	}
}

// checkQueueAction refuses what a queue URL cannot do: an operation that does
// not exist, and the two that are not about one queue.
func checkQueueAction(operation string) *awserr.Error {
	if operation == "CreateQueue" || operation == "ListQueues" || !slices.Contains(sqsapi.Operations, operation) {
		return &awserr.Error{
			Code:       "InvalidAction",
			Message:    "The action " + operation + " is not valid for this endpoint.",
			StatusCode: http.StatusBadRequest,
			Fault:      awserr.FaultSender,
		}
	}

	return nil
}

func writeQueryError(w http.ResponseWriter, rc *request.Context, err *awserr.Error) {
	query.WriteError(w, &query.Envelope{
		Namespace:    queryNamespace,
		RequestID:    rc.RequestID,
		EscapeQuotes: true,
	}, err)
}

// queueURLRegion is the region a queue URL call is made in, which LocalStack
// reads from the part of the URL that carries it: the path of the path
// strategy, the host of the standard and domain ones. A URL with no region in
// it takes the region of the credentials, as LocalStack's legacy handler does.
func queueURLRegion(r *http.Request, credentialRegion string) string {
	if region := chi.URLParam(r, "region"); region != "" {
		return region
	}

	host := r.Host

	name, _, err := net.SplitHostPort(host)
	if err == nil {
		host = name
	}

	labels := strings.Split(host, ".")

	switch {
	case labels[0] == "sqs" && len(labels) > 2 && labels[1] != "":
		return labels[1]
	case labels[0] == pathPrefix:
		return domainRegion
	case len(labels) > 2 && labels[1] == pathPrefix && labels[0] != "":
		return labels[0]
	}

	if credential := r.URL.Query().Get("X-Amz-Credential"); credential != "" {
		if parts := strings.Split(credential, "/"); len(parts) > credentialScopeRegion {
			return parts[credentialScopeRegion]
		}
	}

	return credentialRegion
}

// newRequestID returns a random UUID, version 4.
func newRequestID() string {
	var id [uuidSize]byte

	// crypto/rand.Read never returns an error.
	_, _ = rand.Read(id[:])

	id[6] = id[6]&versionKeep | version4
	id[8] = id[8]&variantKeep | variantRFC4122

	var out [36]byte

	hex.Encode(out[0:8], id[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], id[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], id[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], id[8:10])
	out[23] = '-'
	hex.Encode(out[24:], id[10:])

	return string(out[:])
}
