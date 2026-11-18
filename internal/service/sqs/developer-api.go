package sqs

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/kylecannon/gostack/internal/account"
	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/kylecannon/gostack/internal/request"
)

// The developer API lists the messages of a queue without receiving them. It
// is LocalStack's services/sqs/developer_api.py: ReceiveMessage-shaped
// responses, a dummy receipt handle, and two flags that add the messages a
// receive would not return.
const (
	// developerCanonicalPrefix is where the developer endpoints always live.
	developerCanonicalPrefix = "/_gostack/services"
	// developerCompatPrefix is LocalStack's name for them, mounted unless
	// Settings.DisableCompatPaths is set.
	developerCompatPrefix = "/_aws"

	developerMessagesPath = "/sqs/messages"
	developerQueuePath    = developerMessagesPath + "/{region}/{account_id}/{queue_name}"

	// fieldQueueURL and fieldAction are the form and query names of a
	// ReceiveMessage request.
	fieldQueueURL = "QueueUrl"
	fieldAction   = "Action"

	// backdoorReceiptHandle is the receipt handle of every listed message. It
	// names nothing, so a message cannot be deleted through it.
	backdoorReceiptHandle = "SQS/BACKDOOR/ACCESS"

	flagShowInvisible = "ShowInvisible"
	flagShowDelayed   = "ShowDelayed"

	// attrIsVisible and attrIsDelayed are the two attributes the flags add to
	// a listed message. They are not part of SQS.
	attrIsVisible = "IsVisible"
	attrIsDelayed = "IsDelayed"

	mimeXML    = "text/xml"
	mimeAppXML = "application/xml"
	mimeJSON   = "application/json"
	headerType = "Content-Type"
	headerAcc  = "Accept"

	// The specificity of an Accept entry that matches a type: */* is the
	// loosest and the exact type the tightest.
	matchAny   = 0
	matchGroup = 1
	matchExact = 2
	noMatch    = -1

	// qualityScale is the quality 1 of an Accept entry, in thousandths.
	qualityScale = 1000
)

// supportedMimes are the response types of a query-protocol answer, in the
// order LocalStack's serializer prefers them when the caller has no preference.
var supportedMimes = []string{mimeXML, mimeAppXML, mimeJSON}

// peeked is a message as a queue holds it, with the state a listing reports.
type peeked struct {
	msg           *message
	receiveCount  int
	firstReceived time.Time
	// inflight is a message a receiver took and whose visibility timeout is
	// not over.
	inflight bool
	// delayed is a message sent with a delay that is not over.
	delayed bool
}

// listing says which messages beyond the visible ones a listing includes.
type listing struct {
	// invisible adds the messages in flight, and the IsVisible attribute.
	invisible bool
	// delayed adds the delayed messages, and the IsDelayed attribute.
	delayed bool
}

// peeker is a Contents that can list its messages without any side effect.
// The developer API needs it, and a queue whose Contents lacks it answers 501.
type peeker interface {
	// peek lists the visible messages in sending order, preceded by the
	// in-flight ones when the listing has them and followed by the delayed
	// ones when it has those. The queue's lock is held.
	peek(what listing) []peeked
}

var _ peeker = (*standardContents)(nil)

// peek is peeker for a standard queue. Like Counts it first moves the messages
// whose time has come, which is time passing and not a change by the caller.
func (c *standardContents) peek(what listing) []peeked {
	c.refresh(time.Now())

	visible := slices.Clone(c.visible.items)

	var invisible, delayed []*entry

	for _, e := range c.deadlines.items {
		if e.state == stateDelayed {
			delayed = append(delayed, e)
		} else {
			invisible = append(invisible, e)
		}
	}

	bySeq := func(a, b *entry) int { return cmp.Compare(a.seq, b.seq) }
	slices.SortFunc(invisible, bySeq)
	slices.SortFunc(visible, bySeq)
	slices.SortFunc(delayed, bySeq)

	var chosen []*entry

	if what.invisible {
		chosen = append(chosen, invisible...)
	}

	chosen = append(chosen, visible...)

	if what.delayed {
		chosen = append(chosen, delayed...)
	}

	out := make([]peeked, len(chosen))
	for i, e := range chosen {
		out[i] = peeked{
			msg:           e.msg,
			receiveCount:  e.receiveCount,
			firstReceived: e.firstReceived,
			inflight:      e.state == stateInflight,
			delayed:       e.state == stateDelayed,
		}
	}

	return out
}

