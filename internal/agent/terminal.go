package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"ekpanel/internal/agentproto"
)

// Terminals are shells in pseudo-terminals, streamed to the panel over the agent's
// connection. They belong to that connection: when it drops, they end.

const maxTerminals = 10

var streamIDRE = regexp.MustCompile(`^[A-Za-z0-9]{1,32}$`)

type terminal struct {
	ptmx *os.File
	cmd  *exec.Cmd
	once sync.Once
}

// stop hangs up the shell's session, then kills it if it does not leave.
func (t *terminal) stop() {
	t.once.Do(func() {
		if t.cmd.Process != nil {
			syscall.Kill(-t.cmd.Process.Pid, syscall.SIGHUP)
			go func(p *os.Process) {
				time.Sleep(3 * time.Second)
				syscall.Kill(-p.Pid, syscall.SIGKILL)
			}(t.cmd.Process)
		}
		t.ptmx.Close()
	})
}

type terminals struct {
	sys     *system
	send    func(agentproto.Envelope) error
	command func(context.Context, agentproto.TerminalParams) (*exec.Cmd, error) // tests swap it
	mu      sync.Mutex
	open    map[string]*terminal
}

func newTerminals(sys *system, send func(agentproto.Envelope) error) *terminals {
	ts := &terminals{sys: sys, send: send, open: map[string]*terminal{}}
	ts.command = ts.shellCommand
	return ts
}

// shellCommand is what runs in the terminal.
func (ts *terminals) shellCommand(ctx context.Context, p agentproto.TerminalParams) (*exec.Cmd, error) {
	switch p.Kind {
	case "server":
		shell := "/bin/bash"
		if _, err := os.Stat(shell); err != nil {
			shell = "/bin/sh"
		}
		cmd := exec.Command(shell, "-l")
		cmd.Dir = "/root"
		cmd.Env = []string{"TERM=xterm-256color", "HOME=/root", "USER=root", "LOGNAME=root", "SHELL=" + shell,
			"LANG=C.UTF-8", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
		return cmd, nil
	case "container":
		if !appNameRE.MatchString(p.App) || !serviceRE.MatchString(p.Service) {
			return nil, errors.New("invalid app or service")
		}
		out, err := ts.sys.run(ctx, "docker", "ps", "--quiet", "--filter", "label=com.docker.compose.project="+appProject(p.App),
			"--filter", "label=com.docker.compose.service="+p.Service)
		ids := strings.Fields(string(out))
		if err != nil || len(ids) == 0 {
			return nil, fmt.Errorf("%s of app %s is not running", p.Service, p.App)
		}
		cmd := exec.Command("docker", "exec", "--interactive", "--tty", "--env", "TERM=xterm-256color", ids[0],
			"sh", "-c", "if command -v bash >/dev/null 2>&1; then exec bash; else exec sh; fi")
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		return cmd, nil
	}
	return nil, fmt.Errorf("unknown terminal kind %q", p.Kind)
}

func clampSize(cols, rows int) (uint16, uint16) {
	if cols < 10 || cols > 500 {
		cols = 80
	}
	if rows < 3 || rows > 300 {
		rows = 24
	}
	return uint16(cols), uint16(rows)
}

// start opens a terminal. Its output goes to the panel until the shell exits.
func (ts *terminals) start(ctx context.Context, p agentproto.TerminalParams) *agentproto.OpError {
	if !streamIDRE.MatchString(p.Stream) {
		return opError(agentproto.ErrInvalidParams, "invalid stream id")
	}
	ts.mu.Lock()
	n := len(ts.open)
	_, taken := ts.open[p.Stream]
	ts.mu.Unlock()
	if taken {
		return opError(agentproto.ErrInvalidParams, "stream id in use")
	}
	if n >= maxTerminals {
		return opError(agentproto.ErrNotAllowed, "too many terminals are open on this server")
	}
	cmd, err := ts.command(ctx, p)
	if err != nil {
		return opError(agentproto.ErrFailed, "%v", err)
	}
	cols, rows := clampSize(p.Cols, p.Rows)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return opError(agentproto.ErrFailed, "start the shell: %v", err)
	}
	t := &terminal{ptmx: ptmx, cmd: cmd}
	ts.mu.Lock()
	ts.open[p.Stream] = t
	ts.mu.Unlock()

	go func() {
		buf := make([]byte, agentproto.MaxStreamChunk)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				if ts.send(agentproto.Envelope{Type: agentproto.TypeStreamData, Stream: p.Stream, Data: append([]byte(nil), buf[:n]...)}) != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		t.stop()
		reason := "exit"
		if err := cmd.Wait(); err != nil {
			reason = err.Error()
		}
		ts.mu.Lock()
		_, still := ts.open[p.Stream]
		delete(ts.open, p.Stream)
		ts.mu.Unlock()
		if still {
			ts.send(agentproto.Envelope{Type: agentproto.TypeStreamClose, Stream: p.Stream, Reason: reason})
		}
	}()
	return nil
}

func (ts *terminals) get(id string) *terminal {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.open[id]
}

func (ts *terminals) input(env agentproto.Envelope) {
	if t := ts.get(env.Stream); t != nil {
		t.ptmx.Write(env.Data)
	}
}

func (ts *terminals) resize(env agentproto.Envelope) {
	if t := ts.get(env.Stream); t != nil {
		cols, rows := clampSize(env.Cols, env.Rows)
		pty.Setsize(t.ptmx, &pty.Winsize{Cols: cols, Rows: rows})
	}
}

// close ends a terminal the panel closed; no close message goes back.
func (ts *terminals) close(id string) {
	ts.mu.Lock()
	t := ts.open[id]
	delete(ts.open, id)
	ts.mu.Unlock()
	if t != nil {
		t.stop()
	}
}

func (ts *terminals) closeAll() {
	ts.mu.Lock()
	all := ts.open
	ts.open = map[string]*terminal{}
	ts.mu.Unlock()
	for _, t := range all {
		t.stop()
	}
}
