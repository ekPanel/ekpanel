package agent

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"ekpanel/internal/agentproto"
)

type sink struct {
	mu     sync.Mutex
	out    strings.Builder
	closed []string
}

func (s *sink) send(env agentproto.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch env.Type {
	case agentproto.TypeStreamData:
		s.out.Write(env.Data)
	case agentproto.TypeStreamClose:
		s.closed = append(s.closed, env.Stream+":"+env.Reason)
	}
	return nil
}

func (s *sink) wait(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		s.mu.Lock()
		done := ok()
		s.mu.Unlock()
		if done {
			return
		}
	}
	t.Fatalf("timed out waiting for %s; output %q", what, s.out.String())
}

func TestTerminal(t *testing.T) {
	s := &sink{}
	ts := newTerminals(newFakeSystem(t).system, s.send)
	ts.command = func(context.Context, agentproto.TerminalParams) (*exec.Cmd, error) {
		return exec.Command("/bin/sh", "-c", `stty -echo; echo ready; read line; echo "got:$line"; stty size; read wait`), nil
	}
	if opErr := ts.start(context.Background(), agentproto.TerminalParams{Stream: "s1", Kind: "server", Cols: 100, Rows: 30}); opErr != nil {
		t.Fatal(opErr)
	}
	s.wait(t, "the shell", func() bool { return strings.Contains(s.out.String(), "ready") })
	ts.input(agentproto.Envelope{Stream: "s1", Data: []byte("hello\n")})
	s.wait(t, "the echo", func() bool {
		return strings.Contains(s.out.String(), "got:hello") && strings.Contains(s.out.String(), "30 100")
	})

	// The panel closes it: the shell ends and no close message goes back.
	ts.close("s1")
	time.Sleep(300 * time.Millisecond)
	if len(s.closed) != 0 || ts.get("s1") != nil {
		t.Fatalf("closed by the panel: %v", s.closed)
	}

	// A shell that exits on its own reports it.
	ts.command = func(context.Context, agentproto.TerminalParams) (*exec.Cmd, error) {
		return exec.Command("/bin/sh", "-c", "exit 3"), nil
	}
	ts.start(context.Background(), agentproto.TerminalParams{Stream: "s2", Kind: "server"})
	s.wait(t, "the exit", func() bool { return len(s.closed) == 1 && strings.HasPrefix(s.closed[0], "s2:exit status 3") })
}

func TestTerminalLimits(t *testing.T) {
	s := &sink{}
	ts := newTerminals(newFakeSystem(t).system, s.send)
	ts.command = func(context.Context, agentproto.TerminalParams) (*exec.Cmd, error) {
		return exec.Command("/bin/sh", "-c", "sleep 30"), nil
	}
	defer ts.closeAll()
	if opErr := ts.start(context.Background(), agentproto.TerminalParams{Stream: "../x"}); opErr == nil {
		t.Fatal("bad stream id accepted")
	}
	for i := range maxTerminals {
		if opErr := ts.start(context.Background(), agentproto.TerminalParams{Stream: "t" + string(rune('a'+i))}); opErr != nil {
			t.Fatal(opErr)
		}
	}
	if opErr := ts.start(context.Background(), agentproto.TerminalParams{Stream: "one"}); opErr == nil {
		t.Fatal("more than the limit")
	}
	real := newTerminals(newFakeSystem(t).system, s.send)
	if _, err := real.shellCommand(context.Background(), agentproto.TerminalParams{Kind: "container", App: "../etc", Service: "web"}); err == nil {
		t.Fatal("bad app accepted")
	}
	if _, err := real.shellCommand(context.Background(), agentproto.TerminalParams{Kind: "root"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
}
