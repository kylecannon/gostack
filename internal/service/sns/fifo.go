package sns

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kylecannon/gostack/internal/request"
)

// dedupWindow is how long a FIFO topic remembers a deduplication id.
const dedupWindow = 5 * time.Minute

// fifoTopic is what a FIFO topic keeps besides its attributes: the lock that
// orders its publishes and the deduplication ids it has seen. It is not
// persisted: a restart forgets the window, as SQS's own window is kept.
type fifoTopic struct {
	// mu is held from a publish's deduplication check to the queueing of its
	// deliveries, so the deliveries to one queue keep publish order.
	mu sync.Mutex
	// seen are the ids published in the window, by deduplication id.
	seen map[string]published
	// order is seen's ids, oldest first.
	order []string
}

// published is a message a FIFO topic took.
type published struct {
	id, sequence string
	at           time.Time
}

// nextSequence is the next FIFO sequence number, LocalStack's
// global_sns_message_sequence: one counter for the process, from a start that
// grows with the clock.
func (p *Provider) nextSequence() string {
	return strconv.FormatUint(p.sequence.Add(1), 10)
}

// publishFIFO publishes msg to the FIFO topic t, as AWS does: a message whose
// deduplication id the topic saw in the last five minutes is accepted and not
// delivered again, and the call returns the first one's ids. The
// deduplication id is the MessageDeduplicationId, or else the SHA-256 of the
// message. LocalStack leaves deduplication to the queues; the aws.validated
// test_publish_to_fifo_topic_deduplication_on_topic_level pins AWS's.
func (p *Provider) publishFIFO(ctx context.Context, rc *request.Context, t *Topic, msg *message) (string, string) {
	key := msg.dedupID
	if key == "" {
		sum := sha256.Sum256([]byte(msg.text))
		key = hex.EncodeToString(sum[:])
	}

	f := &t.fifo

	f.mu.Lock()
	defer f.mu.Unlock()

	now := p.now()
	f.expire(now)

	if first, ok := f.seen[key]; ok {
		return first.id, first.sequence
	}

	msg.sequence = p.nextSequence()

	if f.seen == nil {
		f.seen = make(map[string]published)
	}

	f.seen[key] = published{id: msg.id, sequence: msg.sequence, at: now}
	f.order = append(f.order, key)

	p.fanOut(ctx, rc, t, []*message{msg})

	return msg.id, msg.sequence
}

// expire forgets the ids published before the window. The caller holds f.mu.
func (f *fifoTopic) expire(now time.Time) {
	n := 0
	for n < len(f.order) && now.Sub(f.seen[f.order[n]].at) >= dedupWindow {
		delete(f.seen, f.order[n])
		n++
	}

	if n > 0 {
		f.order = append(f.order[:0:0], f.order[n:]...)
	}
}

const hexDigits = "0123456789abcdef"

// appendJSONString writes s as a JSON string. Invalid UTF-8 becomes U+FFFD.
func appendJSONString(b []byte, s string) []byte {
	b = append(b, '"')
	start := 0

	for i := 0; i < len(s); {
		c := s[i]
		if c >= utf8.RuneSelf {
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				b = append(b, s[start:i]...)
				b = append(b, `�`...)
				i++
				start = i

				continue
			}

			i += size

			continue
		}

		if c >= ' ' && c != '"' && c != '\\' {
			i++

			continue
		}

		b = append(b, s[start:i]...)

		switch c {
		case '"', '\\':
			b = append(b, '\\', c)
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default:
			b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
		}

		i++
		start = i
	}

	b = append(b, s[start:]...)

	return append(b, '"')
}
