package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ekpanel/internal/agentproto"
)

func TestSyncAllowlist(t *testing.T) {
	f := newFakeSystem(t)
	run := f.system.run
	f.system.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "cscli" && len(args) > 1 && args[1] == "inspect" {
			f.calls = append(f.calls, name+" "+strings.Join(args, " "))
			return []byte(`{"name":"ekpanel","items":[{"value":"198.51.100.0/24"},{"value":"173.245.48.0/20"},{"value":"192.0.2.1"}]}`), nil
		}
		return run(ctx, name, args...)
	}
	if err := syncAllowlist(context.Background(), f.system, []string{"203.0.113.9", "198.51.100.0/24"}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, "cscli allowlists remove ekpanel 192.0.2.1") {
		t.Errorf("an entry no longer wanted must go:\n%s", joined)
	}
	if !strings.Contains(joined, "cscli allowlists add ekpanel") || !strings.Contains(joined, "203.0.113.9") || !strings.Contains(joined, "2606:4700::/32") ||
		strings.Contains(joined, "add ekpanel 173.245.48.0/20") {
		t.Errorf("missing entries (and only those) must be added:\n%s", joined)
	}
}

func TestSecurityOffAndOverview(t *testing.T) {
	f := newFakeSystem(t)
	f.installed["crowdsec"] = true
	run := f.system.run
	f.system.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "sshd":
			return []byte("port 22\nport 2222\npermitrootlogin prohibit-password\npasswordauthentication yes\n"), nil
		case "apt-get":
			return []byte("Inst libssl3 [3.0.13] (3.0.13-0ubuntu3.6 Ubuntu:24.04/noble-security [amd64])\nInst vim [2:9.1] (2:9.1.1 Ubuntu:24.04/noble-updates [amd64])\n"), nil
		}
		return run(ctx, name, args...)
	}
	res := applySecurity(context.Background(), f.system, agentproto.SecuritySpec{CrowdSec: false})
	if res.Error != "" || !strings.Contains(strings.Join(f.calls, "\n"), "systemctl disable --now crowdsec-firewall-bouncer crowdsec") {
		t.Errorf("turning CrowdSec off: %+v\n%s", res, strings.Join(f.calls, "\n"))
	}
	if res := applySecurity(context.Background(), f.system, agentproto.SecuritySpec{CrowdSec: true, Allowlist: []string{"10.0.0.1/8"}}); res.Error == "" {
		t.Error("an allowlist entry that is not in canonical form must be refused")
	}
	os.MkdirAll(filepath.Join(f.root, "run"), 0o755)
	os.WriteFile(filepath.Join(f.root, "run/reboot-required"), nil, 0o644)
	o, opErr := securityOverview(context.Background(), f.system)
	if opErr != nil {
		t.Fatal(opErr)
	}
	if len(o.SSH.Ports) != 2 || o.SSH.RootLogin != "prohibit-password" || !o.SSH.PasswordAuth || o.Updates.Pending != 2 || o.Updates.Security != 1 || !o.Updates.RebootRequired {
		t.Errorf("overview: %+v", o)
	}
}

func TestBanParams(t *testing.T) {
	ok := agentproto.BanParams{IP: "198.51.100.5", Duration: "4h", Reason: "Spam"}
	if ok.Validate() != nil {
		t.Fatal("a good ban refused")
	}
	for _, ip := range []string{"10.1.2.3", "127.0.0.1", "198.51.100.0/24", "::1", "fe80::1", "198.051.100.5", "-rf"} {
		b := ok
		b.IP = ip
		if b.Validate() == nil {
			t.Errorf("%s accepted", ip)
		}
	}
}
