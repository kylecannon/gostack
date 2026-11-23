package sqs

import (
	"cmp"
	"context"
	"errors"
	"io"
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
	"github.com/kylecannon/gostack/internal/protocol/query"
	"github.com/kylecannon/gostack/internal/request"
)

// The developer API lists the messages of a queue without receiving them. It
// is LocalStack's services/sqs/developer_api.py: ReceiveMessage-shaped
// responses, a dummy receipt handle, and two flags that add the messages a
// receive would not return.
const (
	// The routes are relative to the prefix the gateway scopes DeveloperMount
	// to: /_gostack/services/sqs and, unless GOSTACK_COMPAT_PATHS=0, /_aws/sqs.
	developerMessagesPath = "/messages"
	developerQueuePath    = developerMessagesPath + "/{region}/{account_id}/{queue_name}"

	// fieldQueueURL and fieldAction are the form and query names of a
	// ReceiveMessage request.
	fieldQueueURL = "QueueUrl"
	fieldAction   = "Action"

	// opReceiveMessage is the one operation the developer API answers.
	opReceiveMessage = "ReceiveMessage"

	// backdoorReceiptHandle is the receipt handle of every listed message. It
	// names nothing, so a message cannot be deleted through it.
	backdoorReceiptHandle = "SQS/BACKDOOR/ACCESS"

	flagShowInvisible = "ShowInvisible"
	flagShowDelayed   = "ShowDelayed"

	// attrIsVisible and attrIsDelayed are the two attributes the flags add to
	// a listed message. They are not part of SQS.
	attrIsVisible = "IsVisible"
	attrIsDelayed = "IsDelayed"
)

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

	return peekedOf(chosen)
}

var _ peeker = (*fifoContents)(nil)

// peek is peeker for a FIFO queue, in LocalStack's order: the messages in
// flight, then the visible ones of the groups a receive locked, which only a
// listing with the invisible ones has and which it reports as visible, then the
// visible ones of the other groups, group by group in the order they became
// ready, and the delayed ones last.
func (c *fifoContents) peek(what listing) []peeked {
	c.refresh(time.Now())

	bySeq := func(a, b *entry) int { return cmp.Compare(a.seq, b.seq) }

	var inflight, held, delayed []*entry

	for _, g := range c.groups {
		for e := g.head; e != nil; e = e.fifo.next {
			inflight = append(inflight, e)
		}

		if g.locked {
			held = append(held, g.visible.items...)
		}
	}

	for _, e := range c.deadlines.items {
		if e.state == stateDelayed {
			delayed = append(delayed, e)
		}
	}

	slices.SortFunc(inflight, bySeq)
	slices.SortFunc(held, bySeq)
	slices.SortFunc(delayed, bySeq)

	var chosen []*entry

	if what.invisible {
		chosen = append(chosen, inflight...)
		chosen = append(chosen, held...)
	}

	for _, g := range c.ready.items[c.ready.head:] {
		if g.queued {
			open := slices.Clone(g.visible.items)
			slices.SortFunc(open, bySeq)
			chosen = append(chosen, open...)
		}
	}

	if what.delayed {
		chosen = append(chosen, delayed...)
	}

	return peekedOf(chosen)
}

// peekedOf reports the entries in the order given.
func peekedOf(chosen []*entry) []peeked {
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

// mountDeveloperAPI adds the developer routes. It is the DeveloperMount of the
// registration: the gateway calls it for /_gostack/services/sqs, and for
// /_aws/sqs unless GOSTACK_COMPAT_PATHS=0.
func (p *Provider) mountDeveloperAPI(r chi.Router) {
	r.Get(developerMessagesPath, p.listMessages)
	r.Post(developerMessagesPath, p.listMessages)
	r.Get(developerQueuePath, p.listQueueMessages)
	r.Post(developerQueuePath, p.listQueueMessages)
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
	// A form that cannot be read has no parameters, so the call finds no queue.
	_, err := query.RequestForm(r)
	if err != nil {
		r.Form = url.Values{}
	}

	identity := account.Resolve(r, p.settings().ParityAWSAccessKeyID)

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
	q, found := p.stateAt(loc.account, loc.region).find(loc.name)
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
// form, and a key given with an empty value is that empty value. The form
// RequestForm read lists the query string's pairs first.
func requestValue(r *http.Request, name string) string {
	return r.Form.Get(name)
}

// unreadableBody is the answer to a json body that could not be read: the
// throttling error when the bodies in flight left it no room.
func unreadableBody(err error) *awserr.Error {
	if errors.Is(err, protocol.ErrBusy) {
		return protocol.ThrottledJSON()
	}

	return &awserr.Error{
		Code: "SerializationException", Message: "The request body could not be read.",
		StatusCode: http.StatusBadRequest, Fault: awserr.FaultSender,
	}
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

		body, err := protocol.ReadRequest(c.r)
		if err != nil {
			return "", unreadableBody(err)
		}

		if protocol.OverBody(len(body)) {
			return "", query.MalformedQueryString()
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

	held := &heldResponse{header: http.Header{}}
	sqsapi.Serve(h, held, c.rc) //nolint:contextcheck // the generated Serve takes the context from rc.Request.
	held.sendAs(c.w, c.mime)
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
