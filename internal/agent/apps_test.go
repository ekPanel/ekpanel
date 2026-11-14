package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"ekpanel/internal/agentproto"
)

const appID = "12345678-9abc-def0-1234-56789abcdef0"

// fakeDocker records docker commands and answers "config" like Compose would.
func fakeDocker(f *fakeSystem) *[]command {
	var calls []command
	f.exec = func(_ context.Context, c command) ([]byte, error) {
		calls = append(calls, c)
		if slices.Contains(c.args, "config") {
			return []byte(`{"services":{"app":{"ports":[{"host_ip":"127.0.0.1","published":"20001","target":80}]}}}`), nil
		}
		return nil, nil
	}
	return &calls
}

func composeVerbs(calls []command) []string {
	var out []string
	for _, c := range calls {
		i := slices.IndexFunc(c.args, func(a string) bool { return strings.HasSuffix(a, ".yaml") })
		for i+1 < len(c.args) && c.args[i+1] == "-f" {
			i += 2
		}
		out = append(out, strings.Join(c.args[i+1:], " "))
	}
	return out
}

func imageApp() agentproto.AppSpec {
	return agentproto.AppSpec{
		ID: appID, Generation: 1, Name: "uptime", Kind: "image", Image: "louislam/uptime-kuma:1",
		Service: agentproto.AppServiceName, ContainerPort: 3001, HostPort: 20001, Domain: "status.example.com", TLS: "auto",
		Env: []agentproto.EnvVar{{Key: "TZ", Value: "Asia/Dhaka"}, {Key: "PASS", Value: "p$ss word"}}, Volumes: []string{"/app/data"},
		MemoryMB: 512,
	}
}

func TestImageCompose(t *testing.T) {
	out := string(imageCompose(imageApp()))
	for _, want := range []string{`"image": "louislam/uptime-kuma:1"`, `"127.0.0.1:20001:3001"`, `"PASS": "p$$ss word"`,
		`"data0:/app/data"`, `"mem_limit": "512m"`, `"restart": "unless-stopped"`, `"max-size": "10m"`} {
		if !strings.Contains(out, want) {
			t.Errorf("compose file lacks %s:\n%s", want, out)
		}
	}
}

func TestCheckComposeConfig(t *testing.T) {
	ap := agentproto.AppSpec{Kind: "compose", Service: "web", ContainerPort: 80, HostPort: 20002}
	ok := `{"services":{"web":{"ports":[{"host_ip":"127.0.0.1","published":"20002","target":80}]},"db":{}}}`
	if err := checkComposeConfig([]byte(ok), ap); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`{"services":{"web":{"ports":[{"published":"8080","target":80}]}}}`,
		`{"services":{"web":{"ports":[{"host_ip":"0.0.0.0","published":"8080","target":80}]}}}`,
		`{"services":{"web":{"network_mode":"host"}}}`,
		`{"services":{"api":{}}}`, // no "web" service
		`{"services":{}}`,
	} {
		if err := checkComposeConfig([]byte(bad), ap); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	ap.PublicPorts = true
	if err := checkComposeConfig([]byte(`{"services":{"web":{"ports":[{"published":"25","target":25}]}}}`), ap); err != nil {
		t.Errorf("public ports allowed by the user: %v", err)
	}
}

func TestCustomerComposeApp(t *testing.T) {
	owner := "0e4c2f8a-1b2c-4d5e-8f90-123456789abc"
	slice := customerSlice(ownerKey(owner))
	ap := agentproto.AppSpec{Kind: "compose", Owner: owner, Service: "web", ContainerPort: 80, HostPort: 20003, MemoryMB: 256}

	out := string(composeOverride(ap, []string{"web", "db"}))
	if strings.Count(out, `"cgroup_parent": "`+slice+`"`) != 2 || !strings.Contains(out, `"127.0.0.1:20003:80"`) || !strings.Contains(out, `"256m"`) {
		t.Errorf("every service goes in the customer's slice, the port and limit stay on web:\n%s", out)
	}
	if composeOverride(agentproto.AppSpec{Kind: "compose", Service: "web"}, nil) != nil {
		t.Error("an administrator's app without port or limits needs no override")
	}
	args := []string{"compose", "-p", "x", "--project-directory", "/d", "-f", "/d/compose.yaml", "-f", "/d/" + appOverrideFile, "config", "--services"}
	if got := strings.Join(withoutOverride(args), " "); got != "compose -p x --project-directory /d -f /d/compose.yaml config --services" {
		t.Errorf("without override: %s", got)
	}

	svc := func(extra string) string {
		return `{"services":{"web":{"cgroup_parent":"` + slice + `","ports":[{"host_ip":"127.0.0.1","published":"20003","target":80}]` + extra + `},` +
			`"db":{"cgroup_parent":"` + slice + `","volumes":[{"type":"volume","source":"db","target":"/var/lib/postgresql/data"}]}}`
	}
	if err := checkComposeConfig([]byte(svc(``)+`,"volumes":{"db":{}}}`), ap); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		svc(`,"privileged":true`) + `}`,
		svc(`,"cap_add":["SYS_ADMIN"]`) + `}`,
		svc(`,"devices":[{"source":"/dev/sda","target":"/dev/sda"}]`) + `}`,
		svc(`,"pid":"host"`) + `}`,
		svc(`,"network_mode":"container:other"`) + `}`,
		svc(`,"security_opt":["apparmor=unconfined"]`) + `}`,
		svc(`,"volumes":[{"type":"bind","source":"/var/run/docker.sock","target":"/var/run/docker.sock"}]`) + `}`,
		svc(``) + `,"volumes":{"db":{"driver_opts":{"type":"none","o":"bind","device":"/"}}}}`,
		svc(``) + `,"volumes":{"db":{"external":true}}}`,
		svc(``) + `,"secrets":{"s":{"file":"/etc/shadow"}}}`,
		`{"services":{"web":{"ports":[{"host_ip":"127.0.0.1","published":"20003","target":80}]}}}`, // not in the slice
	} {
		if err := checkComposeConfig([]byte(bad), ap); err == nil {
			t.Errorf("accepted for a customer: %s", bad)
		}
	}
	ap.Owner = ""
	if err := checkComposeConfig([]byte(svc(`,"privileged":true`)+`}`), ap); err != nil {
		t.Errorf("administrators' Compose files may ask for privileges: %v", err)
	}
}

