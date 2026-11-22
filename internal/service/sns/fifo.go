package sns

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/kylecannon/gostack/internal/request"
)

// dedupWindow is how long a FIFO topic remembers a deduplication id.
const dedupWindow = 5 * time.Minute

// fifoTopic is what a FIFO topic keeps besides its attributes: the lock that
// orders its publishes and the deduplication ids it has seen. The ids are
// persisted with their times, so a restart inside the window still
// deduplicates (snapshot.go).
type fifoTopic struct {
	// mu is held from a publish's deduplication check to the queueing of its
	// deliveries in their lanes, so the deliveries to one queue keep publish
	// order. Nothing is sent under it.
	mu sync.Mutex
	// seen are the ids published in the window, by deduplication id.
	seen map[string]published
	// order is seen's ids, oldest first.
	order []string
	// peak is the most ids seen held at an expire since the window was last
	// built, which says how much room it may have to give back.
	peak int
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

// publishFIFO publishes msgs, in order, to the FIFO topic t, as AWS does: a
// message whose deduplication id the topic saw in the last five minutes, in
// this call or before, is accepted and not delivered again, and takes the
// first one's message id and sequence number. The deduplication id is the
// MessageDeduplicationId, or else the SHA-256 of the message. LocalStack
// leaves deduplication to the queues; the aws.validated
// test_publish_to_fifo_topic_deduplication_on_topic_level pins AWS's. It
// returns the deliveries it queued, for the caller to dispatch once the
// topic's lock is let go.
func (p *Provider) publishFIFO(rc *request.Context, t *Topic, msgs []*message) []*job {
	f := &t.fifo

	f.mu.Lock()
	defer f.mu.Unlock()

	now := p.now()
	f.expire(now)

	fresh := make([]*message, 0, len(msgs))

	for _, msg := range msgs {
		key := msg.dedupID
		if key == "" {
			sum := sha256.Sum256([]byte(msg.text))
			key = hex.EncodeToString(sum[:])
		}

		if first, ok := f.seen[key]; ok {
			msg.id, msg.sequence = first.id, first.sequence

			continue
		}

		msg.sequence = p.nextSequence()

		if f.seen == nil {
			f.seen = make(map[string]published)
		}

		f.seen[key] = published{id: msg.id, sequence: msg.sequence, at: now}
		f.order = append(f.order, key)
		fresh = append(fresh, msg)
	}

	if len(fresh) > 0 {
		// The window is saved state: a restart keeps deduplicating.
		p.markChanged()
	}

	return p.fanOut(rc, t, fresh)
}

// shrinkFloor is the fewest ids a window must have held at once before expire
// builds it smaller. Below it the memory is too little to be worth a rebuild.
const shrinkFloor = 1024

// expire forgets the ids published before the window. The ids are in the order
// they were taken, so the expired ones are a run at the front of f.order: the
// cost is in what expires, not in what the window keeps. The caller holds
// f.mu.
func (f *fifoTopic) expire(now time.Time) {
	f.peak = max(f.peak, len(f.seen))

	n := 0
	for n < len(f.order) && now.Sub(f.seen[f.order[n]].at) >= dedupWindow {
		delete(f.seen, f.order[n])
		n++
	}

	if n == 0 {
		return
	}

	// Let go of the ids' strings now, not when append next grows the queue.
	clear(f.order[:n])
	f.order = f.order[n:]

	// A map keeps its table after its entries are deleted, and a queue its
	// array, so a window that held a million ids and now holds ten still owns
	// the room for the million. Once it is down to a quarter of its peak it is
	// built again from what is left: that copy costs what the expiries since
	// the peak cost, so a publish's share stays constant.
	if f.peak >= shrinkFloor && len(f.seen) <= f.peak/4 {
		seen := make(map[string]published, len(f.seen))
		maps.Copy(seen, f.seen)

		f.seen, f.order, f.peak = seen, slices.Clone(f.order), len(seen)
	}
}
