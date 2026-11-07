package hub

import (
	"testing"
	"time"

	"hostpanel/internal/agentproto"
)

func TestHistoryRingKeepsNewestInOrder(t *testing.T) {
	h := New()
	base := time.Unix(0, 0)
	for i := range historySize + 5 {
		h.Publish("s1", agentproto.Metrics{At: base.Add(time.Duration(i) * time.Second)})
	}
	_, hist := h.Snapshot("s1")
	if len(hist) != historySize {
		t.Fatalf("len = %d, want %d", len(hist), historySize)
	}
	if got := hist[0].At; !got.Equal(base.Add(5 * time.Second)) {
		t.Fatalf("oldest = %v, want sample 5", got)
	}
	for i := 1; i < len(hist); i++ {
		if !hist[i].At.After(hist[i-1].At) {
			t.Fatalf("history out of order at %d", i)
		}
	}
	_, latest := h.Latest("s1")
	if latest == nil || !latest.At.Equal(base.Add(time.Duration(historySize+4)*time.Second)) {
		t.Fatalf("latest = %v", latest)
	}
}

func TestOnlineStatusAndSubscribers(t *testing.T) {
	h := New()
	events, unsubscribe := h.Subscribe("s1")
	defer unsubscribe()

	done := h.Connected("s1")
	if e := <-events; e.Type != "status" || !e.Online {
		t.Fatalf("want online status, got %+v", e)
	}
	h.Publish("s1", agentproto.Metrics{CPUPercent: 12})
	if e := <-events; e.Type != "metrics" || e.Metrics.CPUPercent != 12 {
		t.Fatalf("want metrics, got %+v", e)
	}
	done()
	done() // calling twice must not double-count
	if e := <-events; e.Type != "status" || e.Online {
		t.Fatalf("want offline status, got %+v", e)
	}
	if online, _ := h.Snapshot("s1"); online {
		t.Fatal("server should be offline")
	}
}
