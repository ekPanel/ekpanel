package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// isTerminal reports whether f is a terminal (a character device). /dev/null is one too;
// a question asked there gets no answer, and ekctl then refuses instead of guessing.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// sttyNoEcho turns echo off with stty(1): the standard library cannot do it by itself.
// On Windows it fails, and the secret is read with echo after a warning.
func sttyNoEcho(f *os.File) func() (func(), error) {
	return func() (func(), error) {
		if runtime.GOOS == "windows" {
			return nil, errors.New("not supported on Windows")
		}
		if err := stty(f, "-echo"); err != nil {
			return nil, err
		}
		return func() { stty(f, "echo") }, nil
	}
}

func stty(f *os.File, arg string) error {
	cmd := exec.Command("stty", arg)
	cmd.Stdin = f
	return cmd.Run()
}

// readLine reads one line from stdin. Ctrl-C stops it.
func (a *app) readLine() (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := a.in.ReadString('\n')
		ch <- result{line, err}
	}()
	select {
	case <-a.ctx.Done():
		return "", a.ctx.Err()
	case r := <-ch:
		if r.err != nil && (!errors.Is(r.err, io.EOF) || r.line == "") {
			return "", errors.New("no answer was typed")
		}
		return strings.TrimRight(r.line, "\r\n"), nil
	}
}

// readSecret asks for a secret on the terminal without showing it.
func (a *app) readSecret(prompt string) (string, error) {
	fmt.Fprint(a.env.Stderr, prompt)
	restore := func() {}
	hidden := false
	if a.env.NoEcho != nil {
		if r, err := a.env.NoEcho(); err == nil {
			restore, hidden = r, true
		}
	}
	if !hidden {
		fmt.Fprint(a.env.Stderr, "\n(Warning: this terminal cannot hide what you type, so the token will be visible.)\n> ")
	}
	line, err := a.readLine()
	restore()
	if hidden {
		fmt.Fprintln(a.env.Stderr)
	}
	return line, err
}

// confirmDelete asks the person to type the name of what is deleted. Without a terminal
// it needs --yes.
func (a *app) confirmDelete(yes bool, what, name string) error {
	if yes {
		return nil
	}
	if !a.env.StdinIsTerminal {
		return usagef("Not deleting %s without confirmation. Run again with --yes.", what)
	}
	fmt.Fprintf(a.env.Stderr, "This deletes %s. It cannot be undone.\nType %s to confirm: ", what, name)
	line, err := a.readLine()
	if err != nil {
		if a.ctx.Err() != nil {
			return a.ctx.Err()
		}
		return errors.New("No answer was typed. Nothing was deleted. Use --yes to delete without asking.")
	}
	if strings.TrimSpace(line) != name {
		return errors.New("That did not match. Nothing was deleted.")
	}
	return nil
}
