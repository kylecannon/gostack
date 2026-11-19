package inproc

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"sync"
)

// Hub is the handler the services' clients are bound to, and the set of those
// clients, so that a shutdown can drain every queue at once. It serves a call
// through the handler it wraps, normally the gateway's. A Client joins it when
// Bind is given the hub.
type Hub struct {
	handler http.Handler

	mu      sync.Mutex
	clients []*Client
}

var _ http.Handler = (*Hub)(nil)

// NewHub returns a Hub that serves calls through h.
func NewHub(h http.Handler) *Hub {
	return &Hub{handler: h}
}

func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.handler.ServeHTTP(w, r)
}

// Drain waits until no client has a call queued or running, all at the same
// time, since a call served can queue another, or until ctx ends. Then it
// shuts every client's queue: the calls still queued are dropped without
// their done being called, as Close drops them, the ones running are cancelled
// and not waited for, and Enqueue refuses every later call. Drain returns the
// number of calls it cut, and logs it when it is not zero. A client's Close
// still waits for its workers.
func (h *Hub) Drain(ctx context.Context) int {
	// A client can go busy again while another is waited for; then all of them
	// are waited for once more.
	for ctx.Err() == nil {
		if h.idle(ctx) {
			break
		}
	}

	cut := 0
	for _, c := range h.members() {
		cut += c.queue.shut()
	}

	if cut > 0 {
		slog.Warn("in-process calls cut at shutdown: still queued or running at the deadline", "count", cut)
	}

	return cut
}

// Cut is the number of calls the hub's clients never served because their
// queue was shut: cut by Drain or Close, or refused by Enqueue afterwards.
func (h *Hub) Cut() int {
	cut := 0

	for _, c := range h.members() {
		c.queue.mu.Lock()
		cut += c.queue.cut
		c.queue.mu.Unlock()
	}

	return cut
}

// add makes c one of the hub's clients, once.
func (h *Hub) add(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if !slices.Contains(h.clients, c) {
		h.clients = append(h.clients, c)
	}
}

func (h *Hub) members() []*Client {
	h.mu.Lock()
	defer h.mu.Unlock()

	return slices.Clone(h.clients)
}

// idle waits for each client in turn to be idle, until ctx ends, and reports
// whether every client was idle when the last had been waited for.
func (h *Hub) idle(ctx context.Context) bool {
	clients := h.members()

	for _, c := range clients {
		select {
		case <-c.queue.idleSignal():
		case <-ctx.Done():
			return false
		}
	}

	for _, c := range clients {
		if !c.Idle() {
			return false
		}
	}

	return true
}
