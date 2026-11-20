package envelope_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/kylecannon/gostack/internal/service/sns/envelope"
	"github.com/stretchr/testify/require"
)

// Test_Signed_Body signs one message once and writes it for two subscribers:
// one signature serves both, each body names its own subscription in the
// UnsubscribeURL, and both verify.
func Test_Signed_Body(t *testing.T) {
	t.Parallel()

	cert := servedCert(t)
	other := testTopic + ":11111111-2222-4333-8444-555555555555"

	for _, version := range []string{"1", "2"} {
		t.Run("version "+version, func(t *testing.T) {
			t.Parallel()

			signed, err := signer.Sign(&envelope.Message{
				MessageID: testID, TopicARN: testTopic, Subject: "s", Message: "hello",
				Timestamp: testTime, SignatureVersion: version,
			}, testBase)
			require.NoError(t, err)

			signatures := make([]string, 0, 2)

			for _, sub := range []string{testSub, other} {
				_, values := parseOrdered(t, signed.Body(sub))
				require.Equal(t, testBase+"/?Action=Unsubscribe&SubscriptionArn="+sub, values[kUnsub])

				signature := stringValue(t, values, kSignature)
				delete(values, kSignature)
				verify(t, cert, values, signature)

				signatures = append(signatures, signature)
			}

			require.Equal(t, signatures[0], signatures[1])
		})
	}
}

// Test_Signer_Sign_invalidUTF8 puts invalid UTF-8 in every signed field: the
// body carries U+FFFD for each run of invalid bytes, and the signature
// verifies over the text a subscriber parses back.
func Test_Signer_Sign_invalidUTF8(t *testing.T) {
	t.Parallel()

	cert := servedCert(t)

	tests := []struct {
		name       string
		give       envelope.Message
		wantValues map[string]string
	}{
		{
			name: "notification",
			give: envelope.Message{
				MessageID: "id\xff\xfe", TopicARN: testTopic + "\xc3", SubscriptionARN: testSub + "\xff",
				Subject: "s\xfe", Message: "m\xffm", Timestamp: testTime,
			},
			wantValues: map[string]string{
				kMessageID: "id�", kTopicARN: testTopic + "�", kSubject: "s�", kMessage: "m�m",
				kUnsub: testBase + "/?Action=Unsubscribe&SubscriptionArn=" + testSub + "�",
			},
		},
		{
			name: "subscription confirmation",
			give: envelope.Message{
				Type: envelope.SubscriptionConfirmation, MessageID: testID, TopicARN: testTopic,
				Token: "tok\xff", Message: confirmMsg, Timestamp: testTime,
			},
			wantValues: map[string]string{
				kToken: "tok�",
				kSubscribe: testBase + "/?Action=ConfirmSubscription&TopicArn=" + testTopic +
					"&Token=tok�",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			body, err := signer.Body(&tt.give, testBase)
			require.NoError(t, err)

			_, values := parseOrdered(t, body)
			for key, want := range tt.wantValues {
				require.Equal(t, want, values[key], key)
			}

			signature := stringValue(t, values, kSignature)
			delete(values, kSignature)
			verify(t, cert, values, signature)
		})
	}
}

// Test_Signer_Body_null checks the members LocalStack writes as null: the
// SequenceNumber of a FIFO message that has none, and the value of an
// attribute sent without a StringValue.
func Test_Signer_Body_null(t *testing.T) {
	t.Parallel()

	body, err := signer.Body(&envelope.Message{
		Type: envelope.SubscriptionConfirmation, MessageID: testID, TopicARN: testFIFO,
		Token: testToken, Message: confirmMsg, Timestamp: testTime,
	}, testBase)
	require.NoError(t, err)

	_, values := parseOrdered(t, body)
	sequence, present := values[kSequence]
	require.True(t, present)
	require.Nil(t, sequence)

	body, err = signer.Body(&envelope.Message{
		MessageID: testID, TopicARN: testTopic, SubscriptionARN: testSub, Message: "m", Timestamp: testTime,
		Attributes: []envelope.Attribute{{Name: "a", DataType: dataTypeString}},
	}, testBase)
	require.NoError(t, err)

	_, values = parseOrdered(t, body)
	require.Equal(t, map[string]any{"a": map[string]any{kType: "String", "Value": nil}}, values[kAttributes])
}

// Test_Signer_Body_attributeOrder checks that attributes are written sorted
// by name whatever order they come in.
func Test_Signer_Body_attributeOrder(t *testing.T) {
	t.Parallel()

	body, err := signer.Body(&envelope.Message{
		MessageID: testID, TopicARN: testTopic, SubscriptionARN: testSub, Message: "m", Timestamp: testTime,
		Attributes: []envelope.Attribute{
			{Name: "zeta", DataType: dataTypeString, StringValue: new("z")},
			{Name: "Alpha", DataType: dataTypeString, StringValue: new("A")},
			{Name: "mid", DataType: "Number", StringValue: new("1")},
		},
	}, testBase)
	require.NoError(t, err)

	require.Equal(t, []string{"Alpha", "mid", "zeta"}, attributeNames(t, body))
}

// attributeNames are the names of the MessageAttributes in body, in the order
// they are written.
func attributeNames(t *testing.T, body []byte) []string {
	t.Helper()

	var envelopeMembers struct {
		MessageAttributes json.RawMessage `json:"MessageAttributes"`
	}

	require.NoError(t, json.Unmarshal(body, &envelopeMembers))

	dec := json.NewDecoder(bytes.NewReader(envelopeMembers.MessageAttributes))

	_, err := dec.Token()
	require.NoError(t, err)

	var names []string

	for dec.More() {
		tok, err := dec.Token()
		require.NoError(t, err)

		name, isString := tok.(string)
		require.True(t, isString)

		names = append(names, name)

		var skip json.RawMessage
		require.NoError(t, dec.Decode(&skip))
	}

	return names
}

// dataTypeString is the String attribute data type.
const dataTypeString = "String"
