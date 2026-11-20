package inproc

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"
)

// cutGrace is how long Drain waits, once it has cut, for the calls it
// cancelled to return, so that a call its handler was finishing is counted as
// served and not as cut.
const cutGrace = 20 * time.Millisecond

// Hub is the handler the services' clients are bound to, and the set of those
// clients, so that a shutdown can drain every queue at once. It serves a call
// through the handler it wraps, normally the gateway's. A Client joins it when
// Bind is given the hub.
type Hub struct {
	handler http.Handler

	mu      sync.Mutex
	clients []*Client
	// pending counts the calls the hub's clients have queued and not yet done
	// with: waiting, running, or running their done. A call is counted before a
	// worker can see it and let go of after its done returns, so a call queued
	// by a call being served, or by its done, keeps the count above zero.
	pending int
	// idle, when not nil, is closed when pending falls to zero.
	idle chan struct{}
}

var _ http.Handler = (*Hub)(nil)

// NewHub returns a Hub that serves calls through h.
func NewHub(h http.Handler) *Hub {
	return &Hub{handler: h}
}

func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.handler.ServeHTTP(w, r)
}

// Drain waits until no call of the hub's clients is queued or running, or
// until ctx ends. The count is one for the whole hub, so a call that queues
// another on a different client is never missed between two clients. Then it
// shuts every client's queue: the calls still queued are dropped without
// their done being called, as Close drops them, the ones running are cancelled,
// and Enqueue refuses every later call. It waits a short grace for the
// cancelled calls to return, then logs and returns Cut: a cancelled call that
// was still answered with a 2xx is not counted. A client's Close still waits
// for its workers.
func (h *Hub) Drain(ctx context.Context) int {
	select {
	case <-h.idleSignal():
	case <-ctx.Done():
	}

	for _, c := range h.members() {
		c.queue.shut()
	}

	grace, cancel := context.WithTimeout(context.Background(), cutGrace)
	defer cancel()

	select {
	case <-h.idleSignal():
	case <-grace.Done():
	}

	cut := h.Cut()
	if cut > 0 {
		slog.Warn("in-process calls cut at shutdown: still queued or running at the deadline", "count", cut)
	}

	return cut
}

// Cut is the number of calls the hub's clients never served because their
// queue was shut: dropped while queued, cancelled while running and not
// answered with a 2xx, or refused by Enqueue afterwards. A call cancelled
// while running that is still running counts until it is answered.
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

// enter counts a call queued.
func (h *Hub) enter() {
	h.mu.Lock()
	h.pending++
	h.mu.Unlock()
}

// leave counts a call done with or dropped. The last one signals idle.
func (h *Hub) leave() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.pending--

	if h.pending == 0 && h.idle != nil {
		close(h.idle)
		h.idle = nil
	}
}

// idleSignal returns a channel that is closed once no call is pending: at once
// when none is now.
func (h *Hub) idleSignal() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.idle == nil {
		h.idle = make(chan struct{})
	}

	idle := h.idle

	if h.pending == 0 {
		close(h.idle)
		h.idle = nil
	}

	return idle
}