func TestValidateApp(t *testing.T) {
	if err := validateApp(imageApp()); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*agentproto.AppSpec){
		func(a *agentproto.AppSpec) { a.Name = "../etc" },
		func(a *agentproto.AppSpec) { a.Name = "Upper" },
		func(a *agentproto.AppSpec) { a.Image = "nginx latest" },
		func(a *agentproto.AppSpec) { a.Image = "nginx\n  privileged: true" },
		func(a *agentproto.AppSpec) { a.Env = []agentproto.EnvVar{{Key: "BAD KEY", Value: "x"}} },
		func(a *agentproto.AppSpec) { a.Volumes = []string{"/data/../../etc"} },
		func(a *agentproto.AppSpec) { a.HostPort = 80 },
		func(a *agentproto.AppSpec) { a.ContainerPort, a.HostPort = 0, 0 }, // domain without a port
		func(a *agentproto.AppSpec) { a.Domain = "bad domain" },
	} {
		ap := imageApp()
		change(&ap)
		if err := validateApp(ap); err == nil {
			t.Errorf("accepted %+v", ap)
		}
	}
}

func TestApplyApp(t *testing.T) {
	f := newFakeSystem(t)
	calls := fakeDocker(f)
	ap := imageApp()
	res, opErr := stateApply(context.Background(), f.system, agentproto.StateApplyParams{Apps: []agentproto.AppSpec{ap}})
	if opErr != nil || len(res.Apps) != 1 || !res.Apps[0].OK {
		t.Fatalf("apply: %v %+v", opErr, res.Apps)
	}
	dir := filepath.Join(f.root, appsDir, "uptime")
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Errorf("app folder mode %o", info.Mode().Perm())
	}
	for _, name := range []string{appComposeFile, appStateFile} {
		if info, err := os.Stat(filepath.Join(dir, name)); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s must be root-only", name)
		}
	}
	verbs := composeVerbs(*calls)
	if !slices.Contains(verbs, "config --format json") || !slices.Contains(verbs, "up --detach --remove-orphans --wait --wait-timeout 120") {
		t.Fatalf("compose calls %q", verbs)
	}
	for _, c := range *calls {
		if strings.Contains(strings.Join(c.args, " "), "p$ss") {
			t.Fatal("a variable value is on the command line")
		}
	}
	caddy, _ := os.ReadFile(filepath.Join(f.root, appCaddyPath(appID)))
	if !strings.Contains(string(caddy), "status.example.com {") || !strings.Contains(string(caddy), "reverse_proxy 127.0.0.1:20001") {
		t.Fatalf("caddy file:\n%s", caddy)
	}

	// Unchanged: no waiting, just make sure it is up.
	*calls = nil
	stateApply(context.Background(), f.system, agentproto.StateApplyParams{Apps: []agentproto.AppSpec{ap}})
	if verbs := composeVerbs(*calls); !slices.Contains(verbs, "up --detach --remove-orphans") {
		t.Fatalf("second run %q", verbs)
	}

	// Stopped apps stay stopped, even on drift runs.
	*calls = nil
	ap.Stopped, ap.Generation = true, 2
	stateApply(context.Background(), f.system, agentproto.StateApplyParams{Apps: []agentproto.AppSpec{ap}})
	if verbs := composeVerbs(*calls); !slices.Contains(verbs, "stop") || slices.ContainsFunc(verbs, func(v string) bool { return strings.HasPrefix(v, "up") }) {
		t.Fatalf("stopped app %q", verbs)
	}

	// Another app may not take over this one's folder.
	other := imageApp()
	other.ID = siteID
	other.Domain = "other.example.com"
	res, _ = stateApply(context.Background(), f.system, agentproto.StateApplyParams{Apps: []agentproto.AppSpec{ap, other}})
	if res.Apps[1].OK || !strings.Contains(res.Apps[1].Error, "belongs to another app") {
		t.Fatalf("takeover: %+v", res.Apps[1])
	}

	// Delete keeping data: containers go, volumes and folder stay.
	*calls = nil
	ap.Deleted, ap.Generation = true, 3
	res, _ = stateApply(context.Background(), f.system, agentproto.StateApplyParams{Apps: []agentproto.AppSpec{ap}})
	if !res.Apps[0].OK || !slices.Contains(composeVerbs(*calls), "down --remove-orphans") {
		t.Fatalf("delete: %+v %q", res.Apps, composeVerbs(*calls))
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("the folder (and its data) must stay")
	}
	if _, err := os.Stat(filepath.Join(f.root, appCaddyPath(appID))); err == nil {
		t.Fatal("caddy file must go")
	}

	// Delete with data, after re-creating.
	ap.Deleted, ap.Generation = false, 4
	stateApply(context.Background(), f.system, agentproto.StateApplyParams{Apps: []agentproto.AppSpec{ap}})
	*calls = nil
	ap.Deleted, ap.DeleteData, ap.Generation = true, true, 5
	stateApply(context.Background(), f.system, agentproto.StateApplyParams{Apps: []agentproto.AppSpec{ap}})
	if !slices.Contains(composeVerbs(*calls), "down --remove-orphans --volumes") {
		t.Fatalf("delete with data %q", composeVerbs(*calls))
	}
	if !slices.ContainsFunc(f.calls, func(c string) bool {
		return c == "docker volume ls --quiet --filter label=com.docker.compose.project=ekpanel-uptime"
	}) {
		t.Fatalf("left-over volumes not looked for: %q", f.calls)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("the folder must go with the data")
	}
}

