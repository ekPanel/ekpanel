package dnssync

import (
	"testing"

	"ekpanel/internal/cloudflare"
)

func kinds(actions []Action) []string {
	var out []string
	for _, a := range actions {
		out = append(out, a.Kind+" "+a.Record.Type+" "+a.Record.Name+" "+a.Record.Content+" "+a.Forget)
	}
	return out
}

func TestPlan(t *testing.T) {
	want := []Want{
		{Name: "new.example.com", Type: "A", Content: "203.0.113.1"},   // nothing yet: create
		{Name: "same.example.com", Type: "A", Content: "203.0.113.1"},  // made by ekPanel, unchanged
		{Name: "moved.example.com", Type: "A", Content: "203.0.113.9"}, // server address changed
		{Name: "hand.example.com", Type: "A", Content: "203.0.113.1"},  // changed by hand
		{Name: "proxy.example.com", Type: "A", Content: "203.0.113.1"}, // only proxied changed by hand
		{Name: "gone.example.com", Type: "A", Content: "203.0.113.1"},  // deleted by hand
		{Name: "theirs.example.com", Type: "A", Content: "198.51.100.7"},
		{Name: "cname.example.com", Type: "A", Content: "203.0.113.1"},
		{Name: "already.example.com", Type: "A", Content: "203.0.113.1"},
	}
	ours := []Managed{
		{RecordID: "r-same", Name: "same.example.com", Type: "A", Content: "203.0.113.1", Proxied: true},
		{RecordID: "r-moved", Name: "moved.example.com", Type: "A", Content: "203.0.113.1", Proxied: false},
		{RecordID: "r-hand", Name: "hand.example.com", Type: "A", Content: "203.0.113.1", Proxied: true},
		{RecordID: "r-proxy", Name: "proxy.example.com", Type: "A", Content: "203.0.113.1", Proxied: true},
		{RecordID: "r-gone", Name: "gone.example.com", Type: "A", Content: "203.0.113.1", Proxied: true},
		{RecordID: "r-old", Name: "old.example.com", Type: "A", Content: "203.0.113.1", Proxied: true},
		{RecordID: "r-oldhand", Name: "oldhand.example.com", Type: "A", Content: "203.0.113.1", Proxied: true},
	}
	have := []cloudflare.Record{
		{ID: "r-same", Type: "A", Name: "same.example.com", Content: "203.0.113.1", Proxied: true},
		{ID: "r-moved", Type: "A", Name: "moved.example.com", Content: "203.0.113.1"},
		{ID: "r-hand", Type: "A", Name: "hand.example.com", Content: "192.0.2.50", Proxied: true},
		{ID: "r-proxy", Type: "A", Name: "proxy.example.com", Content: "203.0.113.1", Proxied: false},
		{ID: "x-theirs", Type: "A", Name: "theirs.example.com", Content: "192.0.2.1"},
		{ID: "x-cname", Type: "CNAME", Name: "cname.example.com", Content: "elsewhere.example.net"},
		{ID: "x-already", Type: "A", Name: "already.example.com", Content: "203.0.113.1", Proxied: false},
		{ID: "r-old", Type: "A", Name: "old.example.com", Content: "203.0.113.1", Proxied: true},
		{ID: "r-oldhand", Type: "A", Name: "oldhand.example.com", Content: "192.0.2.99", Proxied: true},
		{ID: "x-mx", Type: "MX", Name: "example.com", Content: "mail.example.com"},
	}
	actions, issues := Plan(want, ours, have, true, nil)
	got := map[string]bool{}
	for _, k := range kinds(actions) {
		got[k] = true
	}
	for _, k := range []string{
		"create A new.example.com 203.0.113.1 ",
		"update A moved.example.com 203.0.113.9 ",
		"adopt A proxy.example.com 203.0.113.1 ",
		"create A gone.example.com 203.0.113.1 r-gone",
		"adopt A already.example.com 203.0.113.1 ",
		"delete A old.example.com 203.0.113.1 r-old",
		"forget    r-oldhand",
	} {
		if !got[k] {
			t.Errorf("missing %q in %q", k, kinds(actions))
		}
	}
	if len(actions) != 7 {
		t.Errorf("unexpected actions %q", kinds(actions))
	}
	for _, a := range actions {
		if a.Kind == "create" && (a.Record.Comment != cloudflare.ManagedComment || !a.Record.Proxied) {
			t.Errorf("new records are marked and use the zone's default: %+v", a.Record)
		}
		if a.Record.Name == "moved.example.com" && a.Record.Proxied {
			t.Error("an update keeps the record's proxied setting")
		}
	}
	want2 := map[string]string{"hand.example.com": "changed", "theirs.example.com": "conflict", "cname.example.com": "conflict"}
	if len(issues) != 3 {
		t.Fatalf("issues: %+v", issues)
	}
	for _, i := range issues {
		if want2[i.Name] != i.Kind {
			t.Errorf("issue %+v", i)
		}
	}

	// Forcing writes ekPanel's values over them.
	force := map[string]bool{"A hand.example.com": true, "A theirs.example.com": true, "A cname.example.com": true}
	actions, issues = Plan(want, ours, have, true, force)
	got = map[string]bool{}
	for _, k := range kinds(actions) {
		got[k] = true
	}
	if len(issues) != 0 || !got["update A hand.example.com 203.0.113.1 "] || !got["update A theirs.example.com 198.51.100.7 "] ||
		!got["delete CNAME cname.example.com elsewhere.example.net "] || !got["create A cname.example.com 203.0.113.1 "] {
		t.Fatalf("forced: %q %+v", kinds(actions), issues)
	}
}
