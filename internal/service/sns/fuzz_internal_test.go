package sns

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzParseTopicARN checks that a topic ARN either splits into six fields that
// join back to the input, or is refused for having fewer than six.
func FuzzParseTopicARN(f *testing.F) {
	for _, seed := range []string{
		"arn:aws:sns:us-east-1:000000000000:topic",
		"arn:aws:sns:us-east-1:000000000000:topic.fifo",
		"arn:aws:sns:us-east-1:000000000000:topic:subscription-id",
		"arn:aws:sns:us-east-1:000000000000",
		"",
		"::::::",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, arn string) {
		parts, err := parseTopicARN(arn)

		if strings.Count(arn, ":") < arnFields-1 {
			if err == nil {
				t.Fatalf("parseTopicARN(%q) accepted an ARN of fewer than six fields", arn)
			}

			return
		}

		if err != nil {
			t.Fatalf("parseTopicARN(%q): %v", arn, err)
		}

		head, _, _ := strings.Cut(arn, ":")
		joined := strings.Join([]string{
			head, parts.partition, parts.service, parts.region, parts.account, parts.resource,
		}, ":")

		if joined != arn {
			t.Fatalf("parseTopicARN(%q) fields join to %q", arn, joined)
		}
	})
}

// FuzzDecodePolicy checks that a policy either is refused or decodes to a
// document that setPolicy writes back and decodePolicy reads again with the
// same statements.
func FuzzDecodePolicy(f *testing.F) {
	for _, seed := range []string{
		defaultTopicPolicy("arn:aws:sns:us-east-1:000000000000:topic", "000000000000"),
		`{"Statement":[]}`,
		`{"Statement":{}}`,
		`{"Version":"2012-10-17","Statement":[{"Sid":"a","Effect":"Allow","Principal":"*","Action":"SNS:Publish",` +
			`"Resource":"*","Condition":{"NumericLessThan":{"n":1e400}}}]}`,
		`null`,
		`[]`,
		`{"Statement":[1,"two",null]} trailing`,
		``,
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, policy string) {
		doc, statements, err := decodePolicy(policy)
		if err != nil {
			return
		}

		topic := &Topic{Attributes: map[string]string{}}

		err = setPolicy(topic, doc)
		if err != nil {
			t.Fatalf("setPolicy of decoded %q: %v", policy, err)
		}

		_, again, err := decodePolicy(topic.Attributes[attrPolicy])
		if err != nil {
			t.Fatalf("decodePolicy of written %q: %v", topic.Attributes[attrPolicy], err)
		}

		if len(again) != len(statements) {
			t.Fatalf("policy %q kept %d of %d statements", policy, len(again), len(statements))
		}
	})
}

// FuzzDefaultTopicPolicy checks that the default policy is JSON that names the
// topic ARN and the account it was built from.
func FuzzDefaultTopicPolicy(f *testing.F) {
	f.Add("arn:aws:sns:us-east-1:000000000000:topic", "000000000000")
	f.Add("arn:aws:sns:us-east-1:000000000000:\"quote\\back\x01", "\x1f")
	f.Add("", "")

	f.Fuzz(func(t *testing.T, arn, account string) {
		if !utf8.ValidString(arn) || !utf8.ValidString(account) {
			// encoding/json replaces invalid UTF-8 on decode, so the
			// round trip below would not compare equal. A topic ARN
			// comes from a valid name, region and account.
			return
		}

		policy := defaultTopicPolicy(arn, account)

		var doc map[string]any

		err := json.Unmarshal([]byte(policy), &doc)
		if err != nil {
			t.Fatalf("defaultTopicPolicy(%q, %q) = %q: %v", arn, account, policy, err)
		}

		statements, _ := doc["Statement"].([]any)
		if len(statements) != 1 {
			t.Fatalf("defaultTopicPolicy(%q, %q) = %q has %d statements", arn, account, policy, len(statements))
		}

		statement, _ := statements[0].(map[string]any)
		condition, _ := statement["Condition"].(map[string]any)
		equals, _ := condition["StringEquals"].(map[string]any)

		if statement["Resource"] != arn || equals["AWS:SourceOwner"] != account {
			t.Fatalf("defaultTopicPolicy(%q, %q) = %q", arn, account, policy)
		}
	})
}
