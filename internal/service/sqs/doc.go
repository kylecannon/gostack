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
// A FIFO queue gets the same contents until the FIFO ticket writes its own. The
// message operations refuse a FIFO queue with a 501 until then
// (Queue.requireStandard), and that check goes with the ticket. Dead letters
// are not handled yet: where standardContents.Receive takes a message off the
// visible heap is the place to compare its receive count with the
// RedrivePolicy's maxReceiveCount.
//
// FIFO and dead letters (W5). Queue.fifo is fixed at creation, from the
// FifoQueue attribute. validateAttributes, defaultAttributes and
// validateQueueName already treat FIFO queues, and newQueue passes the queue to
// the contents factory, which picks the FIFO implementation. A dead-letter
// queue is another Queue found through State.find, so Contents does not need to
// reach back into the Provider.
//
// # Persistence
//
// Provider.Snapshot and Provider.Restore implement persist.Snapshotter. The
// snapshot is a json envelope, {"v":1,"data":...}: for each account and region
// the queues in creation order, each with its attributes, tags, purge time and
// the data of its Contents, and the names of recently deleted queues. A Contents
// takes part by implementing persistentContents; a queue whose Contents does
// not fails the snapshot, because it would come back empty. A new kind of
// Contents (FIFO, with its deduplication windows and sequence counters) has to
// implement it, and Test_every_kind_of_queue_can_be_saved fails until it does.
// A snapshot holds absolute times, so the deadline of a delayed or in-flight
// message holds across a restart, and restore re-arms each queue's timer from
// them. Restore reads and checks the whole file before it replaces anything, and
// leaves SQS empty when the file is not valid.
//
// Provider.Reset(account, region) is the /_gostack/state/reset hook.
//
// # Dirty marking
//
// The gateway marks SQS dirty after an operation that went through its own /*
// route, using Registration.Mutating: every operation but Get* and List*, so
// ReceiveMessage counts, because it hides the messages it returns. A call made
// to a queue URL goes through the routes of Mount, so the Provider reports it
// itself through the function given to OnChange.
package sqs
