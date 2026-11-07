package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"ekpanel/internal/agentproto"
)

func fakeSystemctl(t *testing.T) *[][]string {
	t.Helper()
	var calls [][]string
	orig := systemctl
	systemctl = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		if args[0] == "show" && strings.HasPrefix(args[1], "cron") {
			return []byte("LoadState=not-found\nActiveState=inactive\nSubState=dead\nActiveEnterTimestamp=\n"), nil
		}
		return []byte("LoadState=loaded\nActiveState=active\nSubState=running\nActiveEnterTimestamp=@1700000000\n"), nil
	}
	t.Cleanup(func() { systemctl = orig })
	return &calls
}

func TestServicesStatus(t *testing.T) {
	fakeSystemctl(t)
	res, opErr := Dispatch(context.Background(), agentproto.OpServicesStatus, nil)
	if opErr != nil {
		t.Fatal(opErr)
	}
	byName := map[string]agentproto.Service{}
	for _, s := range res.(agentproto.ServicesStatusResult).Services {
		byName[s.Name] = s
	}
	caddy := byName["caddy"]
	if !caddy.Installed || caddy.ActiveState != "active" || caddy.Since == nil || caddy.Since.Unix() != 1700000000 || !caddy.Restartable {
		t.Fatalf("caddy = %+v", caddy)
	}
	if byName["cron"].Installed || byName["cron"].Restartable {
		t.Fatalf("a missing unit must be reported as not installed and not restartable: %+v", byName["cron"])
	}
	if byName["ekpanel-agent"].Restartable {
		t.Fatal("the agent must not be restartable from the panel")
	}
}

func TestRestartOnlyAllowedServices(t *testing.T) {
	calls := fakeSystemctl(t)
	params, _ := json.Marshal(agentproto.ServicesRestartParams{Name: "caddy"})
	if _, opErr := Dispatch(context.Background(), agentproto.OpServicesRestart, params); opErr != nil {
		t.Fatal(opErr)
	}
	if got := strings.Join((*calls)[0], " "); got != "restart caddy.service" {
		t.Fatalf("ran systemctl %s", got)
	}
	for _, name := range []string{"ssh", "ekpanel", "caddy; rm -rf /", "../etc"} {
		params, _ := json.Marshal(agentproto.ServicesRestartParams{Name: name})
		if _, opErr := Dispatch(context.Background(), agentproto.OpServicesRestart, params); opErr == nil || opErr.Code != agentproto.ErrNotAllowed {
			t.Errorf("restart %q: want not_allowed, got %v", name, opErr)
		}
	}
	if len(*calls) != 1 {
		t.Fatalf("refused restarts must not run systemctl; calls: %v", *calls)
	}
}

func TestUnknownOperation(t *testing.T) {
	if _, opErr := Dispatch(context.Background(), "shell.exec", []byte(`{"cmd":"id"}`)); opErr == nil || opErr.Code != agentproto.ErrUnknownOp {
		t.Fatalf("want unknown_op, got %v", opErr)
	}
}

func TestNoSystemdIsReportedClearly(t *testing.T) {
	orig := systemctl
	systemctl = func(_ context.Context, args ...string) ([]byte, error) {
		return nil, errors.New("System has not been booted with systemd as init system (PID 1). Can't operate.")
	}
	defer func() { systemctl = orig }()
	if _, opErr := Dispatch(context.Background(), agentproto.OpServicesStatus, nil); opErr == nil || opErr.Code != agentproto.ErrNoSystemd {
		t.Fatalf("want no_systemd, got %v", opErr)
	}
}
