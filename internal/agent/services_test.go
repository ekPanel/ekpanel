package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"ekpanel/internal/agentproto"
)

const serviceID = "33333333-4444-5555-6666-777777777777"

func serviceApp() agentproto.AppSpec {
	return agentproto.AppSpec{
		ID: serviceID, Generation: 1, Name: "storefront", Kind: "service", Owner: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		User: "shop_ab12", Directory: "apps/front", Command: `pnpm start storefront --title "50% $off"`,
		Runtime: "node", RuntimeVersion: "24", HostPort: 20005, ContainerPort: 20005, Service: agentproto.AppServiceName,
		Domain: "shop.example.com", TLS: "auto", MemoryMB: 512, CPUs: 1.5,
		Env: []agentproto.EnvVar{{Key: "DATABASE_URL", Value: `postgres://u:p"w\d@db/x`}, {Key: "MULTI", Value: "a\nb"}},
	}
}

// serviceSystem is a fake server with the website account and folder of serviceApp, and
// Node.js 24 already installed.
func serviceSystem(t *testing.T) *fakeSystem {
	f := newFakeSystem(t)
	f.users["shop_ab12"] = []string{"shop_ab12", sitesGroup}
	os.MkdirAll(f.path("/home/shop_ab12/apps/front"), 0o750)
	os.MkdirAll(f.path(nodeDir+"/24/bin"), 0o755)
	os.WriteFile(f.path(nodeDir+"/24/bin/node"), nil, 0o755)
	return f
}

