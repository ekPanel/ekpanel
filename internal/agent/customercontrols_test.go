package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ekpanel/internal/agentproto"
)

func TestBlockedSiteAndApp(t *testing.T) {
	f := newFakeSystem(t)
	sp := phpSite()
	sp.Blocked = agentproto.BlockedSuspended
	if res := apply(t, f, sp); !res[0].OK {
		t.Fatalf("apply: %s", res[0].Error)
	}
	caddy := f.read(t, caddySitePath(siteID))
	for _, want := range []string{"example.com, www.example.com {", "This website is suspended.", "` 503", "Retry-After",
		"output file /var/log/caddy/site-" + siteID + ".log"} {
		if !strings.Contains(caddy, want) {
			t.Errorf("blocked site lacks %q:\n%s", want, caddy)
		}
	}
	for _, not := range []string{"php_fastcgi", "file_server", "root *"} {
		if strings.Contains(caddy, not) {
			t.Errorf("a blocked site still serves (%q):\n%s", not, caddy)
		}
	}
	sp.Blocked = agentproto.BlockedBandwidth
	apply(t, f, sp)
	if caddy := f.read(t, caddySitePath(siteID)); !strings.Contains(caddy, "used up its bandwidth") {
		t.Errorf("bandwidth page missing:\n%s", caddy)
	}
	sp.Blocked = ""
	apply(t, f, sp)
	if caddy := f.read(t, caddySitePath(siteID)); !strings.Contains(caddy, "php_fastcgi") || strings.Contains(caddy, "503") {
		t.Errorf("unblocked site:\n%s", caddy)
	}
	sp.Blocked = "closed`\n\trespond 200"
	if res := apply(t, f, sp); res[0].OK {
		t.Fatal("an unknown block was accepted")
	}

	fakeDocker(f)
	ap := imageApp()
	ap.Blocked, ap.Stopped = agentproto.BlockedSuspended, true
	res, opErr := stateApply(context.Background(), f.system, agentproto.StateApplyParams{Apps: []agentproto.AppSpec{ap}})
	if opErr != nil || !res.Apps[0].OK {
		t.Fatalf("apply app: %v %+v", opErr, res.Apps)
	}
	appCaddy, _ := os.ReadFile(filepath.Join(f.root, appCaddyPath(appID)))
	if !strings.Contains(string(appCaddy), "status.example.com {") || !strings.Contains(string(appCaddy), "This website is suspended.") ||
		strings.Contains(string(appCaddy), "reverse_proxy") {
		t.Fatalf("blocked app:\n%s", appCaddy)
	}
}

func TestCronResults(t *testing.T) {
	f := newFakeSystem(t)
	const a, b, c = "11111111-2222-3333-4444-555555555555", "21111111-2222-3333-4444-555555555555", "31111111-2222-3333-4444-555555555555"
	os.MkdirAll(f.path(systemdDir), 0o755)
	for _, id := range []string{a, b} {
		os.WriteFile(f.path(systemdDir+"/"+cronUnit(id)+".service"), nil, 0o644)
	}
	run := f.run
	var shown string
	f.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "systemctl" && args[0] == "show" {
			shown = strings.Join(args, " ")
			return []byte("Id=" + cronUnit(a) + ".service\nActiveState=failed\nResult=exit-code\nExecMainStartTimestamp=@1700000000\n" +
				"ExecMainExitTimestamp=@1700000005\nExecMainStatus=3\n\n" +
				"Id=" + cronUnit(b) + ".service\nActiveState=activating\nResult=success\nExecMainStartTimestamp=@1700000000\n" +
				"ExecMainExitTimestamp=\nExecMainStatus=0\n"), nil
		}
		return run(ctx, name, args...)
	}
	out, opErr := cronResults(context.Background(), f.system, agentproto.CronResultsParams{IDs: []string{a, b, c}})
	if opErr != nil {
		t.Fatal(opErr)
	}
	jobs := out.(agentproto.CronResultsResult).Jobs
	if len(jobs) != 1 || jobs[0].ID != a || jobs[0].Result != "exit-code" || jobs[0].ExitCode != 3 || jobs[0].Ended.Unix() != 1700000005 {
		t.Fatalf("results %+v", jobs)
	}
	if !strings.HasPrefix(shown, "show --timestamp=unix") || !strings.Contains(shown, cronUnit(b)) || strings.Contains(shown, cronUnit(c)) {
		t.Errorf("systemctl %s", shown)
	}
	if _, opErr := cronResults(context.Background(), f.system, agentproto.CronResultsParams{IDs: []string{"../x"}}); opErr == nil {
		t.Fatal("a bad id was accepted")
	}
}
