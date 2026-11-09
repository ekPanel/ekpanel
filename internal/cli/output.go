package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// printJSON pretty-prints the API's answer as it is. An answer that is not JSON is written
// unchanged.
func (a *app) printJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		buf.Reset()
		buf.Write(raw)
	}
	buf.WriteByte('\n')
	_, err := a.env.Stdout.Write(buf.Bytes())
	return err
}

// printValue prints a value of ekctl's own as JSON.
func (a *app) printValue(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return a.printJSON(raw)
}

// table prints aligned columns. With no rows it says so on stderr and prints nothing, so a
// pipe gets no header without data.
func (a *app) table(header []string, rows [][]string, empty string) error {
	if len(rows) == 0 {
		fmt.Fprintln(a.env.Stderr, empty)
		return nil
	}
	tw := tabwriter.NewWriter(a.env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range rows {
		cells := make([]string, len(r))
		for i, c := range r {
			cells[i] = cell(c)
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

// details prints "Key: value" lines.
func (a *app) details(rows [][2]string) error {
	tw := tabwriter.NewWriter(a.env.Stdout, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintf(tw, "%s:\t%s\n", r[0], cell(r[1]))
	}
	return tw.Flush()
}

func cell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "-"
	}
	return s
}

func truncate(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04")
}

func fmtTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return fmtTime(*t)
}

func fmtBytes(n *int64) string {
	if n == nil {
		return ""
	}
	v := float64(*n)
	for _, unit := range []string{"B", "KB", "MB", "GB", "TB"} {
		if v < 1024 || unit == "TB" {
			if unit == "B" {
				return strconv.FormatInt(*n, 10) + " B"
			}
			return fmt.Sprintf("%.1f %s", v, unit)
		}
		v /= 1024
	}
	return ""
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func isUUID(s string) bool { return uuidRE.MatchString(s) }

// named is what ekctl looks at to find something by name instead of ID.
type named struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Domain        string `json:"domain"`
	DomainDisplay string `json:"domain_display"`
}

// resolve turns an ID or a name (or domain) into an ID, looking the name up in a list.
func (a *app) resolve(c *client, kind, listPath, arg string) (string, error) {
	if isUUID(arg) {
		return strings.ToLower(arg), nil
	}
	var list []named
	if _, err := c.get(a.ctx, listPath, &list); err != nil {
		return "", err
	}
	var ids []string
	for _, n := range list {
		for _, v := range []string{n.Name, n.Domain, n.DomainDisplay} {
			if v != "" && strings.EqualFold(v, arg) {
				ids = append(ids, n.ID)
				break
			}
		}
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("No %s is called %q. Give its ID or its exact name.", kind, arg)
	case 1:
		return ids[0], nil
	}
	return "", fmt.Errorf("More than one %s is called %q. Give the ID instead: %s", kind, arg, strings.Join(ids, ", "))
}

// timeoutError: --wait gave up.
type timeoutError struct{ after time.Duration }

func (e *timeoutError) Error() string {
	return fmt.Sprintf("Stopped waiting after %s. The work goes on in the panel; check it with 'ekctl tasks list'.", e.after)
}

// waitFor calls check until it says done, fails, or the time is up.
func (a *app) waitFor(timeout time.Duration, check func(ctx context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(a.ctx, timeout)
	defer cancel()
	tick := time.NewTicker(a.env.PollInterval)
	defer tick.Stop()
	for {
		done, err := check(ctx)
		if err == nil && done {
			return nil
		}
		if ctx.Err() != nil && a.ctx.Err() == nil {
			return &timeoutError{after: timeout}
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			if a.ctx.Err() != nil {
				return a.ctx.Err()
			}
			return &timeoutError{after: timeout}
		case <-tick.C:
		}
	}
}

func (a *app) progress(format string, args ...any) {
	fmt.Fprintf(a.env.Stderr, format+"\n", args...)
}