func TestValidateService(t *testing.T) {
	if err := validateApp(serviceApp()); err != nil {
		t.Fatal(err)
	}
	worker := serviceApp()
	worker.HostPort, worker.ContainerPort, worker.Domain, worker.Runtime, worker.RuntimeVersion = 0, 0, "", "", ""
	if err := validateApp(worker); err != nil {
		t.Fatalf("worker: %v", err)
	}
	fixed := serviceApp()
	fixed.HostPort, fixed.ContainerPort = 4007, 4007 // a port the program insists on
	if err := validateApp(fixed); err != nil {
		t.Fatalf("fixed port: %v", err)
	}
	for name, change := range map[string]func(*agentproto.AppSpec){
		"no account":        func(a *agentproto.AppSpec) { a.User = "" },
		"root":              func(a *agentproto.AppSpec) { a.User = "root" },
		"absolute folder":   func(a *agentproto.AppSpec) { a.Directory = "/etc" },
		"climbing folder":   func(a *agentproto.AppSpec) { a.Directory = "apps/../../other" },
		"empty command":     func(a *agentproto.AppSpec) { a.Command = "  " },
		"two lines":         func(a *agentproto.AppSpec) { a.Command = "node a.js\nrm -rf /" },
		"unknown node":      func(a *agentproto.AppSpec) { a.RuntimeVersion = "18" },
		"unknown runtime":   func(a *agentproto.AppSpec) { a.Runtime = "deno" },
		"privileged port":   func(a *agentproto.AppSpec) { a.HostPort, a.ContainerPort = 80, 80 },
		"domain no port":    func(a *agentproto.AppSpec) { a.HostPort, a.ContainerPort = 0, 0 },
		"port mismatch":     func(a *agentproto.AppSpec) { a.ContainerPort = 3000 },
		"version, no node":  func(a *agentproto.AppSpec) { a.Runtime = "" },
		"bad variable name": func(a *agentproto.AppSpec) { a.Env = []agentproto.EnvVar{{Key: "A B", Value: "x"}} },
	} {
		ap := serviceApp()
		change(&ap)
		if validateApp(ap) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestServiceUnit(t *testing.T) {
	unit := string(serviceUnit(serviceApp()))
	for _, want := range []string{
		"User=shop_ab12\n", "Group=shop_ab12\n", "WorkingDirectory=/home/shop_ab12/apps/front\n",
		`"PATH=` + nodeDir + `/24/bin:/usr/local/sbin`, `"PORT=20005"`, `"HOME=/home/shop_ab12"`,
		"EnvironmentFile=/srv/ekpanel/apps/storefront/env\n",
		// systemd would expand $ and %; the shell must get them as typed.
		`ExecStart=/bin/sh -c "pnpm start storefront --title \"50%% $$off\""` + "\n",
		"Slice=ekpanel-caaaaaaaabbbbccccddddeeeeeeeeeeee.slice\n", "MemoryMax=512M\n", "CPUQuota=150%\n",
		"Restart=always\n", "NoNewPrivileges=yes\n", "ProtectSystem=strict\n", "ProtectHome=tmpfs\n",
		"BindPaths=/home/shop_ab12\n", "CapabilityBoundingSet=\n", "PrivateDevices=yes\n",
		"WantedBy=multi-user.target\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	worker := serviceApp()
	worker.HostPort, worker.Runtime, worker.RuntimeVersion, worker.Owner, worker.MemoryMB, worker.CPUs, worker.Directory = 0, "", "", "", 0, 0, ""
	unit = string(serviceUnit(worker))
	for _, absent := range []string{"PORT=", nodeDir, "Slice=", "MemoryMax=", "CPUQuota="} {
		if strings.Contains(unit, absent) {
			t.Errorf("worker unit has %q:\n%s", absent, unit)
		}
	}
	if !strings.Contains(unit, "WorkingDirectory=/home/shop_ab12\n") {
		t.Errorf("no folder means the home folder:\n%s", unit)
	}
}

func TestServiceEnvFile(t *testing.T) {
	got := string(serviceEnv(serviceApp().Env))
	want := "DATABASE_URL=\"postgres://u:p\\\"w\\\\d@db/x\"\nMULTI=\"a\\nb\"\n"
	if !strings.HasSuffix(got, want) {
		t.Fatalf("env file:\n%s\nwant suffix:\n%s", got, want)
	}
}

func TestApplyServiceApp(t *testing.T) {
	f := serviceSystem(t)
	ap := serviceApp()
	res, opErr := stateApply(context.Background(), f.system, agentproto.StateApplyParams{Apps: []agentproto.AppSpec{ap}})
	if opErr != nil || len(res.Apps) != 1 || !res.Apps[0].OK {
		t.Fatalf("apply: %v %+v", opErr, res.Apps)
	}
	unit := agentproto.ServiceUnit("storefront")
	for _, want := range []string{"systemctl daemon-reload", "systemctl enable " + unit, "systemctl restart " + unit} {
		if !f.ran(want) {
			t.Errorf("did not run %q: %q", want, f.calls)
		}
	}
	if f.ran("docker") {
		t.Error("a service app needs no Docker")
	}
	env := f.path(appDir("storefront") + "/" + serviceEnvFile)
	if info, err := os.Stat(env); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the variables must be root's alone: %v", err)
	}
	if !f.exists(serviceUnitPath("storefront")) {
		t.Fatal("no unit")
	}
	caddy := f.read(t, appCaddyPath(serviceID))
	if !strings.Contains(caddy, "shop.example.com {") || !strings.Contains(caddy, "reverse_proxy 127.0.0.1:20005") {
		t.Fatalf("caddy file:\n%s", caddy)
	}

	// Nothing changed: started if it is not running, not restarted.
	f.calls = nil
	stateApply(context.Background(), f.system, agentproto.StateApplyParams{Apps: []agentproto.AppSpec{ap}})
	if f.ran("systemctl restart") || !f.ran("systemctl start "+unit) || f.ran("systemctl daemon-reload") {
		t.Errorf("unchanged run: %q", f.calls)
	}

	// A new variable restarts it.
	f.calls = nil
	ap.Env = append(ap.Env, agentproto.EnvVar{Key: "NEW", Value: "1"})
	stateApply(context.Background(), f.system, agentproto.StateApplyParams{Apps: []agentproto.AppSpec{ap}})
	if !f.ran("systemctl restart "+unit) || f.ran("systemctl daemon-reload") {
		t.Errorf("new variable: %q", f.calls)
	}

	// Stopped: disabled and stopped.
	f.calls = nil
	ap.Stopped = true
	stateApply(context.Background(), f.system, agentproto.StateApplyParams{Apps: []agentproto.AppSpec{ap}})
	if !f.ran("systemctl disable --now "+unit) || f.ran("systemctl start") {
		t.Errorf("stopped: %q", f.calls)
	}
	// Started again: restarted.
	f.calls = nil
	ap.Stopped = false
	stateApply(context.Background(), f.system, agentproto.StateApplyParams{Apps: []agentproto.AppSpec{ap}})
	if !f.ran("systemctl restart " + unit) {
		t.Errorf("started again: %q", f.calls)
	}

	// Deleted, even with its data: the unit and variables go, the website's files stay.
	f.calls = nil
	ap.Deleted, ap.DeleteData = true, true
	res, _ = stateApply(context.Background(), f.system, agentproto.StateApplyParams{Apps: []agentproto.AppSpec{ap}})
	if !res.Apps[0].OK || !f.ran("systemctl disable --now "+unit) || f.exists(serviceUnitPath("storefront")) || f.exists(appDir("storefront")) {
		t.Fatalf("delete: %+v %q", res.Apps, f.calls)
	}
	if !f.exists("/home/shop_ab12/apps/front") || f.exists(appCaddyPath(serviceID)) {
		t.Fatal("the website's files must stay, the domain must go")
	}
}

func TestServiceAppNeedsItsWebsite(t *testing.T) {
	f := serviceSystem(t)
	ap := serviceApp()
	ap.Directory = "missing"
	res, _ := stateApply(context.Background(), f.system, agentproto.StateApplyParams{Apps: []agentproto.AppSpec{ap}})
	if res.Apps[0].OK || !strings.Contains(res.Apps[0].Error, "upload the app there first") {
		t.Fatalf("missing folder: %+v", res.Apps)
	}
	ap = serviceApp()
	ap.User = "gone_ab12"
	res, _ = stateApply(context.Background(), f.system, agentproto.StateApplyParams{Apps: []agentproto.AppSpec{ap}})
	if res.Apps[0].OK || !strings.Contains(res.Apps[0].Error, "does not exist") {
		t.Fatalf("missing account: %+v", res.Apps)
	}
}

// nodeRelease makes a fake Node.js download: the archive and the server's answers.
func nodeRelease(t *testing.T) (map[string][]byte, string) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "node-v24.9.0/bin/node", Mode: 0o755, Size: 4, Typeflag: tar.TypeReg})
	tw.Write([]byte("node"))
	tw.Close()
	gz.Close()
	arch := nodeArch[runtime.GOARCH]
	file := "node-v24.9.0-linux-" + arch + ".tar.gz"
	sum := sha256.Sum256(buf.Bytes())
	return map[string][]byte{
		"https://nodejs.org/dist/index.json": []byte(`[{"version":"v26.1.0","files":["linux-x64","linux-arm64"]},
			{"version":"v24.9.0","files":["linux-x64","linux-arm64"]},{"version":"v24.8.0","files":["linux-x64","linux-arm64"]}]`),
		"https://nodejs.org/dist/v24.9.0/SHASUMS256.txt": []byte(hex.EncodeToString(sum[:]) + "  " + file + "\nabc  other.tar.gz\n"),
		"https://nodejs.org/dist/v24.9.0/" + file:        buf.Bytes(),
	}, file
}

