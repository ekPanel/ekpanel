package store

import (
	"testing"
	"time"
)

func TestNextBackupRun(t *testing.T) {
	// Wednesday 2026-11-04 10:30 UTC.
	now := time.Date(2026, 11, 4, 10, 30, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"hourly": time.Date(2026, 11, 4, 11, 0, 0, 0, time.UTC),
		"daily":  time.Date(2026, 11, 5, 2, 0, 0, 0, time.UTC), // 02:00 has passed today
		"weekly": time.Date(2026, 11, 8, 2, 0, 0, 0, time.UTC), // next Sunday
	}
	for sched, want := range cases {
		got := NextBackupRun(BackupSchedule{Schedule: sched, HourUTC: 2}, now)
		if got == nil || !got.Equal(want) {
			t.Errorf("%s: got %v want %v", sched, got, want)
		}
	}
	if got := NextBackupRun(BackupSchedule{Schedule: "daily", HourUTC: 23}, now); !got.Equal(time.Date(2026, 11, 4, 23, 0, 0, 0, time.UTC)) {
		t.Errorf("daily later today: %v", got)
	}
	if NextBackupRun(BackupSchedule{Schedule: "off"}, now) != nil {
		t.Error("off must have no next run")
	}
}
