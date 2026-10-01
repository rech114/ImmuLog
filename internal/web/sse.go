// SPDX-License-Identifier: Apache-2.0

// sse.go -- the SSE transport layer. A pure pipe: it knows nothing about
// message semantics, only how to write Events onto the wire.
//
// Three reasons to use text/event-stream instead of WebSocket (docs/DESIGN.md §7.10):
//
//   - Zero dependencies -- net/http plus Flusher is enough; no gorilla/websocket
//   - The browser's native EventSource reconnects on its own; nothing to write
//   - The event id is the commit OID, so on reconnect the browser sends
//     Last-Event-ID automatically -- resume-after-disconnect comes for free
package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	subBuffer   = 64
	retryMillis = 3000
)

// Heartbeat is the keep-alive interval. A comment line stops intermediaries
// from cutting an idle connection.
const Heartbeat = 20 * time.Second

// ErrNoFlush means the underlying ResponseWriter cannot stream.
var ErrNoFlush = errors.New("sse: ResponseWriter does not support Flusher")

// Event is one event waiting to be sent. ID is the commit OID.
type Event struct {
	ID   string
	Type string
	Data any
}

// Hub is the broadcast centre. A slow client is dropped so it can reconnect and
// resume via Last-Event-ID -- dropping is always preferable to blocking the
// broadcast.
type Hub struct {
	mu      sync.Mutex
	clients map[*subscriber]struct{}
}

type subscriber struct {
	ch   chan Event
	done chan struct{}
}

// NewHub creates an empty broadcast centre.
func NewHub() *Hub { return &Hub{clients: make(map[*subscriber]struct{})} }

// Subscribe registers a subscriber and returns its channel plus an unsubscribe
// function.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	s := &subscriber{ch: make(chan Event, subBuffer), done: make(chan struct{})}
	h.mu.Lock()
	h.clients[s] = struct{}{}
	h.mu.Unlock()

	return s.ch, func() {
		h.mu.Lock()
		if _, ok := h.clients[s]; ok {
			delete(h.clients, s)
			close(s.done)
		}
		h.mu.Unlock()
	}
}

// Broadcast pushes to every subscriber without blocking.
func (h *Hub) Broadcast(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.clients {
		select {
		case s.ch <- ev:
		default:
			delete(h.clients, s) // slow client: drop it, let it resume on reconnect
			close(s.done)
		}
	}
}

// ClientCount returns the current subscriber count (for /api/health).
func (h *Hub) ClientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// Session is one established SSE connection.
type Session struct {
	w http.ResponseWriter
	f http.Flusher
}

// NewSession writes the response headers and returns a writable session.
func NewSession(w http.ResponseWriter) (*Session, error) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, ErrNoFlush
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no") // stop reverse proxies from buffering
	w.WriteHeader(http.StatusOK)
	return &Session{w: w, f: f}, nil
}

// Open sends the reconnect interval and flushes immediately, so the browser
// knows this is a stream.
func (s *Session) Open() error {
	return s.raw(fmt.Sprintf("retry: %d\n\n", retryMillis))
}

// Send writes one event. The id is the commit OID -- the resume cursor is the
// content hash itself.
func (s *Session) Send(ev Event) error {
	var prefix string
	if ev.ID != "" {
		prefix += "id: " + ev.ID + "\n"
	}
	if ev.Type != "" {
		prefix += "event: " + ev.Type + "\n"
	}
	payload, err := json.Marshal(ev.Data)
	if err != nil {
		return err
	}
	return s.raw(prefix + "data: " + string(payload) + "\n\n")
}

// Ping sends a comment line to keep the connection alive.
func (s *Session) Ping() error { return s.raw(": ping\n\n") }

func (s *Session) raw(text string) error {
	if _, err := s.w.Write([]byte(text)); err != nil {
		return err
	}
	s.f.Flush()
	return nil
}