func TestEnsureNode(t *testing.T) {
	f := newFakeSystem(t)
	files, file := nodeRelease(t)
	var fetched []string
	f.fetch = func(_ context.Context, url string) ([]byte, error) {
		fetched = append(fetched, url)
		if b, ok := files[url]; ok {
			return b, nil
		}
		return nil, errors.New("404")
	}
	var npm []string
	f.exec = func(_ context.Context, c command) ([]byte, error) {
		npm = append(npm, c.name+" "+strings.Join(c.args, " "))
		return nil, nil
	}
	run := f.run
	f.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "tar" { // unpack as tar would
			os.MkdirAll(filepath.Join(args[3], "bin"), 0o755)
			os.WriteFile(filepath.Join(args[3], "bin/node"), []byte("node"), 0o755)
		}
		return run(ctx, name, args...)
	}
	if err := ensureNode(context.Background(), f.system, "24"); err != nil {
		t.Fatal(err)
	}
	if got := f.read(t, nodeDir+"/24/"+nodeVersionTag); got != "v24.9.0\n" {
		t.Fatalf("installed %q, want the newest 24", got)
	}
	if !f.exists(nodeDir+"/24/bin/node") || f.exists(nodeDir+"/24.new") || f.exists(nodeDir+"/24/"+file) {
		t.Fatal("the release must be in place, without leftovers")
	}
	if len(npm) != 1 || !strings.Contains(npm[0], "--ignore-scripts pnpm@latest yarn@latest") {
		t.Fatalf("package managers: %q", npm)
	}
	// Installed: nothing is downloaded again.
	fetched = nil
	if err := ensureNode(context.Background(), f.system, "24"); err != nil || len(fetched) != 0 {
		t.Fatalf("second run fetched %q (%v)", fetched, err)
	}

	// A download that does not match its checksum is refused.
	g := newFakeSystem(t)
	files["https://nodejs.org/dist/v24.9.0/"+file] = []byte("tampered")
	g.fetch = f.fetch
	if err := ensureNode(context.Background(), g.system, "24"); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("tampered download: %v", err)
	}
	if g.exists(nodeDir + "/24") {
		t.Fatal("nothing may be installed from a bad download")
	}
}