// mountDeveloperAPI adds the developer routes: always under /_gostack/services,
// and under /_aws too unless the settings turn the LocalStack names off. It is
// called when the router is built, so the setting is read once.
func (p *Provider) mountDeveloperAPI(r chi.Router) {
	prefixes := []string{developerCanonicalPrefix}
	if !p.settings().DisableCompatPaths {
		prefixes = append(prefixes, developerCompatPrefix)
	}

	for _, prefix := range prefixes {
		r.Get(prefix+developerMessagesPath, p.listMessages)
		r.Post(prefix+developerMessagesPath, p.listMessages)
		r.Get(prefix+developerQueuePath, p.listQueueMessages)
		r.Post(prefix+developerQueuePath, p.listQueueMessages)
	}
}

// devCall is one call of the developer API: the request context it answers in
// and the form of its response.
type devCall struct {
	w  http.ResponseWriter
	r  *http.Request
	rc *request.Context
	// mime is the type of a query-protocol response.
	mime string
}

func (p *Provider) beginDevCall(w http.ResponseWriter, r *http.Request) *devCall {
	// An unparsable form is read as far as it parses, as werkzeug reads it.
	_ = r.ParseForm()

	identity := account.Resolve(r.Header.Get("Authorization"), p.settings().ParityAWSAccessKeyID)

	rc := &request.Context{
		Service:   sqsapi.Metadata.Name,
		Operation: opReceiveMessage,
		Protocol:  protocol.KindQuery,
		AccountID: identity.AccountID,
		Region:    identity.Region,
		RequestID: newRequestID(),
	}

	// The json protocol is chosen by the exact Content-Type, as in LocalStack.
	if r.Header.Get(headerType) == jsonContentType {
		rc.Protocol = protocol.KindJSON
	}

	return &devCall{w: w, r: r, rc: rc, mime: negotiateMime(r)}
}

// listMessages is /sqs/messages: the queue is the QueueUrl of the call, as in
// a ReceiveMessage request or as a query or form parameter.
func (p *Provider) listMessages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	call := p.beginDevCall(w, r)

	queueURL, err := call.queueURL()
	if err != nil {
		call.fail(ctx, err)

		return
	}

	if queueURL == "" {
		call.fail(ctx, sqsapi.NewQueueDoesNotExist(""))

		return
	}

	loc, ok := parseQueueURL(queueURL)
	if !ok {
		call.fail(ctx, invalidAddress())

		return
	}

	if loc.region == "" {
		loc.region = call.rc.Region
	}

	p.listQueue(ctx, call, loc)
}

// listQueueMessages is /sqs/messages/<region>/<account>/<queue>.
func (p *Provider) listQueueMessages(w http.ResponseWriter, r *http.Request) {
	p.listQueue(r.Context(), p.beginDevCall(w, r), location{
		account: chi.URLParam(r, "account_id"),
		region:  chi.URLParam(r, "region"),
		name:    chi.URLParam(r, "queue_name"),
	})
}

