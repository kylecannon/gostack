package envelope_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/sns/envelope"
)

// BenchmarkBody covers the envelope's matrix: the message type, the signature
// (version 1, version 2, or none for a FIFO topic), the payload (small,
// 64 KiB, and SNS's 256 KiB maximum) and message attributes (none or 10).
// A confirmation carries SNS's fixed text, so it has no payload or attribute
// axis.
//
//	BenchmarkBody/type=notification/sig=2/payload=64KiB/attrs=10
//	BenchmarkBody/type=subscription-confirmation/sig=1
func BenchmarkBody(b *testing.B) {
	payloads := []struct {
		name string
		size int
	}{
		{name: "512B", size: 512},
		{name: "64KiB", size: 64 << 10},
		{name: "256KiB", size: 256 << 10},
	}

	const (
		attrCount = 10
		sigFIFO   = "fifo"
	)

	attrs := make([]envelope.Attribute, 0, attrCount)
	for i := range attrCount {
		n := strconv.Itoa(i)
		attrs = append(attrs, envelope.Attribute{Name: "attr" + n, DataType: "String", StringValue: "value " + n})
	}

	for _, sig := range []string{"1", "2", sigFIFO} {
		for _, p := range payloads {
			for _, withAttrs := range []bool{false, true} {
				m := envelope.Message{
					MessageID: testID, TopicARN: testTopic, SubscriptionARN: testSub,
					Message: strings.Repeat("x", p.size), Timestamp: testTime, SignatureVersion: sig,
				}
				if sig == sigFIFO {
					m.TopicARN, m.SubscriptionARN, m.SequenceNumber = testFIFO, testFIFOSb, testSeq
				}

				name := "type=notification/sig=" + sig + "/payload=" + p.name + "/attrs=none"
				if withAttrs {
					m.Attributes = attrs
					name = strings.TrimSuffix(name, "none") + "10"
				}

				b.Run(name, func(b *testing.B) { benchBody(b, &m, p.size) })
			}
		}
	}

	for _, sig := range []string{"1", "2"} {
		m := envelope.Message{
			Type: envelope.SubscriptionConfirmation, MessageID: testID, TopicARN: testTopic,
			Token: testToken, Message: confirmMsg, Timestamp: testTime, SignatureVersion: sig,
		}
		b.Run("type=subscription-confirmation/sig="+sig, func(b *testing.B) { benchBody(b, &m, len(confirmMsg)) })
	}
}

func benchBody(b *testing.B, m *envelope.Message, size int) {
	b.Helper()

	_, err := signer.CertificatePEM()
	if err != nil {
		b.Fatal(err)
	}

	b.SetBytes(int64(size))
	b.ReportAllocs()

	for b.Loop() {
		_, err = signer.Body(m, testBase)
		if err != nil {
			b.Fatal(err)
		}
	}
}
