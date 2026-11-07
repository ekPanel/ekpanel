package agent

import (
	"context"
	"slices"
	"strings"
	"testing"

	"ekpanel/internal/agentproto"
)

func TestPHPExtensions(t *testing.T) {
	f := newFakeSystem(t)
	run := f.system.run
	f.system.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		switch {
		case name == "dpkg-query" && args[len(args)-1] == "php8.4-*":
			f.calls = append(f.calls, call)
			return []byte("php8.4-fpm install ok installed\nphp8.4-cli install ok installed\nphp8.4-redis install ok installed\nphp8.4-imap deinstall ok config-files\n"), nil
		case name == "dpkg-query" && strings.HasPrefix(args[len(args)-1], "php"):
			f.calls = append(f.calls, call)
			return nil, nil
		case name == "systemctl" && args[0] == "list-units":
			f.calls = append(f.calls, call)
			return []byte("php8.4-fpm.service loaded active running PHP\nekpanel-php8.4-cabc.service loaded active running PHP\n"), nil
		}
		return run(ctx, name, args...)
	}
	info, opErr := phpInfo(context.Background(), f.system)
	if opErr != nil || len(info.Versions) != 1 || info.Versions[0].Version != "8.4" {
		t.Fatalf("info: %v %+v", opErr, info)
	}
	got := map[string]agentproto.PHPExtensionInfo{}
	for _, e := range info.Versions[0].Extensions {
		got[e.Name] = e
	}
	if !got["redis"].Installed || got["imap"].Installed || !got["fpm"].Required || got["redis"].Required {
		t.Errorf("extensions: %+v", got)
	}

	f.calls = nil
	if _, opErr := phpExtension(context.Background(), f.system, agentproto.PHPExtensionParams{Version: "8.4", Name: "mongodb", Installed: true}); opErr != nil {
		t.Fatal(opErr)
	}
	for _, want := range []string{"apt-get -o DPkg::Lock::Timeout=300 install -y --no-install-recommends php8.4-mongodb",
		"systemctl reload-or-restart php8.4-fpm.service", "systemctl reload-or-restart ekpanel-php8.4-cabc.service"} {
		if !slices.Contains(f.calls, want) {
			t.Errorf("missing %q in %q", want, f.calls)
		}
	}
	for _, bad := range []agentproto.PHPExtensionParams{{Version: "8.4", Name: "fpm"}, {Version: "8.4", Name: "x; rm -rf /"}, {Version: "8.3", Name: "redis", Installed: true}} {
		if _, opErr := phpExtension(context.Background(), f.system, bad); opErr == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}
