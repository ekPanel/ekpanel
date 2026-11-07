// Package cron reads classic five-field cron schedules ("*/15 * * * 1-5", "@daily"), finds
// their next run times, and writes them as systemd OnCalendar expressions. The panel uses it
// to check and preview schedules; the agent uses it again to build timers, trusting nothing.
package cron

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed cron expression: the allowed values of each field.
type Schedule struct {
	Minute, Hour, Day, Month, Weekday []bool
	// A day field written as "*" does not restrict; cron then uses only the other one.
	// With both restricted, a day matches if either matches.
	dayAny, weekdayAny bool
}

var macros = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *",
	"@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

var monthNames = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
var dayNames = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// Parse reads a cron expression.
func Parse(expr string) (Schedule, error) {
	expr = strings.TrimSpace(strings.ToLower(expr))
	if m, ok := macros[expr]; ok {
		expr = m
	}
	f := strings.Fields(expr)
	if len(f) != 5 {
		return Schedule{}, errors.New("a schedule has five fields: minute, hour, day of month, month and day of week")
	}
	var s Schedule
	var err error
	if s.Minute, err = field(f[0], 0, 59, nil); err != nil {
		return s, fmt.Errorf("minute: %w", err)
	}
	if s.Hour, err = field(f[1], 0, 23, nil); err != nil {
		return s, fmt.Errorf("hour: %w", err)
	}
	if s.Day, err = field(f[2], 1, 31, nil); err != nil {
		return s, fmt.Errorf("day of month: %w", err)
	}
	if s.Month, err = field(f[3], 1, 12, monthNames); err != nil {
		return s, fmt.Errorf("month: %w", err)
	}
	// 7 is Sunday too.
	wd, err := field(f[4], 0, 7, dayNames)
	if err != nil {
		return s, fmt.Errorf("day of week: %w", err)
	}
	if wd[7] {
		wd[0] = true
	}
	s.Weekday = wd[:7]
	s.dayAny = f[2] == "*" || f[2] == "?"
	s.weekdayAny = f[4] == "*" || f[4] == "?"
	return s, nil
}

// field reads one comma-separated field into a set indexed by value.
func field(text string, lo, hi int, names []string) ([]bool, error) {
	set := make([]bool, hi+1)
	value := func(v string) (int, error) {
		for i, n := range names {
			if v == n {
				return i + lo, nil
			}
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			return 0, fmt.Errorf("%q is not between %d and %d", v, lo, hi)
		}
		return n, nil
	}
	for part := range strings.SplitSeq(text, ",") {
		rng, stepText, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			var err error
			if step, err = strconv.Atoi(stepText); err != nil || step < 1 || step > hi {
				return nil, fmt.Errorf("bad step %q", stepText)
			}
		}
		from, to := lo, hi
		switch {
		case rng == "*" || rng == "?":
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err error
			if from, err = value(a); err != nil {
				return nil, err
			}
			if to, err = value(b); err != nil {
				return nil, err
			}
			if to < from {
				return nil, fmt.Errorf("range %q runs backwards", rng)
			}
		default:
			v, err := value(rng)
			if err != nil {
				return nil, err
			}
			from, to = v, v
			if hasStep {
				to = hi // "5/15" means from 5, every 15
			}
		}
		for v := from; v <= to; v += step {
			set[v] = true
		}
	}
	return set, nil
}

func (s Schedule) dayMatches(t time.Time) bool {
	day, weekday := s.Day[t.Day()], s.Weekday[int(t.Weekday())]
	switch {
	case s.dayAny && s.weekdayAny:
		return true
	case s.dayAny:
		return weekday
	case s.weekdayAny:
		return day
	default:
		return day || weekday
	}
}

// Next returns the first run strictly after `after`, in loc's wall clock, or the zero time
// if there is none within five years (such as February 30).
func (s Schedule) Next(after time.Time, loc *time.Location) time.Time {
	t := after.In(loc).Truncate(time.Minute).Add(time.Minute)
	end := t.AddDate(5, 0, 0)
	for t.Before(end) {
		switch {
		case !s.Month[int(t.Month())]:
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
		case !s.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
		case !s.Hour[t.Hour()]:
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
		case !s.Minute[t.Minute()]:
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}

// list writes the set values from lo to hi as systemd does: "*", or values and ranges
// ("0,15,30", "1..5"). names, when given, replace numbers.
func list(set []bool, lo, hi int, names []string) string {
	all := true
	for v := lo; v <= hi; v++ {
		all = all && set[v]
	}
	if all {
		return "*"
	}
	var parts []string
	label := func(v int) string {
		if names != nil {
			return names[v]
		}
		if hi > 9 {
			return fmt.Sprintf("%02d", v)
		}
		return strconv.Itoa(v)
	}
	for v := lo; v <= hi; v++ {
		if !set[v] {
			continue
		}
		end := v
		for end+1 <= hi && set[end+1] {
			end++
		}
		switch {
		case names != nil: // day names are listed one by one; systemd ranges do not wrap past Sunday
			for x := v; x <= end; x++ {
				parts = append(parts, label(x))
			}
		case end-v >= 2:
			parts = append(parts, label(v)+".."+label(end))
		case end > v:
			parts = append(parts, label(v), label(end))
		default:
			parts = append(parts, label(v))
		}
		v = end
	}
	return strings.Join(parts, ",")
}

var systemdDays = []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}

// tzRE allows zone names such as Asia/Dhaka or America/Argentina/Buenos_Aires.
var tzRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z0-9_+-]+){0,2}$`)

// ValidZone reports whether name is a time zone this machine knows.
func ValidZone(name string) bool {
	if !tzRE.MatchString(name) {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

// OnCalendar writes the schedule as systemd OnCalendar values in a time zone. Cron's "either
// day field" rule needs two values when both day fields are restricted; a timer accepts several.
func (s Schedule) OnCalendar(zone string) []string {
	clock := list(s.Hour, 0, 23, nil) + ":" + list(s.Minute, 0, 59, nil) + ":00"
	month := list(s.Month, 1, 12, nil)
	days := list(s.Day, 1, 31, nil)
	weekdays := list(s.Weekday, 0, 6, systemdDays)
	suffix := ""
	if zone != "" {
		suffix = " " + zone
	}
	at := func(wd, d string) string {
		v := "*-" + month + "-" + d + " " + clock + suffix
		if wd != "*" {
			v = wd + " " + v
		}
		return v
	}
	switch {
	case s.dayAny && s.weekdayAny:
		return []string{at("*", "*")}
	case s.dayAny:
		return []string{at(weekdays, "*")}
	case s.weekdayAny:
		return []string{at("*", days)}
	default:
		return []string{at("*", days), at(weekdays, "*")}
	}
}
