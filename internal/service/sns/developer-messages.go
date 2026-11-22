package sns

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"
	"github.com/kylecannon/gostack/internal/account"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/kylecannon/gostack/internal/service/sns/envelope"
)

// The developer endpoints that list what SNS recorded instead of sending, and
// the one that opts a number out of SMS. They are LocalStack's
// SNSServicePlatformEndpointMessagesApiResource, SNSServiceSMSMessagesApiResource
// and SNSServiceSMSPhoneOptOutResource, under /_gostack/services and /_aws.
const (
	endpointMessagesPath = "/sns/platform-endpoint-messages"
	smsMessagesPath      = "/sns/sms-messages"
	phoneOptOutsPath     = "/sns/phone-opt-outs"

	// maxOptOutBody is the most bytes of an opt-out request read.
	maxOptOutBody = 1 << 20
)

// mountMessages adds the routes of the recorded messages and the opt-out
// under prefix.
func (p *Provider) mountMessages(r chi.Router, prefix string) {
	r.Get(prefix+endpointMessagesPath, p.listEndpointMessages)
	r.Delete(prefix+endpointMessagesPath, p.deleteEndpointMessages)
	r.Get(prefix+smsMessagesPath, p.listSMSMessages)
	r.Delete(prefix+smsMessagesPath, p.deleteSMSMessages)
	r.Post(prefix+phoneOptOutsPath, p.optOutPhoneNumber)
}

// messagesScope is the State a call of the endpoints asks about: the account
// and region of its parameters, us-east-1 of the default account where it names
// none.
type messagesScope struct {
	account, region string
}

// endpointScopeOf reads the scope of a call on an endpoint's messages. The
// endpointArn of the call, when it has one, is the scope and the filter, as in
// LocalStack; an ARN of fewer than six fields, which crashes LocalStack, is
// reported as false.
func endpointScopeOf(r *http.Request) (messagesScope, string, bool) {
	arn := r.URL.Query().Get("endpointArn")
	if arn == "" {
		return scopeOf(r), "", true
	}

	parts, ok := parseARN(arn)

	return messagesScope{account: parts.account, region: parts.region}, arn, ok
}

// scopeOf reads the account and region of a call.
func scopeOf(r *http.Request) messagesScope {
	q := r.URL.Query()
	scope := messagesScope{account: account.Default, region: account.DefaultRegion}
	if q.Has("accountId") {
		scope.account = q.Get("accountId")
	}

	if q.Has("region") {
		scope.region = q.Get("region")
	}

	return scope
}

func (p *Provider) listEndpointMessages(w http.ResponseWriter, r *http.Request) {
	scope, arn, ok := endpointScopeOf(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "The provided endpointArn is invalid")

		return
	}

	st := p.stateAt(scope.account, scope.region)

	st.mu.RLock()
	defer st.mu.RUnlock()

	b := append(make([]byte, 0, jsonSizeHint), `{"platform_endpoint_messages":{`...)

	if arn != "" {
		b = appendEndpointList(b, arn, st.endpointMessages[arn])
	} else {
		for i, endpointARN := range slices.Sorted(maps.Keys(st.endpointMessages)) {
			if i > 0 {
				b = append(b, ',')
			}

			b = appendEndpointList(b, endpointARN, st.endpointMessages[endpointARN])
		}
	}

	b = append(b, `},"region":`...)
	b = awsjson.AppendQuote(b, scope.region)
	b = append(b, '}')

	writeJSON(w, http.StatusOK, b)
}

func (p *Provider) deleteEndpointMessages(w http.ResponseWriter, r *http.Request) {
	scope, arn, ok := endpointScopeOf(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "The provided endpointArn is invalid")

		return
	}

	st := p.stateAt(scope.account, scope.region)

	st.mu.Lock()
	if arn != "" {
		delete(st.endpointMessages, arn)
	} else {
		clear(st.endpointMessages)
	}
	st.mu.Unlock()

	p.markChanged()
	w.WriteHeader(http.StatusNoContent)
}

