package agent

import (
	"context"
	"slices"
	"strings"
	"testing"

	"ekpanel/internal/agentproto"
)

func TestFirewallRuleArgs(t *testing.T) {
	for _, c := range []struct {
		in   agentproto.FirewallRuleSpec
		want string
	}{
		{agentproto.FirewallRuleSpec{Action: "allow", Protocol: "tcp", PortFrom: 8080, PortTo: 8080}, "allow 8080/tcp"},
		{agentproto.FirewallRuleSpec{Action: "allow", Protocol: "both", PortFrom: 6000, PortTo: 6010}, "allow 6000:6010/tcp | allow 6000:6010/udp"},
		{agentproto.FirewallRuleSpec{Action: "allow", Protocol: "tcp", PortFrom: 5432, PortTo: 5432, Source: "10.0.0.7/8"}, "allow from 10.0.0.0/8 to any port 5432 proto tcp"},
		{agentproto.FirewallRuleSpec{Action: "deny", Protocol: "both", Source: "203.0.113.9"}, "deny from 203.0.113.9"},
		{agentproto.FirewallRuleSpec{Action: "deny", Protocol: "tcp", Source: "2001:db8::1/128", PortFrom: 25, PortTo: 25}, "deny from 2001:db8::1 to any port 25 proto tcp"},
	} {
		rs, err := firewallRule(c.in)
		var got []string
		for _, r := range rs {
			got = append(got, strings.Join(r, " "))
		}
		if err != nil || strings.Join(got, " | ") != c.want {
			t.Errorf("%+v = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, bad := range []agentproto.FirewallRuleSpec{
		{Action: "allow", Protocol: "tcp"},                                                   // nothing to open
		{Action: "deny", Protocol: "tcp", PortFrom: 22, PortTo: 22},                          // nobody to block
		{Action: "allow", Protocol: "tcp", PortFrom: 90, PortTo: 80},                         // backwards range
		{Action: "allow", Protocol: "icmp", PortFrom: 80, PortTo: 80},                        // protocol
		{Action: "deny", Protocol: "tcp", Source: "1.2.3.4; reboot"},                         // not an address
		{Action: "allow", Protocol: "tcp", PortFrom: 80, PortTo: 80, Source: "fe80::1%eth0"}, // zone
	} {
		if _, err := firewallRule(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func ufwCalls(f *fakeSystem) []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "ufw ") {
			out = append(out, strings.TrimPrefix(c, "ufw "))
		}
	}
	return out
}

// fakeUFW keeps the rules a real ufw would list in "ufw show added".
func fakeUFW(f *fakeSystem) {
	var rules []string
	active := false
	run := f.run
	f.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name != "ufw" {
			return run(ctx, name, args...)
		}
		f.calls = append(f.calls, name+" "+strings.Join(args, " "))
		switch {
		case slices.Equal(args, []string{"show", "added"}):
			return []byte("Added user rules (see 'ufw status' for running firewall):\n" + strings.Join(rules, "\n")), nil
		case args[0] == "status":
			if active {
				return []byte("Status: active\nDefault: deny (incoming), allow (outgoing), disabled (routed)"), nil
			}
			return []byte("Status: inactive"), nil
		case args[0] == "delete":
			rules = slices.DeleteFunc(rules, func(r string) bool { return r == "ufw "+strings.Join(args[1:], " ")+" comment 'ekpanel'" })
		case args[0] == "--force":
			active = true
		case args[0] == "disable":
			active = false
		case args[0] == "allow" || args[0] == "prepend":
			r := strings.Join(slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "prepend" }), " ")
			rules = append(rules, "ufw "+strings.TrimSuffix(r, " comment ekpanel")+" comment 'ekpanel'")
		}
		return nil, nil
	}
}

func TestApplyFirewall(t *testing.T) {
	f := newFakeSystem(t)
	f.installed["ufw"] = true
	fakeUFW(f)
	spec := agentproto.FirewallSpec{Enabled: true, Rules: []agentproto.FirewallRuleSpec{
		{ID: "a", Action: "allow", Protocol: "tcp", PortFrom: 8080, PortTo: 8080},
		{ID: "b", Action: "deny", Protocol: "both", Source: "203.0.113.9"},
	}}
	res := applyFirewall(context.Background(), f.system, spec)
	if res.Error != "" || !slices.Equal(res.SSHPorts, []int{22}) {
		t.Fatalf("result %+v", res)
	}
	calls := ufwCalls(f)
	ssh := slices.Index(calls, "allow 22/tcp comment ekpanel")
	enable := slices.Index(calls, "--force enable")
	if ssh < 0 || enable < ssh || !slices.Contains(calls, "prepend deny from 203.0.113.9 comment ekpanel") ||
		!slices.Contains(calls, "allow 8080/tcp comment ekpanel") || !slices.Contains(calls, "default deny incoming") {
		t.Fatalf("ufw calls %q", calls)
	}

	// Dropping a rule deletes exactly that rule, and nothing ekPanel did not add.
	f.calls = nil
	spec.Rules = spec.Rules[1:]
	applyFirewall(context.Background(), f.system, spec)
	var deletes []string
	for _, c := range ufwCalls(f) {
		if strings.HasPrefix(c, "delete ") {
			deletes = append(deletes, c)
		}
	}
	if !slices.Equal(deletes, []string{"delete allow 8080/tcp"}) {
		t.Fatalf("deletes %q", deletes)
	}
	// Nothing changed and ufw is up: only reads, no reloads.
	f.calls = nil
	applyFirewall(context.Background(), f.system, spec)
	for _, c := range ufwCalls(f) {
		if c != "show added" && !strings.HasPrefix(c, "status") {
			t.Errorf("unchanged firewall ran ufw %s", c)
		}
	}

	f.calls = nil
	applyFirewall(context.Background(), f.system, agentproto.FirewallSpec{Enabled: false})
	if calls := ufwCalls(f); !slices.Contains(calls, "disable") || slices.Contains(calls, "--force enable") {
		t.Fatalf("turning off: %q", calls)
	}
}