// listQueue answers with the messages of the queue loc names.
func (p *Provider) listQueue(ctx context.Context, call *devCall, loc location) {
	q, found := p.states.Get(loc.account, loc.region).find(loc.name)
	if !found {
		call.fail(ctx, sqsapi.NewQueueDoesNotExist(""))

		return
	}

	what := listing{invisible: flagSet(call.r, flagShowInvisible), delayed: flagSet(call.r, flagShowDelayed)}

	q.mu.Lock()

	lister, ok := q.contents.(peeker)
	if !ok {
		q.mu.Unlock()
		call.fail(ctx, awserr.NotImplemented(sqsapi.Metadata.Name, "ListMessages"))

		return
	}

	listed := lister.peek(what)

	q.mu.Unlock()

	out := &sqsapi.ReceiveMessageOutput{Messages: make([]sqsapi.Message, len(listed))}

	for i := range listed {
		out.Messages[i] = listedMessage(&listed[i], what)
	}

	call.respond(ctx, &previewHandler{out: out})
}

// listedMessage is what a receiver would see of a message, with every
// attribute and the dummy receipt handle, and the flags' own attributes.
func listedMessage(l *peeked, what listing) sqsapi.Message {
	first := l.firstReceived
	if first.IsZero() {
		// A message nobody received reports ApproximateFirstReceiveTimestamp 0.
		first = time.UnixMilli(0)
	}

	d := delivery{msg: l.msg, handle: backdoorReceiptHandle, receiveCount: l.receiveCount, firstReceived: first}
	out := toAPIMessage(&d, []string{filterAll}, []string{filterAll})

	if out.Attributes == nil {
		out.Attributes = map[string]string{}
	}

	if what.invisible {
		out.Attributes[attrIsVisible] = strconv.FormatBool(!l.inflight)
	}

	if what.delayed {
		out.Attributes[attrIsDelayed] = strconv.FormatBool(l.delayed)
	}

	return out
}

// flagSet reads a flag of the query string or the form, which LocalStack reads
// as true for "true" in any case and for "1".
func flagSet(r *http.Request, name string) bool {
	switch strings.ToLower(requestValue(r, name)) {
	case attrValueTrue, "1":
		return true
	default:
		return false
	}
}

// requestValue is werkzeug's request.values.get: the query string wins over the
// form, and a key given with an empty value is that empty value.
func requestValue(r *http.Request, name string) string {
	if query := r.URL.Query(); query.Has(name) {
		return query.Get(name)
	}

	return r.PostForm.Get(name)
}

// queueURL reads the QueueUrl of a call. A call that says it is from an AWS
// client (an x-amz- or a form content type) is parsed as a ReceiveMessage
// request and must be one. Any other call carries the QueueUrl as a query or
// form parameter.
func (c *devCall) queueURL() (string, *awserr.Error) {
	mediaType, _, _ := mime.ParseMediaType(c.r.Header.Get(headerType))
	if !strings.Contains(mediaType, "x-amz-") && !strings.Contains(mediaType, "x-www-form-urlencoded") {
		return requestValue(c.r, fieldQueueURL), nil
	}

	if c.rc.Protocol == protocol.KindJSON {
		target := c.r.Header.Get("X-Amz-Target")
		if target[strings.LastIndex(target, ".")+1:] != opReceiveMessage {
			return "", onlyReceiveMessage()
		}

		body, err := io.ReadAll(c.r.Body)
		if err != nil {
			return "", &awserr.Error{
				Code: "SerializationException", Message: "The request body could not be read.",
				StatusCode: http.StatusBadRequest, Fault: awserr.FaultSender,
			}
		}

		in, decodeErr := awsjson.Decode[sqsapi.ReceiveMessageInput](body)
		if decodeErr != nil {
			return "", decodeErr
		}

		return deref(in.QueueUrl), nil
	}

	if c.r.Form.Get(fieldAction) != opReceiveMessage {
		return "", onlyReceiveMessage()
	}

	return c.r.Form.Get(fieldQueueURL), nil
}

// previewHandler is the Handler the generated dispatcher calls to write a
// listing or an error in the form of the caller's protocol.
type previewHandler struct {
	sqsapi.Unimplemented

	out *sqsapi.ReceiveMessageOutput
	err *awserr.Error
}

