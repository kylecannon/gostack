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
// Messages (W4). A queue's messages live in its Contents, an interface the
// Queue calls with its own lock held: Counts feeds the three Approximate
// attributes, Clear is PurgeQueue, Close stops timers when the queue is deleted
// or reset. W4 writes the implementation and replaces Provider.contents, the
// factory New fills with an empty one. The implementation reads the queue's
// attributes (VisibilityTimeout, DelaySeconds, MaximumMessageSize and the
// rest) from Queue.attrs under the queue's lock, so a SetQueueAttributes call
// reaches it without a notification. The per-queue deadline heap and its one
// armed timer belong to the Contents, and a timer callback takes the queue's
// lock like any other caller.
//
// FIFO and dead letters (W5). Queue.fifo is fixed at creation, from the
// FifoQueue attribute. validateAttributes, defaultAttributes and
// validateQueueName already treat FIFO queues, and newQueue passes the queue to
// the contents factory, which picks the FIFO implementation. A dead-letter
// queue is another Queue found through State.find, so Contents does not need to
// reach back into the Provider.
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
