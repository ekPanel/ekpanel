// Package hub keeps live state for connected servers (online status and recent metrics)
// and fans updates out to browser subscribers.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"ekpanel/internal/agentproto"
)

// ErrOffline means the server's agent is not connected.
var ErrOffline = errors.New("the server's agent is not connected")

// Caller sends an operation to one agent connection and waits for the result.
type Caller interface {
	Call(ctx context.Context, op string, params any) (json.RawMessage, error)
}

// historySize keeps about 20 minutes of samples at the agent's 2-second interval.
const historySize = 600

type Event struct {
	Type    string              `json:"type"` // "metrics" or "status"
	Online  bool                `json:"online"`
	Metrics *agentproto.Metrics `json:"metrics,omitempty"`
}

type server struct {
	connections int
	callers     []Caller             // open agent connections, newest last
	history     []agentproto.Metrics // ring buffer, oldest at index start
	start       int
	subs        map[chan Event]struct{}
}

type Hub struct {
	mu      sync.Mutex
	servers map[string]*server
}

func New() *Hub { return &Hub{servers: map[string]*server{}} }

func (h *Hub) get(id string) *server {
	s := h.servers[id]
	if s == nil {
		s = &server{subs: map[chan Event]struct{}{}}
		h.servers[id] = s
	}
	return s
}

// broadcast sends without blocking: a subscriber that falls behind misses samples
// rather than stalling the agent connection.
func (s *server) broadcast(e Event) {
	for ch := range s.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// Connected marks an agent connection open and returns a func to call when it closes.
// c receives operations for the server while it is the newest connection.
func (h *Hub) Connected(id string, c Caller) (disconnected func()) {
	h.mu.Lock()
	s := h.get(id)
	s.connections++
	s.callers = append(s.callers, c)
	if s.connections == 1 {
		s.broadcast(Event{Type: "status", Online: true})
	}
	h.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			s.connections--
			for i, x := range s.callers {
				if x == c {
					s.callers = append(s.callers[:i], s.callers[i+1:]...)
					break
				}
			}
			if s.connections == 0 {
				s.broadcast(Event{Type: "status", Online: false})
			}
		})
	}
}

func (h *Hub) Publish(id string, m agentproto.Metrics) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.get(id)
	if len(s.history) < historySize {
		s.history = append(s.history, m)
	} else {
		s.history[s.start] = m
		s.start = (s.start + 1) % historySize
	}
	s.broadcast(Event{Type: "metrics", Online: s.connections > 0, Metrics: &m})
}

// Snapshot returns whether the server's agent is connected and its recent samples, oldest first.
func (h *Hub) Snapshot(id string) (online bool, history []agentproto.Metrics) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.servers[id]
	if s == nil {
		return false, nil
	}
	history = make([]agentproto.Metrics, 0, len(s.history))
	history = append(history, s.history[s.start:]...)
	history = append(history, s.history[:s.start]...)
	return s.connections > 0, history
}

// Latest returns the newest sample, if any.
func (h *Hub) Latest(id string) (online bool, latest *agentproto.Metrics) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.servers[id]
	if s == nil || len(s.history) == 0 {
		return s != nil && s.connections > 0, nil
	}
	i := (s.start + len(s.history) - 1) % len(s.history)
	m := s.history[i]
	return s.connections > 0, &m
}

// Subscribe returns a channel of events for server id and a func that unsubscribes.
func (h *Hub) Subscribe(id string) (<-chan Event, func()) {
	ch := make(chan Event, 16)
	h.mu.Lock()
	h.get(id).subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.get(id).subs, ch)
		h.mu.Unlock()
	}
}

// Call runs an operation on the server's agent (its newest connection).
func (h *Hub) Call(ctx context.Context, id, op string, params any) (json.RawMessage, error) {
	h.mu.Lock()
	var c Caller
	if s := h.servers[id]; s != nil && len(s.callers) > 0 {
		c = s.callers[len(s.callers)-1]
	}
	h.mu.Unlock()
	if c == nil {
		return nil, ErrOffline
	}
	return c.Call(ctx, op, params)
}

// Disconnecter is implemented by agent connections that can be closed from the panel.
type Disconnecter interface {
	Disconnect(reason string)
}

// Disconnect closes every open connection of a server's agent, as when its certificate is
// revoked: a revoked agent must not keep a connection it opened before.
func (h *Hub) Disconnect(id string) {
	h.mu.Lock()
	var callers []Caller
	if s := h.servers[id]; s != nil {
		callers = append(callers, s.callers...)
	}
	h.mu.Unlock()
	for _, c := range callers {
		if d, ok := c.(Disconnecter); ok {
			d.Disconnect("the agent's certificate was revoked")
		}
	}
}

// Forget drops all state for a deleted server.
func (h *Hub) Forget(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.servers, id)
}
