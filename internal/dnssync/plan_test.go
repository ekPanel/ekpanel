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

func TestPlanMailRecords(t *testing.T) {
	want := []Want{
		{Name: "example.com", Type: "MX", Content: "mail.example.net", Priority: 10, DNSOnly: true},
		{Name: "example.com", Type: "TXT", Content: "v=spf1 mx -all", DNSOnly: true},
		{Name: "_dmarc.example.com", Type: "TXT", Content: "v=DMARC1; p=reject", DNSOnly: true},
		{Name: "sel._domainkey.example.com", Type: "TXT", Content: "v=DKIM1; k=rsa; p=AB", DNSOnly: true},
		{Name: "mail.example.net", Type: "A", Content: "203.0.113.1", DNSOnly: true},
	}
	ours := []Managed{
		// Cloudflare hands TXT content back quoted, and long data split into strings.
		{RecordID: "r-dkim", Name: "sel._domainkey.example.com", Type: "TXT", Content: `"v=DKIM1; k=rsa; " "p=AB"`},
	}
	have := []cloudflare.Record{
		{ID: "r-dkim", Type: "TXT", Name: "sel._domainkey.example.com", Content: `"v=DKIM1; k=rsa; " "p=AB"`},
		{ID: "r-verify", Type: "TXT", Name: "example.com", Content: `"google-site-verification=abc"`},
		{ID: "r-dmarc", Type: "TXT", Name: "_dmarc.example.com", Content: `"v=DMARC1; p=none"`},
	}
	actions, issues := Plan(want, ours, have, true, nil)
	got := kinds(actions)
	exp := []string{"create MX example.com mail.example.net ", "create TXT example.com v=spf1 mx -all ", "create A mail.example.net 203.0.113.1 "}
	if len(got) != len(exp) {
		t.Fatalf("actions %q, want %q", got, exp)
	}
	for i := range exp {
		if got[i] != exp[i] {
			t.Errorf("action %d: %q, want %q", i, got[i], exp[i])
		}
	}
	for _, a := range actions {
		if a.Record.Proxied {
			t.Errorf("mail records are never proxied: %+v", a.Record)
		}
		if a.Record.Type == "MX" && (a.Record.Priority == nil || *a.Record.Priority != 10) {
			t.Errorf("MX priority: %+v", a.Record)
		}
	}
	// Someone else's DMARC policy is in the way; the site verification is not.
	if len(issues) != 1 || issues[0].Kind != "conflict" || issues[0].Name != "_dmarc.example.com" {
		t.Errorf("issues: %+v", issues)
	}
}

func TestSameContent(t *testing.T) {
	for _, c := range []struct {
		typ, a, b string
		same      bool
	}{
		{"TXT", `"v=spf1 mx -all"`, "v=spf1 mx -all", true},
		{"TXT", `"a" "b"`, "ab", true},
		{"TXT", `"say \"hi\""`, `say "hi"`, true},
		{"TXT", "v=spf1 a", "v=spf1 mx", false},
		{"MX", "Mail.Example.com.", "mail.example.com", true},
		{"A", "203.0.113.1", "203.0.113.2", false},
	} {
		if sameContent(c.typ, c.a, c.b) != c.same {
			t.Errorf("%s %q %q: want %v", c.typ, c.a, c.b, c.same)
		}
	}
	if txtKind(`"v=DKIM1; k=rsa"`) != "v=dkim1" || txtKind("v=spf1 mx -all") != "v=spf1" {
		t.Error("txtKind")
	}
}
