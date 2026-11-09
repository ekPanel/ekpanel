// Package cli is ekctl, the command-line tool for the ekPanel API. Everything it needs from
// the outside world comes in through Env, so tests can run it against httptest servers.
package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Env is what ekctl takes from the outside world.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
	// ConfigDir is the user's configuration directory (os.UserConfigDir). The default config
	// file is ConfigDir/ekctl/config.json.
	ConfigDir string
	// StdinIsTerminal is true when a person types on stdin, so ekctl may ask questions.
	StdinIsTerminal bool
	// NoEcho turns off terminal echo while a secret is typed and returns the function that
	// turns it back on. Nil, or an error, means the secret is read with echo (and a warning).
	NoEcho func() (restore func(), err error)
	// Transport replaces the HTTP transport, and with it the certificate checks. For tests.
	Transport http.RoundTripper
	// Version is ekctl's own version, sent as User-Agent: ekctl/<version>.
	Version string
	// PollInterval is how often --wait asks the panel again. Zero means 2 seconds.
	PollInterval time.Duration
}

// OSEnv is the Env of a real process.
func OSEnv(version string) *Env {
	dir, _ := os.UserConfigDir()
	return &Env{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv,
		ConfigDir: dir, StdinIsTerminal: isTerminal(os.Stdin), NoEcho: sttyNoEcho(os.Stdin),
		Version: version,
	}
}

// Exit codes. Scripts depend on them.
const (
	ExitOK    = 0
	ExitError = 1 // the API refused, the network failed, or anything else went wrong
	ExitUsage = 2 // the command line was wrong
)

// globals are the flags every command takes.
type globals struct {
	URL    string
	Token  string
	Config string
	JSON   bool
}

type app struct {
	ctx  context.Context
	env  *Env
	in   *bufio.Reader
	g    globals
	path string // the command being run, such as "ekctl sites create"

	cfg     *Config
	cfgPath string
}

// Run runs ekctl with the arguments after the program name and returns the exit code.
func Run(ctx context.Context, env *Env, args []string) int {
	e := *env
	if e.Getenv == nil {
		e.Getenv = func(string) string { return "" }
	}
	if e.Stdin == nil {
		e.Stdin = strings.NewReader("")
	}
	if e.PollInterval <= 0 {
		e.PollInterval = 2 * time.Second
	}
	if e.Version == "" {
		e.Version = "dev"
	}
	a := &app{ctx: ctx, env: &e, in: bufio.NewReader(e.Stdin), path: "ekctl"}
	return a.exit(a.dispatch(args))
}

// usageError is a mistake on the command line: exit code 2.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// exit prints an error the way a person can act on it and returns the exit code.
func (a *app) exit(err error) int {
	if err == nil {
		return ExitOK
	}
	w := a.env.Stderr
	var ue *usageError
	if errors.As(err, &ue) {
		fmt.Fprintf(w, "Error: %s\n", ue.msg)
		fmt.Fprintf(w, "Run '%s --help' for usage.\n", a.path)
		return ExitUsage
	}
	var ae *APIError
	if errors.As(err, &ae) {
		fmt.Fprintf(w, "Error: %s (%s)\n", ae.Message, ae.Code)
		if h := ae.hint(a.loginCommand()); h != "" {
			fmt.Fprintln(w, h)
		}
		return ExitError
	}
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(w, "Error: interrupted.")
		return ExitError
	}
	fmt.Fprintf(w, "Error: %s\n", err)
	return ExitError
}

// loginCommand is the command that logs in again to the panel in use.
func (a *app) loginCommand() string {
	u := a.g.URL
	if u == "" {
		u = a.env.Getenv("EKCTL_URL")
	}
	if u == "" && a.cfg != nil {
		u = a.cfg.URL
	}
	if n, err := normalizeURL(u); err == nil && u != "" {
		return "ekctl login --url " + n
	}
	return "ekctl login --url https://panel.example.com"
}

// flagSet makes a flag set with the global flags. Flags are parsed by ekctl itself, so the
// flag package prints nothing.
func (a *app) flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

var globalFlagNames = map[string]bool{"url": true, "token": true, "config": true, "json": true}

func (a *app) addGlobals(fs *flag.FlagSet) {
	// The current values are the defaults, so a flag given earlier on the line is kept.
	fs.StringVar(&a.g.URL, "url", a.g.URL, "the panel's `URL`, such as https://panel.example.com (or EKCTL_URL)")
	fs.StringVar(&a.g.Token, "token", a.g.Token, "an API `token` (ekp_…) to use instead of the saved one (or EKCTL_TOKEN)")
	fs.StringVar(&a.g.Config, "config", a.g.Config, "the config `file` (or EKCTL_CONFIG; default: your config folder/ekctl/config.json)")
	fs.BoolVar(&a.g.JSON, "json", a.g.JSON, "print the API's JSON instead of a table")
}

// parseInterspersed parses flags that may come before, between or after positional
// arguments. Everything after "--" is positional.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// dispatch finds the command in args and runs it.
func (a *app) dispatch(args []string) error {
	root := a.commands()
	cmd := root
	for cmd.run == nil {
		fs := a.flagSet(cmd.fullName())
		a.addGlobals(fs)
		showVersion := false
		if cmd == root {
			fs.BoolVar(&showVersion, "version", false, "print ekctl's version")
		}
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return a.printHelp(cmd)
			}
			return usagef("%s", flagError(err))
		}
		if showVersion {
			return a.printVersion()
		}
		args = fs.Args()
		if len(args) == 0 {
			return a.printHelp(cmd)
		}
		sub := cmd.find(args[0])
		if sub == nil {
			msg := fmt.Sprintf("Unknown command %q for %q.", args[0], cmd.fullName())
			if s := cmd.suggest(args[0]); s != "" {
				msg += fmt.Sprintf(" Did you mean %q?", s)
			}
			return usagef("%s", msg)
		}
		cmd, args = sub, args[1:]
		a.path = cmd.fullName()
	}

	fs := a.flagSet(cmd.fullName())
	run := cmd.run(fs)
	a.addGlobals(fs)
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return a.printHelp(cmd)
		}
		return usagef("%s", flagError(err))
	}
	if !cmd.anyArgs {
		if len(pos) < len(cmd.args) {
			return usagef("Missing %s. Usage: %s", cmd.args[len(pos)], cmd.usageLine())
		}
		if len(pos) > len(cmd.args) {
			return usagef("Too many arguments: %s. Usage: %s", strings.Join(pos[len(cmd.args):], " "), cmd.usageLine())
		}
	}
	return run(pos)
}

// flagError rewords the flag package's errors as sentences.
func flagError(err error) string {
	s := err.Error()
	if name, ok := strings.CutPrefix(s, "flag provided but not defined: "); ok {
		return fmt.Sprintf("Unknown flag %s.", name)
	}
	if name, ok := strings.CutPrefix(s, "flag needs an argument: "); ok {
		return fmt.Sprintf("The flag %s needs a value.", name)
	}
	if len(s) > 0 {
		s = strings.ToUpper(s[:1]) + s[1:]
	}
	return s + "."
}