// ReceiveMessage returns what the developer API already worked out.
func (h *previewHandler) ReceiveMessage(
	context.Context, *sqsapi.ReceiveMessageInput,
) (*sqsapi.ReceiveMessageOutput, error) {
	if h.err != nil {
		return nil, h.err
	}

	return h.out, nil
}

func (c *devCall) fail(ctx context.Context, err *awserr.Error) {
	c.respond(ctx, &previewHandler{err: err})
}

// respond has the generated dispatcher write what h holds. The dispatcher
// reads a request of its own, so it gets one that carries no input.
func (c *devCall) respond(ctx context.Context, h *previewHandler) {
	req := c.r.Clone(ctx)
	req.Body = io.NopCloser(strings.NewReader("{}"))
	req.Form, req.PostForm = url.Values{}, url.Values{}
	c.rc.Request = req.WithContext(request.With(ctx, c.rc))

	if c.rc.Protocol != protocol.KindQuery || c.mime == mimeXML {
		sqsapi.Serve(h, c.w, c.rc) //nolint:contextcheck // the generated Serve takes the context from rc.Request.

		return
	}

	// The query writer only writes text/xml. A caller that asked for another
	// type gets the same document in it.
	held := &heldResponse{header: http.Header{}}
	sqsapi.Serve(h, held, c.rc) //nolint:contextcheck // the generated Serve takes the context from rc.Request.

	out := c.w.Header()
	maps.Copy(out, held.header)

	body := held.body.Bytes()

	if c.mime == mimeJSON {
		converted, err := xmlToJSON(body)
		if err == nil {
			body = converted
		}
	}

	out.Set(headerType, c.mime)

	status := held.status
	if status == 0 {
		status = http.StatusOK
	}

	c.w.WriteHeader(status)
	_, _ = c.w.Write(body)
}

// heldResponse keeps a response so it can be rewritten before it is sent.
type heldResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (h *heldResponse) Header() http.Header { return h.header }

func (h *heldResponse) WriteHeader(status int) { h.status = status }

func (h *heldResponse) Write(p []byte) (int, error) {
	// A bytes.Buffer never fails to write.
	n, _ := h.body.Write(p)

	return n, nil
}

// invalidAddress is LocalStack's InvalidAddress. LocalStack sends it with a
// code and a fault and no message.
func invalidAddress() *awserr.Error {
	return &awserr.Error{
		Code:       "InvalidAddress",
		StatusCode: http.StatusNotFound,
		Fault:      awserr.FaultSender,
	}
}

// onlyReceiveMessage is LocalStack's answer to any other operation: a
// CommonServiceException that the query protocol names InvalidRequest, with no
// sender fault.
func onlyReceiveMessage() *awserr.Error {
	return &awserr.Error{
		Code:       "CommonServiceException",
		Message:    "This endpoint only accepts ReceiveMessage calls",
		StatusCode: http.StatusBadRequest,
		QueryCode:  "InvalidRequest",
	}
}

// negotiateMime is LocalStack's _get_mime_type: the best of the types a
// query-protocol answer can take for the Accept header, or for the Content-Type
// of the call when there is no Accept header worth reading.
func negotiateMime(r *http.Request) string {
	accept := r.Header.Get(headerAcc)

	if accept == "" || accept == "*/*" {
		contentType := r.Header.Get(headerType)
		if contentType == "" {
			return supportedMimes[0]
		}

		accept = contentType + ", */*"
	}

	best, bestQuality, bestSpecificity := supportedMimes[0], -1, noMatch

	for _, supported := range supportedMimes {
		for _, e := range parseAccept(accept) {
			specificity := matchSpecificity(e.media, supported)
			if e.quality <= 0 || specificity == noMatch {
				continue
			}

			if e.quality > bestQuality || (e.quality == bestQuality && specificity > bestSpecificity) {
				best, bestQuality, bestSpecificity = supported, e.quality, specificity
			}
		}
	}

	return best
}

// acceptEntry is one media range of an Accept header, with its quality in
// thousandths.
type acceptEntry struct {
	media   string
	quality int
}

