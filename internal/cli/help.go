package cli

import (
	"flag"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"text/tabwriter"
)

// command is one ekctl command: a group of commands (subs), or a command that runs.
type command struct {
	name     string
	aliases  []string
	summary  string // one line, shown in lists
	long     string // shown at the top of --help; summary when empty
	args     []string
	anyArgs  bool   // the command checks its arguments itself
	usage    string // after the command name; built from args and flags when empty
	examples []string
	subs     []*command
	// run registers the command's flags and returns the function that runs it.
	run func(fs *flag.FlagSet) func(args []string) error

	parent *command
}

func (c *command) fullName() string {
	if c.parent == nil {
		return c.name
	}
	return c.parent.fullName() + " " + c.name
}

func (c *command) usageLine() string {
	if c.run == nil {
		return c.fullName() + " <command> [flags]"
	}
	if c.usage != "" {
		return c.fullName() + " " + c.usage
	}
	return strings.TrimSpace(c.fullName() + " " + strings.Join(c.args, " ") + " [flags]")
}

func (c *command) find(name string) *command {
	for _, s := range c.subs {
		if s.name == name || slices.Contains(s.aliases, name) {
			return s
		}
	}
	return nil
}

// suggest returns the sub-command closest to a mistyped name, or "".
func (c *command) suggest(name string) string {
	best, bestDist := "", 3
	for _, s := range c.subs {
		for _, n := range append([]string{s.name}, s.aliases...) {
			if strings.HasPrefix(n, name) && len(name) >= 2 {
				return s.name
			}
			if d := editDistance(name, n); d < bestDist {
				best, bestDist = s.name, d
			}
		}
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

func link(c *command) *command {
	for _, s := range c.subs {
		s.parent = c
		link(s)
	}
	return c
}

// printHelp writes a command's help to stdout.
func (a *app) printHelp(c *command) error {
	w := tabwriter.NewWriter(a.env.Stdout, 0, 0, 3, ' ', 0)
	text := c.long
	if text == "" {
		text = c.summary + "."
	}
	fmt.Fprintf(w, "%s\n\nUsage:\n  %s\n", text, c.usageLine())

	if c.run == nil {
		fmt.Fprintln(w, "\nCommands:")
		for _, s := range c.subs {
			name := s.name
			if s.run == nil {
				name += " …"
			}
			fmt.Fprintf(w, "  %s\t%s\n", name, s.summary)
		}
	} else {
		fs := a.flagSet(c.fullName())
		c.run(fs)
		var own []*flag.Flag
		fs.VisitAll(func(f *flag.Flag) { own = append(own, f) })
		if len(own) > 0 {
			fmt.Fprintln(w, "\nFlags:")
			for _, f := range own {
				fmt.Fprintf(w, "%s\n", flagLine(f, true))
			}
		}
	}
	if len(c.examples) > 0 {
		fmt.Fprintln(w, "\nExamples:")
		for _, e := range c.examples {
			fmt.Fprintf(w, "  %s\n", e)
		}
	}
	fmt.Fprintln(w, "\nGlobal flags:")
	gs := a.flagSet("globals")
	saved := a.g
	a.g = globals{}
	a.addGlobals(gs)
	a.g = saved
	gs.VisitAll(func(f *flag.Flag) { fmt.Fprintf(w, "%s\n", flagLine(f, false)) })
	if c.parent == nil {
		fmt.Fprintf(w, "  --version\tprint ekctl's version\n")
	}
	if c.run == nil {
		fmt.Fprintf(w, "\nRun '%s <command> --help' for more about a command.\n", c.fullName())
	}
	return w.Flush()
}

func flagLine(f *flag.Flag, withDefault bool) string {
	name, usage := flag.UnquoteUsage(f)
	left := "  --" + f.Name
	if name != "" {
		left += " " + strings.ToUpper(name)
	}
	if withDefault && f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" && f.DefValue != "0s" {
		usage += fmt.Sprintf(" (default %s)", f.DefValue)
	}
	return left + "\t" + usage
}

func (a *app) printVersion() error {
	if a.g.JSON {
		return a.printValue(map[string]string{"version": a.env.Version, "os": runtime.GOOS, "arch": runtime.GOARCH, "go": runtime.Version()})
	}
	fmt.Fprintf(a.env.Stdout, "ekctl %s (%s/%s, %s)\n", a.env.Version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	return nil
}