func (p *Provider) listSMSMessages(w http.ResponseWriter, r *http.Request) {
	scope := scopeOf(r)
	number := r.URL.Query().Get("phoneNumber")
	st := p.stateAt(scope.account, scope.region)

	st.mu.RLock()
	defer st.mu.RUnlock()

	b := append(make([]byte, 0, jsonSizeHint), `{"sms_messages":{`...)

	// The messages are grouped by number in the order of each number's first
	// message, as LocalStack's dict keeps them.
	var numbers []string

	byNumber := make(map[string][]*RecordedMessage)

	for i := range st.smsMessages {
		m := &st.smsMessages[i]
		if number != "" && m.PhoneNumber != number {
			continue
		}

		if _, seen := byNumber[m.PhoneNumber]; !seen {
			numbers = append(numbers, m.PhoneNumber)
		}

		byNumber[m.PhoneNumber] = append(byNumber[m.PhoneNumber], m)
	}

	if number != "" && len(numbers) == 0 {
		numbers = append(numbers, number)
	}

	for i, n := range numbers {
		if i > 0 {
			b = append(b, ',')
		}

		b = awsjson.AppendQuote(b, n)
		b = append(b, ":["...)

		for j, m := range byNumber[n] {
			if j > 0 {
				b = append(b, ',')
			}

			b = appendSMSMessage(b, m)
		}

		b = append(b, ']')
	}

	b = append(b, `},"region":`...)
	b = awsjson.AppendQuote(b, scope.region)
	b = append(b, '}')

	writeJSON(w, http.StatusOK, b)
}

func (p *Provider) deleteSMSMessages(w http.ResponseWriter, r *http.Request) {
	scope := scopeOf(r)
	number := r.URL.Query().Get("phoneNumber")
	st := p.stateAt(scope.account, scope.region)

	st.mu.Lock()
	if number != "" {
		st.smsMessages = slices.DeleteFunc(st.smsMessages, func(m RecordedMessage) bool { return m.PhoneNumber == number })
	} else {
		st.smsMessages = nil
	}
	st.mu.Unlock()

	p.markChanged()
	w.WriteHeader(http.StatusNoContent)
}

// optOutPhoneNumber puts a number on an account's opt-out list, from a JSON
// body {"phoneNumber": ..., "accountId": ...} where the account defaults to
// 000000000000. A body that is not a JSON object is a 400, where LocalStack
// crashes. The answer is an empty 200, as LocalStack's.
func (p *Provider) optOutPhoneNumber(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PhoneNumber *string `json:"phoneNumber"`
		AccountID   *string `json:"accountId"`
	}

	data, err := io.ReadAll(io.LimitReader(r.Body, maxOptOutBody+1))
	if err == nil && len(data) <= maxOptOutBody {
		// A body that is JSON null is an empty one, as LocalStack's `or {}`.
		err = json.Unmarshal(data, &body)
	}

	if err != nil || len(data) > maxOptOutBody {
		writeJSONError(w, http.StatusBadRequest, "The request body is not a JSON object")

		return
	}

	acct := account.Default
	if body.AccountID != nil {
		acct = *body.AccountID
	}

	if body.PhoneNumber != nil && *body.PhoneNumber != "" {
		a := p.accounts.Get(acct, accountRegion)

		a.mu.Lock()
		a.optedOut[*body.PhoneNumber] = struct{}{}
		a.mu.Unlock()

		p.markChanged()
	}

	w.WriteHeader(http.StatusOK)
}

// jsonSizeHint is the room a listing starts with.
const jsonSizeHint = 512

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	// An error is a write to a client that is gone, and there is no one left
	// to tell.
	_, err := w.Write(body) //nolint:gosec // JSON with its own content type, built here
	if err != nil {
		return
	}
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, append(awsjson.AppendQuote([]byte(`{"error":`), message), '}'))
}