// parseAccept reads the media ranges of an Accept header. A range with no q
// parameter, or one that is not a number, has the quality 1.
func parseAccept(accept string) []acceptEntry {
	var entries []acceptEntry

	for item := range strings.SplitSeq(accept, ",") {
		media, params, _ := strings.Cut(item, ";")
		entries = append(entries, acceptEntry{
			media:   strings.ToLower(strings.TrimSpace(media)),
			quality: acceptQuality(params),
		})
	}

	return entries
}

// acceptQuality reads the q parameter of a media range's parameters.
func acceptQuality(params string) int {
	for param := range strings.SplitSeq(params, ";") {
		key, value, found := strings.Cut(strings.TrimSpace(param), "=")
		if !found || key != "q" {
			continue
		}

		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			break
		}

		return int(parsed * qualityScale)
	}

	return qualityScale
}

// matchSpecificity tells how an Accept entry matches a type: exactly, by its
// group ("text/*"), by "*/*", or not at all.
func matchSpecificity(entry, supported string) int {
	switch {
	case entry == supported:
		return matchExact
	case entry == "*/*" || entry == "*":
		return matchAny
	case strings.HasSuffix(entry, "/*") && strings.HasPrefix(supported, strings.TrimSuffix(entry, "*")):
		return matchGroup
	default:
		return noMatch
	}
}

// xmlNode is an element of a document read by xmlToJSON.
type xmlNode struct {
	name     string
	children []*xmlNode
	text     strings.Builder
}

// xmlToJSON converts an XML document to the JSON that LocalStack answers with
// when a query-protocol caller asks for application/json, which is what
// xmltodict makes of it: the root element names the only key, an element with
// children is an object, a repeated child is an array, an element with only
// text is a string, an empty element is null, and namespaces and attributes
// are dropped.
func xmlToJSON(document []byte) ([]byte, error) {
	decoder := xml.NewDecoder(bytes.NewReader(document))

	var (
		root  *xmlNode
		stack []*xmlNode
	)

	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, err //nolint:wrapcheck // the caller falls back to the XML.
		}

		switch t := token.(type) {
		case xml.StartElement:
			node := &xmlNode{name: t.Name.Local}

			if len(stack) == 0 {
				root = node
			} else {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, node)
			}

			stack = append(stack, node)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text.Write(t)
			}
		default:
			// Declarations, comments and processing instructions carry nothing.
		}
	}

	if root == nil {
		return nil, io.ErrUnexpectedEOF
	}

	out := []byte{'{'}
	out = appendJSONString(out, root.name)
	out = append(out, ':')
	out = root.appendJSON(out)

	return append(out, '}'), nil
}

func (n *xmlNode) appendJSON(out []byte) []byte {
	if len(n.children) == 0 {
		if n.text.Len() == 0 {
			return append(out, "null"...)
		}

		return appendJSONString(out, n.text.String())
	}

	var order []string

	groups := map[string][]*xmlNode{}

	for _, child := range n.children {
		if _, seen := groups[child.name]; !seen {
			order = append(order, child.name)
		}

		groups[child.name] = append(groups[child.name], child)
	}

	out = append(out, '{')

	for i, name := range order {
		if i > 0 {
			out = append(out, ',')
		}

		out = appendJSONString(out, name)
		out = append(out, ':')

		group := groups[name]
		if len(group) == 1 {
			out = group[0].appendJSON(out)

			continue
		}

		out = append(out, '[')

		for j, child := range group {
			if j > 0 {
				out = append(out, ',')
			}

			out = child.appendJSON(out)
		}

		out = append(out, ']')
	}

	return append(out, '}')
}

func appendJSONString(out []byte, s string) []byte {
	encoded, err := json.Marshal(s)
	if err != nil {
		// A string always marshals; this keeps the document valid regardless.
		return append(out, `""`...)
	}

	return append(out, encoded...)
}
