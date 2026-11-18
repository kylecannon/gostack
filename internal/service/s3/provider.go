package s3

import (
	"context"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"

	s3api "github.com/kylecannon/gostack/internal/api/s3"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/blob"
	"github.com/kylecannon/gostack/internal/inproc"
	"github.com/kylecannon/gostack/internal/protocol/query"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/store"
)

const (
	// serviceName is S3's name in the catalog, which service.Own marks.
	serviceName = "s3"

	// hostID is the x-amz-id-2 LocalStack's S3 serializer sends on every
	// response (S3_HOST_ID).
	hostID = "9Gjjt1m+cjU4OPvX9O9/8RuvnG41MRb/18Oux2o5H5MY7ISNTlXN+Dz9IG62/ILVxhAGI0qyPfg="
	// controlPath is the path prefix of every s3control operation, which shares
	// S3's signing name (service_router.py:168).
	controlPath = "/v20180820"

	contentTypeXML = "application/xml"

	// serverName is the Server header AWS's S3 sends.
	serverName = "AmazonS3"
)

// Settings is what S3 reads from the configuration on every call.
type Settings struct {
	// LocalstackHost is LOCALSTACK_HOST as host:port. S3 claims requests to
	// s3.<host>, and a bucket created outside us-east-1 is located on it.
	LocalstackHost string
	// ValidateSignatures is S3_SKIP_SIGNATURE_VALIDATION=0: a presigned URL
	// whose signature does not match, that has expired or that carries headers
	// it did not sign is refused. The default, false, lets it through.
	ValidateSignatures bool
	// GatewayPort is the port of the first GATEWAY_LISTEN address. A presigned
	// URL's host is also tried with it when its signature does not match.
	GatewayPort int
}

// hostname is the host part of LocalstackHost.
func (s Settings) hostname() string {
	host, _, err := net.SplitHostPort(s.LocalstackHost)
	if err != nil {
		return s.LocalstackHost
	}

	return host
}

// Provider implements the S3 API. Operations it does not implement yet answer
// 501, because it embeds s3api.Unimplemented.
type Provider struct {
	s3api.Unimplemented

	settings func() Settings
	states   *store.Bundle[State]
	names    nameIndex
	blobs    blob.Store

	// transfer is held for writing by whatever replaces or copies the set of
	// buckets as a whole: Snapshot, Restore and Reset. CreateBucket and
	// DeleteBucket hold it for reading from the name claim to the State change,
	// so neither falls between a restore's swap of the buckets and of the names.
	// It is taken before State.mu and Bucket.mu, and never held over a body.
	transfer sync.RWMutex

	// client delivers notifications, and checks their targets, through the
	// gateway. Bind gives it the handler.
	client *inproc.Client

	// taken counts the responses written through a hijacked connection, which
	// http.Server.Shutdown does not wait for (rawheader.go). Close does.
	taken inflight
}

var _ s3api.Handler = (*Provider)(nil)

// New returns a Provider with no buckets that keeps bodies in blobs. settings
// is called on every operation, so a configuration change applies to the next
// one.
func New(settings func() Settings, blobs blob.Store) *Provider {
	return &Provider{
		settings: settings,
		states:   store.NewBundle(newState, emptyState),
		blobs:    blobs,
		client:   inproc.New(),
	}
}

// UseBlobs makes blobs the store S3 keeps its bodies in, in place of the one New
// was given: a persisted S3 keeps them as assets of the persistence engine. It
// is called once, before S3 serves or its state loads.
func (p *Provider) UseBlobs(blobs blob.Store) {
	p.blobs = blobs
}

// Registration describes S3 to the gateway: the generated dispatcher in front
// of p, the hosts and legacy rules that make a request S3's, and the lifecycle
// of its state.
func (p *Provider) Registration() service.Registration {
	return service.Registration{
		Metadata:       s3api.Metadata,
		Serve:          p.serve,
		ClaimHost:      p.claimHost,
		SigningDefault: true,
		Fallback:       fallback,
		Preprocess:     p.preprocess,
		RawBody:        true,
		OwnCORS:        true,
		Persist:        p,
		Reset:          p.Reset,
		Close:          p.Close,
	}
}