// appendEndpointList writes `"<arn>":[...]`.
func appendEndpointList(b []byte, arn string, msgs []RecordedMessage) []byte {
	b = awsjson.AppendQuote(b, arn)
	b = append(b, ":["...)

	for i := range msgs {
		if i > 0 {
			b = append(b, ',')
		}

		b = appendEndpointMessage(b, &msgs[i])
	}

	return append(b, ']')
}

// appendEndpointMessage writes a message of an endpoint's list with the members
// LocalStack's platform endpoint listing returns: TargetArn, TopicArn (empty
// for a direct publish), the message, its attributes, structure and subject,
// and MessageId when the message has one.
func appendEndpointMessage(b []byte, m *RecordedMessage) []byte {
	b = append(b, `{"TargetArn":`...)
	b = awsjson.AppendQuote(b, m.TargetARN)
	b = append(b, `,"TopicArn":`...)
	b = awsjson.AppendQuote(b, m.TopicARN)
	b = appendBody(b, m)

	if m.MessageID != "" {
		b = append(b, `,"MessageId":`...)
		b = awsjson.AppendQuote(b, m.MessageID)
	}

	return append(b, '}')
}

// appendSMSMessage writes a message of the SMS listing.
func appendSMSMessage(b []byte, m *RecordedMessage) []byte {
	b = append(b, `{"PhoneNumber":`...)
	b = awsjson.AppendQuote(b, m.PhoneNumber)
	b = appendNullable(b, "TopicArn", m.TopicARN)
	b = appendNullable(b, "SubscriptionArn", m.SubscriptionARN)
	b = append(b, `,"MessageId":`...)
	b = awsjson.AppendQuote(b, m.MessageID)
	b = appendBody(b, m)

	return append(b, '}')
}

// appendBody writes the members both listings end with: the message, which a
// MessageStructure=json message has as the JSON text of the body, its
// attributes, structure and subject, null where it has none.
func appendBody(b []byte, m *RecordedMessage) []byte {
	b = append(b, `,"Message":`...)

	if m.MessageStructure == structureJSON {
		// LocalStack encodes the body again, as Python's json.dumps does.
		b = awsjson.AppendQuote(b, string(envelope.AppendJSONDumps(nil, m.Message)))
	} else {
		b = awsjson.AppendQuote(b, m.Message)
	}

	b = append(b, `,"MessageAttributes":`...)

	if len(m.MessageAttributes) == 0 {
		b = append(b, "null"...)
	} else {
		b = appendAttributes(b, m.MessageAttributes)
	}

	b = appendNullable(b, "MessageStructure", m.MessageStructure)

	return appendNullable(b, "Subject", m.Subject)
}

// appendNullable writes `,"name":"value"`, or null for an empty value.
func appendNullable(b []byte, name, value string) []byte {
	b = append(b, ',', '"')
	b = append(b, name...)
	b = append(b, '"', ':')

	if value == "" {
		return append(b, "null"...)
	}

	return awsjson.AppendQuote(b, value)
}

// appendAttributes writes message attributes by name: the data type and the
// string value, or a binary value in base64.
func appendAttributes(b []byte, attrs map[string]MessageAttribute) []byte {
	b = append(b, '{')

	for i, name := range slices.Sorted(maps.Keys(attrs)) {
		if i > 0 {
			b = append(b, ',')
		}

		a := attrs[name]
		b = awsjson.AppendQuote(b, name)
		b = append(b, `:{"DataType":`...)
		b = awsjson.AppendQuote(b, a.DataType)

		if a.BinaryValue != nil {
			b = append(b, `,"BinaryValue":"`...)
			b = base64.StdEncoding.AppendEncode(b, a.BinaryValue)
			b = append(b, '"')
		} else {
			b = append(b, `,"StringValue":`...)
			b = awsjson.AppendQuote(b, a.StringValue)
		}

		b = append(b, '}')
	}

	return append(b, '}')
}
