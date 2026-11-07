// Package hub keeps live state for connected servers (online status and recent metrics)
// and fans updates out to browser subscribers.
package hub

import (
	"sync"

	"ekpanel/internal/agentproto"
)

// historySize keeps about 20 minutes of samples at the agent's 2-second interval.
const historySize = 600

type Event struct {
	Type    string              `json:"type"` // "metrics" or "status"
	Online  bool                `json:"online"`
	Metrics *agentproto.Metrics `json:"metrics,omitempty"`
}

type server struct {
	connections int
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
func (h *Hub) Connected(id string) (disconnected func()) {
	h.mu.Lock()
	s := h.get(id)
	s.connections++
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

// Forget drops all state for a deleted server.
func (h *Hub) Forget(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.servers, id)
}