// Reset empties the state of an account and region: the buckets of that region,
// their objects and their bodies. An empty account or region matches every one.
// It backs /_gostack/state/reset. A body goes once its bucket has left the
// State, so a persisted S3 removes only what no state names.
func (p *Provider) Reset(account, region string) {
	p.transfer.Lock()
	defer p.transfer.Unlock()

	p.states.Range(func(owner, _ string, s *State) bool {
		if account != "" && owner != account {
			return true
		}

		for _, b := range s.take(region) {
			p.names.release(b.Name, owner)
			p.retire(b)
		}

		return true
	})
}

// Close waits for the responses still being written on a hijacked connection,
// which the gateway's drain cannot see, then drops the notifications not yet
// delivered and lets go of the bodies the store holds, when it is one that can
// be closed. The wait ends at the services' Close budget, because a Close that
// overruns it is abandoned, or at drainLimit.
func (p *Provider) Close() {
	p.taken.drain(drainLimit)
	p.client.Close()

	if closer, ok := p.blobs.(interface{ Close() }); ok {
		closer.Close()
	}
}

// serve answers one S3 call. A website host and an s3control path reach it
// because S3 claims the host or the signing name; a website host is served
// here, and an s3control path is refused.
func (p *Provider) serve(w http.ResponseWriter, rc *request.Context) {
	w.Header()["x-amz-id-2"] = []string{hostID}
	// S3 names itself in Server, as AWS does. It sends no x-amzn-RequestId: with
	// one, botocore reads the request id from it and drops HostId, so the
	// recorded snapshots of aws.validated tests, which have HostId, would differ.
	w.Header().Set("Server", serverName)

	r := rc.Request

	if p.serveCORS(w, rc) {
		return
	}

	if rc.CORS != nil {
		w = &defaultCORSWriter{ResponseWriter: w, rc: rc}
	}

	switch {
	case isWebsiteHost(r.Host):
		delete(w.Header(), "x-amz-id-2")
		p.serveWebsite(w, rc)
	case isControlCall(r):
		// gostack has no s3control, so its calls get the gateway's answer for a
		// service that is not enabled.
		query.WriteError(w, &query.Envelope{RequestID: rc.RequestID}, &awserr.Error{
			Code:       "InternalFailure",
			Message:    "Service 's3control' is not enabled. Please check your 'SERVICES' configuration variable.",
			StatusCode: http.StatusNotImplemented,
			Fault:      awserr.FaultReceiver,
		})
	default:
		rw := &responseWriter{
			ResponseWriter: w, method: r.Method, http1: r.ProtoMajor == 1, http11: r.ProtoAtLeast(1, 1), inflight: &p.taken,
		}
		rw.trackBody(r)

		// abort does nothing once finish has run; it ends a taken response whose
		// handler panicked.
		defer rw.abort()

		p.serveAPI(rw, rc)
		rw.finish()
	}
}

// isControlCall reports whether r is an s3control call that detection gave S3
// because the signing name is S3's and s3control is not registered: a path
// that is /v20180820 or under it that preprocess did not own. A request to a
// host S3 claims, a virtual host's included, or to an existing bucket named
// v20180820 is owned, and stays S3's, as LocalStack's S3 pre-router decides it
// before the signing name is read.
func isControlCall(r *http.Request) bool {
	path := r.URL.Path
	if path != controlPath && !strings.HasPrefix(path, controlPath+"/") {
		return false
	}

	return service.OwnerOf(r) != serviceName
}

// retire marks a bucket that left its State as gone, so a call that found it
// before cannot write to it, and removes its bodies.
func (p *Provider) retire(b *Bucket) {
	for _, id := range b.drop() {
		_ = p.blobs.Remove(id)
	}
}

func (p *Provider) state(account string) *State {
	return p.states.Get(account, "")
}

// bucket finds a bucket by name for the caller's account: its own, or another
// account's, as LocalStack's _get_cross_account_bucket does.
func (p *Provider) bucket(account, name string) (*Bucket, error) {
	if b := p.state(account).get(name); b != nil {
		return b, nil
	}

	if owner, ok := p.names.owner(name); ok && owner != account {
		if b := p.state(owner).get(name); b != nil {
			return b, nil
		}
	}

	return nil, noSuchBucket(name)
}

