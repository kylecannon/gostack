package sqs

import "context"

// Shutdown wakes every long poll, as LocalStack's on_before_stop does through
// queue.shutdown() (provider.py): a ReceiveMessage that is waiting for messages
// answers with what it has, which is nothing, and one that starts afterwards
// does not wait. The queues keep their messages, so the state that persistence
// saves at exit is whole. It is for the gateway, which calls it when it starts
// to shut down, and it may be called more than once.
func (p *Provider) Shutdown() {
	p.stopOnce.Do(func() { close(p.stopping) })
}

// untilShutdown returns ctx with its cancellation extended to the Provider's
// shutdown, and the function that releases it.
func (p *Provider) untilShutdown(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)

	go func() {
		select {
		case <-p.stopping:
			cancel()
		case <-ctx.Done():
		}
	}()

	return ctx, cancel
}
