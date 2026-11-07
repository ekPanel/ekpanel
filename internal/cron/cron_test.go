package cron

import (
	"slices"
	"testing"
	"time"
)

func TestOnCalendar(t *testing.T) {
	for _, c := range []struct {
		expr string
		want []string
	}{
		{"* * * * *", []string{"*-*-* *:*:00 UTC"}},
		{"*/15 * * * *", []string{"*-*-* *:00,15,30,45:00 UTC"}},
		{"30 2 * * *", []string{"*-*-* 02:30:00 UTC"}},
		{"@hourly", []string{"*-*-* *:00:00 UTC"}},
		{"0 9-17 * * mon-fri", []string{"Mon,Tue,Wed,Thu,Fri *-*-* 09..17:00:00 UTC"}},
		{"0 0 1,15 * *", []string{"*-*-01,15 00:00:00 UTC"}},
		{"0 0 * jan,jul 7", []string{"Sun *-01,07-* 00:00:00 UTC"}},
		// Cron runs on the 1st OR on Mondays when both are given.
		{"0 6 1 * 1", []string{"*-*-01 06:00:00 UTC", "Mon *-*-* 06:00:00 UTC"}},
		{"5/20 * * * *", []string{"*-*-* *:05,25,45:00 UTC"}},
	} {
		s, err := Parse(c.expr)
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		if got := s.OnCalendar("UTC"); !slices.Equal(got, c.want) {
			t.Errorf("%s = %q, want %q", c.expr, got, c.want)
		}
	}
	for _, bad := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 8",
		"*/0 * * * *", "5-1 * * * *", "* * * * mon;reboot", "@reboot", "a b c d e"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestNext(t *testing.T) {
	dhaka, _ := time.LoadLocation("Asia/Dhaka")
	from := time.Date(2026, 11, 7, 10, 7, 30, 0, time.UTC) // 16:07:30 in Dhaka, a Saturday
	for _, c := range []struct {
		expr string
		want string // in Dhaka time
	}{
		{"* * * * *", "2026-11-07 16:08"},
		{"*/15 * * * *", "2026-11-07 16:15"},
		{"30 2 * * *", "2026-11-08 02:30"},
		{"0 9 * * mon-fri", "2026-11-09 09:00"},
		{"0 0 1 * *", "2026-12-01 00:00"},
		{"0 6 15 * 1", "2026-11-09 06:00"}, // Monday comes before the 15th
		{"0 0 29 2 *", "2028-02-29 00:00"},
	} {
		s, _ := Parse(c.expr)
		if got := s.Next(from, dhaka).Format("2006-01-02 15:04"); got != c.want {
			t.Errorf("%s: next %s, want %s", c.expr, got, c.want)
		}
	}
	s, _ := Parse("0 0 30 2 *")
	if !s.Next(from, time.UTC).IsZero() {
		t.Error("February 30 never comes")
	}
}

func TestZones(t *testing.T) {
	for _, z := range []string{"UTC", "Asia/Dhaka", "America/Argentina/Buenos_Aires"} {
		if !ValidZone(z) {
			t.Errorf("rejected %s", z)
		}
	}
	for _, z := range []string{"", "Asia/Nowhere", "../etc/passwd", "UTC\nOnCalendar=*", "Asia Dhaka"} {
		if ValidZone(z) {
			t.Errorf("accepted %q", z)
		}
	}
}