// responseWriter gives an S3 response the Content-Type LocalStack's does:
// application/xml unless the handler set one, except on a 204 and on a
// successful PUT, which carry no body (s3_response_handler).
//
// A response over HTTP/1.x that has a header value starting or ending in a space
// or tab takes the connection and writes itself (rawheader.go), because net/http
// would trim the value.
type responseWriter struct {
	http.ResponseWriter

	method string
	http1  bool
	http11 bool // HTTP/1.1 or later: the client reads a chunked body.
	wrote  bool

	// raw is set once takeConn has the connection.
	raw *takenConn

	// inflight counts the taken responses, and body notes whether the request
	// body was read to its end (see trackBody). Both belong to this response.
	inflight *inflight
	body     trackedBody
}

func (w *responseWriter) WriteHeader(code int) {
	if w.raw != nil {
		return
	}

	if !w.wrote && code >= http.StatusOK {
		w.wrote = true

		header := w.Header()

		// AWS sends no body, Content-Type or Content-Length with a 204 or a
		// 304 (s3_response_handler strips them), and no Content-Type with an
		// empty 200 PUT.
		stripped := code == http.StatusNoContent || code == http.StatusNotModified
		bodiless := stripped || (w.method == http.MethodPut && code == http.StatusOK)

		if !bodiless && header.Get("Content-Type") == "" {
			header.Set("Content-Type", contentTypeXML)
		}

		if stripped {
			header.Del("Content-Type")
			header.Del("Content-Length")
		}

		trims := spellHeaders(header)

		if trims && w.http1 && w.takeConn(code) {
			return
		}
	}

	w.ResponseWriter.WriteHeader(code)
}

// spellHeaders spells the header names Header.Set canonicalized as the S3
// model and LocalStack's serializer do: Etag as ETag, and X-Amz-* in lower
// case, the case of every x-amz- header in the model. Clients that compare
// header names exactly, as the CORS snapshots do, see the difference. It
// reports whether a header value starts or ends with a space or tab, which
// net/http would trim.
func spellHeaders(header http.Header) bool {
	// The names are collected first, so the map is not changed while it is
	// ranged over, and in order, so a repeated name keeps its values in a
	// fixed order.
	var respelled []string

	for name := range header {
		if spelledName(name) != name {
			respelled = append(respelled, name)
		}
	}

	slices.Sort(respelled)

	for _, name := range respelled {
		// Both spellings can be present, so the values are joined, not replaced.
		spelled := spelledName(name)
		header[spelled] = append(header[spelled], header[name]...)
		delete(header, name)
	}

	trims := false

	for _, values := range header {
		for _, value := range values {
			trims = trims || edgeSpace(value)
		}
	}

	return trims
}

// spelledName is name as the S3 model spells it (see spellHeaders).
func spelledName(name string) string {
	switch {
	case name == "Etag":
		return "ETag"
	case strings.HasPrefix(name, "X-Amz-"):
		return strings.ToLower(name)
	default:
		return name
	}
}

func (w *responseWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}

	if w.raw != nil {
		return w.raw.write(p)
	}

	return w.ResponseWriter.Write(p) //nolint:wrapcheck // io.Writer: the error of the Write it delegates to.
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *responseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// callOf returns the request context the gateway put in ctx.
func callOf(ctx context.Context) (*request.Context, error) {
	rc, ok := request.From(ctx)
	if !ok {
		return nil, awserr.Internal("the request context is missing")
	}

	return rc, nil
}

// The errors below are the reply the client gets, so they go out as the
// generated package builds them, unwrapped.

func noSuchBucket(name string) error {
	return (&s3api.NoSuchBucket{BucketName: &name}).Err("The specified bucket does not exist")
}

func noSuchKey(key string) error {
	return (&s3api.NoSuchKey{Key: &key}).Err("The specified key does not exist.")
}

// invalidArgument names the argument, and its value when value is not nil.
func invalidArgument(message, name string, value *string) error {
	return (&s3api.InvalidArgument{ArgumentName: &name, ArgumentValue: value}).Err(message)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}
