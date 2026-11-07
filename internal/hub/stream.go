package hub

import (
	"context"
	"errors"
)

// Stream is an open byte stream to an agent, such as a terminal.
type Stream interface {
	// Output delivers what the agent sends; it is closed when the stream ends.
	Output() <-chan []byte
	// Reason says why the stream ended, once Output is closed.
	Reason() string
	Write(ctx context.Context, data []byte) error
	Resize(ctx context.Context, cols, rows int) error
	Close()
}

// Streamer is implemented by agent connections that can open streams.
type Streamer interface {
	OpenStream(ctx context.Context, op string, params func(id string) any) (Stream, error)
}

// ErrNoStreams means the agent connection cannot carry streams.
var ErrNoStreams = errors.New("this agent connection cannot open streams")

// OpenStream opens a stream on the server's agent (its newest connection). params builds the
// request's parameters around the stream's ID.
func (h *Hub) OpenStream(ctx context.Context, id, op string, params func(streamID string) any) (Stream, error) {
	h.mu.Lock()
	var c Caller
	if s := h.servers[id]; s != nil && len(s.callers) > 0 {
		c = s.callers[len(s.callers)-1]
	}
	h.mu.Unlock()
	if c == nil {
		return nil, ErrOffline
	}
	st, ok := c.(Streamer)
	if !ok {
		return nil, ErrNoStreams
	}
	return st.OpenStream(ctx, op, params)
}
