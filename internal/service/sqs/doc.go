// Package sqs is the SQS provider. It implements the generated sqs.Handler for
// queues, attributes, tags, permissions and purge, serves both the json and the
// query protocol through the generated dispatcher, and makes queue URLs work as
// endpoints (see Provider.Registration).
//
// # Model
//
// A Provider keeps one State per account and region in a store.Bundle. A State
// holds the queues of that pair by name and the names deleted lately. Its lock
// guards those two maps only, and a call holds it just to find, add or remove a
// queue. Every Queue has its own lock for its attributes and tags, so calls on
// different queues never wait for each other.
//
// Settings are read through a function on every call and never cached, because
// the parity bridge changes SQS_ENDPOINT_STRATEGY while gostack runs.
//
// # Extension points
//
// The Queue type is complete for what the later tickets add. They fill these
// seams and leave the type as it is.
//
// Messages. A queue's messages live in its Contents, an interface the Queue
// calls with its own lock held: Counts feeds the three Approximate attributes,
// Clear is PurgeQueue, Close stops timers when the queue is deleted or reset,
// and Put, Receive, Delete, ChangeVisibility and Wake are the message
// operations. Provider.contents is the factory that makes one per queue. For a
// standard queue it makes a standardContents, which keeps the visible messages
// in a heap in sending order, and the delayed and in-flight ones in a second
// heap by deadline. One time.Timer is armed for the earliest deadline of that
// heap, and stopped when the heap is empty, so a queue with no pending deadline
// costs no wakeups. The timer callback takes the queue's lock like any other
// caller. Long polls do not hold the lock: ReceiveMessage asks the contents for
// a channel (Wake) that is closed when a message may be visible, releases the
// lock and waits on that channel, the end of its wait and its request context.
// The implementation reads the queue's attributes (VisibilityTimeout,
// DelaySeconds, MaximumMessageSize, MessageRetentionPeriod) from Queue.attrs
// under the queue's lock, so a SetQueueAttributes call reaches it without a
// notification. Message retention runs without a timer, when
// Settings.EnableMessageRetentionPeriod is on: whatever looks at the queue first
// drops the visible messages that are past it.
//
// FIFO queues. A FIFO queue's contents is a fifoContents, which the Provider's
// factory picks from the queue's type. Its messages are in message groups: a
// receive takes from the group that became ready first, and from the next ones
// while it wants more, and closes each group it took from until one of its
// messages is visible again or the last one in flight is deleted. Sending to a
// closed group does not open it. A group's in-flight messages come back in the
// order they were received, so a message whose time is over waits behind an
// earlier one that is not. Delayed messages, the head of each group's in-flight
// list and the deduplication records (five minutes, exact, kept in the same heap
// and dropped at the first whole second after) share the one heap and the one
// timer, so a FIFO queue adds no goroutine and no wakeup of its own.
// DeduplicationScope is read when the queue is created, as LocalStack does.
//
// Dead letters. Receive returns the messages whose receive count passed the
// RedrivePolicy's maxReceiveCount next to the ones it hands out, and the
// Provider puts them on the dead-letter queue after the source queue's lock is
// released, so no call holds the lock of two queues. The message keeps its id
// and attributes and the receives so far carry over into ApproximateReceiveCount.
//
// Message move tasks. A task is a goroutine that takes one message at a time
// from a dead-letter queue and puts it on its destination (or on the queue it
// came from), pausing 1/MaxNumberOfMessagesPerSecond between messages. It ends
// when the source is empty, on an error or when its context is cancelled, and
// Provider.Close waits for every one. The pause is a timer in the goroutine, not
// an entry of the queue's heap: a task is one goroutine for as long as it runs
// and none afterwards.
//
// Persistence (W5). State.Range walks the queues in creation order, and a
// queue's persistent fields are name, account, region, fifo, seq, attrs, tags
// and purgedAt, plus whatever its Contents saves. Provider.Reset(account,
// region) is the /_gostack/state/reset hook and already closes the queues it
// drops. Restore builds queues with newQueue-like construction under
// State.mu, and has to keep State.next above the highest restored seq.
//
// Dirty marking. The gateway marks a service dirty after an operation that
// went through its own /* route. A call made to a queue URL goes through the
// routes of Mount and is not marked yet. Persistence needs a seam for it.
package sqs
