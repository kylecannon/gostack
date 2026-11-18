package sqs_test

import (
	"encoding/base64"
	"maps"
	"strconv"

	"github.com/stretchr/testify/require"
)

// attr is a message attribute value as a test writes it. The json tags are the
// member names of the json protocol.
type attr struct {
	DataType    string `json:"DataType"`
	StringValue string `json:"StringValue,omitempty"`
	BinaryValue []byte `json:"BinaryValue,omitempty"`
}

// attrs is the MessageAttributes or MessageSystemAttributes of a call.
type attrs = map[string]attr

// entry is one entry of a batch request, by member name.
type entry = map[string]any

// received is a message of a ReceiveMessage result.
type received struct {
	id        string
	handle    string
	body      string
	bodyMD5   string
	attrsMD5  string
	system    map[string]string
	userAttrs attrs
}

// messages reads the Messages of a ReceiveMessage result.
func (r result) messages() []received {
	var found []received

	if r.query {
		for _, m := range r.member().all("Message") {
			one := received{
				id: m.text("MessageId"), handle: m.text("ReceiptHandle"), body: m.text("Body"),
				bodyMD5: m.text("MD5OfBody"), attrsMD5: m.text("MD5OfMessageAttributes"),
				system: map[string]string{}, userAttrs: attrs{},
			}

			for _, a := range m.all("Attribute") {
				one.system[a.text("Name")] = a.text("Value")
			}

			for _, a := range m.all("MessageAttribute") {
				value := a.child("Value")
				binary, err := base64.StdEncoding.DecodeString(value.text("BinaryValue"))
				require.NoError(r.t, err)

				one.userAttrs[a.text("Name")] = attr{
					DataType: value.text("DataType"), StringValue: value.text("StringValue"), BinaryValue: nonEmpty(binary),
				}
			}

			found = append(found, one)
		}

		return found
	}

	items, _ := r.json["Messages"].([]any)
	for _, item := range items {
		m, _ := item.(map[string]any)
		one := received{
			id: jsonText(m["MessageId"]), handle: jsonText(m["ReceiptHandle"]), body: jsonText(m["Body"]),
			bodyMD5: jsonText(m["MD5OfBody"]), attrsMD5: jsonText(m["MD5OfMessageAttributes"]),
			system: map[string]string{}, userAttrs: attrs{},
		}

		system, _ := m["Attributes"].(map[string]any)
		for name, value := range system {
			one.system[name] = jsonText(value)
		}

		user, _ := m["MessageAttributes"].(map[string]any)
		for name, value := range user {
			fields, _ := value.(map[string]any)
			binary, err := base64.StdEncoding.DecodeString(jsonText(fields["BinaryValue"]))
			require.NoError(r.t, err)

			one.userAttrs[name] = attr{
				DataType: jsonText(fields["DataType"]), StringValue: jsonText(fields["StringValue"]), BinaryValue: nonEmpty(binary),
			}
		}

		found = append(found, one)
	}

	return found
}

func nonEmpty(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}

	return b
}

// jsonText formats a scalar of a decoded json document the way the query form
// writes it.
func jsonText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		return strconv.FormatInt(int64(typed), 10)
	default:
		return ""
	}
}

// entries reads the entries of one list of a batch result, each as its scalar
// members by name. The json list and the query element have different names.
func (r result) entries(jsonName, element string) []map[string]string {
	var found []map[string]string

	if r.query {
		for _, e := range r.member().all(element) {
			fields := map[string]string{}
			for _, child := range e.Children {
				fields[child.XMLName.Local] = child.Text
			}

			found = append(found, fields)
		}

		return found
	}

	items, _ := r.json[jsonName].([]any)
	for _, item := range items {
		m, _ := item.(map[string]any)
		fields := map[string]string{}

		for name, value := range m {
			fields[name] = jsonText(value)
		}

		found = append(found, fields)
	}

	return found
}

// ids returns the Id member of each entry.
func ids(entries []map[string]string) []string {
	found := make([]string, 0, len(entries))
	for _, e := range entries {
		found = append(found, e["Id"])
	}

	return found
}

// send sends a message and returns the result, failing the test when it fails.
func (c caller) send(queueURL, body string, extra map[string]any) result {
	c.s.t.Helper()

	params := map[string]any{paramQueueURL: queueURL, paramMessageBody: body}
	maps.Copy(params, extra)

	res := c.call(opSendMessage, params)
	require.True(c.s.t, res.ok(), "SendMessage: %s %s", res.errorCode(), res.errorMessage())

	return res
}

// receive receives with the given parameters and returns the messages.
func (c caller) receive(queueURL string, extra map[string]any) []received {
	c.s.t.Helper()

	params := map[string]any{paramQueueURL: queueURL}
	maps.Copy(params, extra)

	res := c.call(opReceiveMessage, params)
	require.True(c.s.t, res.ok(), "ReceiveMessage: %s %s", res.errorCode(), res.errorMessage())

	return res.messages()
}