func TestServiceStatusAndLogs(t *testing.T) {
	f := serviceSystem(t)
	stateApply(context.Background(), f.system, agentproto.StateApplyParams{Apps: []agentproto.AppSpec{serviceApp()}})
	run := f.run
	f.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "systemctl" && args[0] == "show" {
			return []byte("ActiveState=active\nSubState=running\nNRestarts=2\nMemoryCurrent=157286400\nActiveEnterTimestamp=Sat 2026-11-14 12:00:00 UTC\nMainPID=42\n"), nil
		}
		return run(ctx, name, args...)
	}
	st, opErr := appStatus(context.Background(), f.system, agentproto.AppParams{Name: "storefront"})
	if opErr != nil || len(st.Containers) != 1 {
		t.Fatalf("status: %v %+v", opErr, st)
	}
	c := st.Containers[0]
	if c.State != "running" || c.Name != agentproto.ServiceUnit("storefront") || !strings.Contains(c.Status, "2 restarts") || !strings.Contains(c.Status, "150 MB") {
		t.Fatalf("status %+v", c)
	}
	var journal []string
	f.exec = func(_ context.Context, cmd command) ([]byte, error) {
		journal = append(journal, cmd.name+" "+strings.Join(cmd.args, " "))
		cmd.stdout.Write([]byte("2026-11-14T12:00:00+0000 host ekpanel-app-storefront[42]: ready on 20005\n"))
		return nil, nil
	}
	logs, opErr := appLogs(context.Background(), f.system, agentproto.AppParams{Name: "storefront", Tail: 50})
	if opErr != nil || !strings.Contains(logs.Logs, "ready on 20005") || !strings.HasPrefix(journal[0], "journalctl --unit "+agentproto.ServiceUnit("storefront")+" --lines 50") {
		t.Fatalf("logs: %v %+v %q", opErr, logs, journal)
	}
	if _, opErr := appRestart(context.Background(), f.system, agentproto.AppParams{Name: "storefront"}); opErr != nil || !f.ran("systemctl restart "+agentproto.ServiceUnit("storefront")) {
		t.Fatalf("restart: %v", opErr)
	}
	if _, opErr := appPull(context.Background(), f.system, agentproto.AppParams{Name: "storefront"}); opErr == nil {
		t.Fatal("a service app has no images to pull")
	}
}

func TestServiceBuild(t *testing.T) {
	f := serviceSystem(t)
	ap := serviceApp()
	if res, _ := appBuild(context.Background(), f.system, agentproto.AppBuildParams{App: ap, Command: "pnpm install && pnpm build"}); res != nil {
		t.Fatal("a build before the app is set up must be refused")
	}
	stateApply(context.Background(), f.system, agentproto.StateApplyParams{Apps: []agentproto.AppSpec{ap}})
	var cmd command
	f.exec = func(_ context.Context, c command) ([]byte, error) {
		cmd = c
		c.stdout.Write([]byte("built in 3s\n"))
		return nil, nil
	}
	res, opErr := appBuild(context.Background(), f.system, agentproto.AppBuildParams{App: ap, Command: "pnpm install && pnpm build"})
	if opErr != nil || !res.OK || !strings.Contains(res.Log, "built in 3s") {
		t.Fatalf("build: %v %+v", opErr, res)
	}
	args := strings.Join(cmd.args, " ")
	for _, want := range []string{"--uid=shop_ab12", "--gid=shop_ab12", "--working-directory=/home/shop_ab12/apps/front", "--wait", "--pipe",
		"--property=Slice=ekpanel-c", "--property=MemoryMax=512M", "--property=ProtectSystem=strict", "--property=EnvironmentFile=/srv/ekpanel/apps/storefront/env",
		"--setenv=PATH=" + nodeDir + "/24/bin:", "/bin/sh -c pnpm install && pnpm build"} {
		if cmd.name != "systemd-run" || !strings.Contains(args, want) {
			t.Errorf("systemd-run lacks %q: %s %s", want, cmd.name, args)
		}
	}
	if strings.Contains(args, "postgres://") {
		t.Error("a variable value is on the command line")
	}
	f.exec = func(_ context.Context, c command) ([]byte, error) {
		c.stdout.Write([]byte("error TS2304\n"))
		return nil, errors.New("exit status 2")
	}
	res, _ = appBuild(context.Background(), f.system, agentproto.AppBuildParams{App: ap, Command: "pnpm build"})
	if res.OK || !strings.Contains(res.Log, "error TS2304") || !strings.Contains(res.Log, "The build failed") {
		t.Fatalf("failed build: %+v", res)
	}
}
