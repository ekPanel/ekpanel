package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"ekpanel/internal/agentproto"
)

func applyPanel(t *testing.T, f *fakeSystem, p *agentproto.PanelSpec) *agentproto.PanelResult {
	t.Helper()
	f.calls = nil
	res, opErr := stateApply(context.Background(), f.system, agentproto.StateApplyParams{Panel: p})
	if opErr != nil {
		t.Fatal(opErr)
	}
	return res.Panel
}

func TestPanelDomains(t *testing.T) {
	f := newFakeSystem(t)
	os.MkdirAll(f.path("/etc/caddy"), 0o755)
	os.WriteFile(f.path(caddyfile), []byte("{\n\temail a@b.c\n}\n\npanel.example.com {\n\treverse_proxy 127.0.0.1:8080\n}\n\nimport /etc/ekpanel/caddy/sites/*.caddy\n"), 0o644)

	res := applyPanel(t, f, &agentproto.PanelSpec{Upstream: "127.0.0.1:8080",
		Domains: []string{"hosting.example.org", "panel.example.com", "lab.test"}})
	if res == nil || !res.OK || len(res.Skipped) != 1 || res.Skipped[0] != "panel.example.com" {
		t.Fatalf("result: %+v", res)
	}
	conf := f.read(t, panelCaddy)
	for _, want := range []string{"hosting.example.org {", "lab.test {\n\ttls internal", "reverse_proxy 127.0.0.1:8080"} {
		if !strings.Contains(conf, want) {
			t.Errorf("config lacks %q:\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "panel.example.com") || strings.Contains(conf, "hosting.example.org {\n\ttls internal") {
		t.Errorf("wrong config:\n%s", conf)
	}
	if !f.ran("caddy reload") {
		t.Fatal("Caddy not reloaded")
	}

	// Unchanged: no reload.
	applyPanel(t, f, &agentproto.PanelSpec{Upstream: "127.0.0.1:8080", Domains: []string{"hosting.example.org", "panel.example.com", "lab.test"}})
	if f.ran("caddy reload") {
		t.Fatal("reloaded without a change")
	}

	// Nil leaves the file alone (servers that are not the panel's, older panels).
	if res := applyPanel(t, f, nil); res != nil || !f.exists(panelCaddy) {
		t.Fatal("a nil spec must leave the domains alone")
	}

	// No domains: the file goes.
	if res := applyPanel(t, f, &agentproto.PanelSpec{Upstream: "127.0.0.1:8080"}); !res.OK || f.exists(panelCaddy) {
		t.Fatalf("file should be removed: %+v", res)
	}
}

func TestPanelDomainsRefused(t *testing.T) {
	f := newFakeSystem(t)
	for _, p := range []agentproto.PanelSpec{
		{Upstream: "10.0.0.5:8080", Domains: []string{"a.example.org"}},
		{Upstream: "127.0.0.1:8080", Domains: []string{"a.example.org {\n}\nevil.example"}},
		{Upstream: "127.0.0.1", Domains: []string{"a.example.org"}},
	} {
		if res := applyPanel(t, f, &p); res.OK || f.exists(panelCaddy) {
			t.Errorf("accepted %+v", p)
		}
	}
	f.badCaddy = true
	res := applyPanel(t, f, &agentproto.PanelSpec{Upstream: "127.0.0.1:8080", Domains: []string{"a.example.org"}})
	if res.OK || !strings.Contains(res.Error, "web server rejected") || f.exists(panelCaddy) {
		t.Fatalf("a rejected configuration must be rolled back: %+v", res)
	}
}