func TestDiskUsage(t *testing.T) {
	f := newFakeSystem(t)
	home := filepath.Join(f.root, "home", "shop_ab12")
	os.MkdirAll(filepath.Join(home, "public_html"), 0o755)
	os.WriteFile(filepath.Join(home, "public_html", "big.bin"), make([]byte, 100<<10), 0o644)
	os.Link(filepath.Join(home, "public_html", "big.bin"), filepath.Join(home, "public_html", "same.bin"))
	os.Symlink("/", filepath.Join(home, "public_html", "root"))
	res, opErr := diskUsage(context.Background(), f.system, agentproto.DiskUsageParams{
		Sites: []agentproto.DiskUsageSite{{ID: siteID, User: "shop_ab12"}},
		Apps:  []agentproto.DiskUsageApp{{ID: appID, Name: "notthere"}},
	})
	if opErr != nil {
		t.Fatal(opErr)
	}
	got := res.Sites[siteID]
	if got < 100<<10 || got > 200<<10 {
		t.Fatalf("site uses %d bytes; want about 100 KB (hard link once, symlink not followed)", got)
	}
	if _, ok := res.Apps[appID]; ok {
		t.Fatal("an app that is not set up has no usage")
	}
	if _, opErr := diskUsage(context.Background(), f.system, agentproto.DiskUsageParams{Sites: []agentproto.DiskUsageSite{{ID: siteID, User: "root"}}}); opErr == nil {
		t.Fatal("measured root's home")
	}
}

// An app whose Compose file Compose no longer accepts can still be deleted: its containers
// go by the project's label.
func TestDeleteAppWithBrokenCompose(t *testing.T) {
	f := newFakeSystem(t)
	fakeDocker(f)
	ap := imageApp()
	stateApply(context.Background(), f.system, agentproto.StateApplyParams{Apps: []agentproto.AppSpec{ap}})
	f.exec = func(_ context.Context, c command) ([]byte, error) {
		if slices.Contains(c.args, "down") {
			return nil, errors.New(`service "app" refers to undefined volume data: invalid compose project`)
		}
		return nil, nil
	}
	f.calls = nil
	ap.Deleted, ap.Generation = true, 2
	res, opErr := stateApply(context.Background(), f.system, agentproto.StateApplyParams{Apps: []agentproto.AppSpec{ap}})
	if opErr != nil || len(res.Apps) != 1 || !res.Apps[0].OK {
		t.Fatalf("delete: %v %+v", opErr, res.Apps)
	}
	if !slices.Contains(f.calls, "docker ps --all --quiet --filter label=com.docker.compose.project=ekpanel-uptime") {
		t.Fatalf("containers not looked for by label: %q", f.calls)
	}
}
