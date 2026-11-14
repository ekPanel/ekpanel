package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	"ekpanel/internal/agentproto"
)

// Service apps run a command under systemd, from a folder of a website, as the website's
// account: Node.js, Go and other programs, and workers. The unit is
// ekpanel-app-<name>.service; the app's folder in appsDir keeps only its state and its
// variables (root's, as for other apps). The program sees its own website's home folder and
// nothing of other accounts; it gets no privileges, and the owner's slice limits it.

const (
	nodeDir             = "/usr/local/lib/ekpanel/node" // one folder per major version
	nodeVersionTag      = ".ekpanel-version"
	serviceEnvFile      = "env"
	maxCommandLen       = 4096
	maxBuildLog         = 256 << 10
	serviceBuildTimeout = 30 * time.Minute
)

// serviceDirRE is a folder inside a home folder, written without a leading "/".
var serviceDirRE = regexp.MustCompile(`^[A-Za-z0-9._@+-]+(/[A-Za-z0-9._@+-]+)*$`)

var nodeArch = map[string]string{"amd64": "x64", "arm64": "arm64"}

func serviceUnitPath(name string) string { return systemdDir + "/" + agentproto.ServiceUnit(name) }

func serviceHome(user string) string { return "/home/" + user }

func serviceWorkDir(ap agentproto.AppSpec) string {
	if ap.Directory == "" {
		return serviceHome(ap.User)
	}
	return serviceHome(ap.User) + "/" + ap.Directory
}

func validateService(ap agentproto.AppSpec) error {
	switch {
	case !userRE.MatchString(ap.User) || slices.Contains(reserved, ap.User):
		return fmt.Errorf("invalid website account %q", ap.User)
	case ap.Directory != "" && (!serviceDirRE.MatchString(ap.Directory) || slices.Contains(strings.Split(ap.Directory, "/"), "..") || len(ap.Directory) > 1024):
		return fmt.Errorf("invalid folder %q", ap.Directory)
	case strings.TrimSpace(ap.Command) == "" || len(ap.Command) > maxCommandLen || strings.ContainsAny(ap.Command, "\x00\r\n"):
		return errors.New("the command must be one line")
	case ap.Runtime == "" && ap.RuntimeVersion != "":
		return errors.New("a runtime version needs a runtime")
	case ap.Runtime == "node" && !slices.Contains(agentproto.NodeVersions, ap.RuntimeVersion):
		return fmt.Errorf("unsupported Node.js version %q", ap.RuntimeVersion)
	case ap.Runtime != "" && ap.Runtime != "node":
		return fmt.Errorf("unknown runtime %q", ap.Runtime)
	case ap.HostPort != 0 && (ap.HostPort < 1024 || ap.HostPort > 65535 || ap.ContainerPort != ap.HostPort):
		return errors.New("invalid port")
	}
	return nil
}

// systemdArg quotes s as one argument of a unit's command line: systemd expands "$" and
// "%" there, which must reach the shell as they are.
func systemdArg(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`)
	return `"` + r.Replace(s) + `"`
}

// envFileValue quotes a value for an EnvironmentFile, where a double-quoted value may hold
// any character and only \, " and newlines need care.
func envFileValue(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

func serviceEnv(env []agentproto.EnvVar) []byte {
	var b bytes.Buffer
	b.WriteString("# Managed by ekPanel. Changes here are overwritten.\n")
	for _, e := range env {
		b.WriteString(e.Key + "=" + envFileValue(e.Value) + "\n")
	}
	return b.Bytes()
}

// servicePath is the PATH of a service: its Node.js first, then the system's programs.
func servicePath(ap agentproto.AppSpec) string {
	p := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	if ap.Runtime == "node" {
		p = nodeDir + "/" + ap.RuntimeVersion + "/bin:" + p
	}
	return p
}

// serviceSandbox is what a service and its builds may not do, as systemd properties.
func serviceSandbox(user string) []string {
	return []string{
		"UMask=0027", "NoNewPrivileges=yes", "PrivateTmp=yes", "PrivateDevices=yes",
		"ProtectSystem=strict", "ProtectHome=tmpfs", "BindPaths=" + serviceHome(user),
		"ProtectKernelTunables=yes", "ProtectKernelModules=yes", "ProtectKernelLogs=yes",
		"ProtectControlGroups=yes", "ProtectClock=yes", "ProtectHostname=yes", "RestrictSUIDSGID=yes",
		"RestrictRealtime=yes", "RestrictNamespaces=yes", "LockPersonality=yes",
		"SystemCallArchitectures=native", "CapabilityBoundingSet=", "TasksMax=1024",
	}
}

// serviceLimits are the app's own limits; its owner's slice limits all their things together.
func serviceLimits(ap agentproto.AppSpec) []string {
	var out []string
	if ap.Owner != "" {
		out = append(out, "Slice="+customerSlice(ownerKey(ap.Owner)))
	}
	if ap.MemoryMB > 0 {
		out = append(out, "MemoryMax="+strconv.Itoa(ap.MemoryMB)+"M")
	}
	if ap.CPUs > 0 {
		out = append(out, "CPUQuota="+strconv.Itoa(int(ap.CPUs*100+0.5))+"%")
	}
	return out
}

var serviceUnitTemplate = template.Must(template.New("service").Parse(`# Managed by ekPanel (app {{.ID}}). Changes here are overwritten.
[Unit]
Description=ekPanel app {{.Name}}
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
User={{.User}}
Group={{.User}}
WorkingDirectory={{.Dir}}
Environment="HOME={{.Home}}" "PATH={{.Path}}"{{if .Port}} "PORT={{.Port}}"{{end}}
EnvironmentFile={{.EnvFile}}
ExecStart=/bin/sh -c {{.Command}}
Restart=always
RestartSec=3
TimeoutStopSec=20
SyslogIdentifier=ekpanel-app-{{.Name}}
{{- range .Properties}}
{{.}}
{{- end}}

[Install]
WantedBy=multi-user.target
`))

func serviceUnit(ap agentproto.AppSpec) []byte {
	var b bytes.Buffer
	serviceUnitTemplate.Execute(&b, map[string]any{
		"ID": ap.ID, "Name": ap.Name, "User": ap.User, "Dir": serviceWorkDir(ap), "Home": serviceHome(ap.User),
		"Path": servicePath(ap), "Port": ap.HostPort, "EnvFile": appDir(ap.Name) + "/" + serviceEnvFile,
		"Command": systemdArg(ap.Command), "Properties": append(serviceLimits(ap), serviceSandbox(ap.User)...),
	})
	return b.Bytes()
}

// ensureService writes a service app's unit and variables and starts, restarts or stops it.
func (a *applier) ensureService(ctx context.Context, ap agentproto.AppSpec) error {
	if st, err := readAppState(a.sys, ap.Name); err == nil && st.ID != ap.ID {
		return fmt.Errorf("the folder %s belongs to another app", appDir(ap.Name))
	}
	if _, err := a.sys.run(ctx, "id", "-u", ap.User); err != nil {
		return fmt.Errorf("the website account %s does not exist on this server", ap.User)
	}
	work := serviceWorkDir(ap)
	if info, err := os.Stat(a.sys.path(work)); err != nil || !info.IsDir() {
		return fmt.Errorf("the folder %s does not exist; upload the app there first", work)
	}
	if ap.Runtime == "node" {
		if err := a.nodeRuntime(ctx, ap.RuntimeVersion); err != nil {
			return err
		}
	}
	dir := a.sys.path(appDir(ap.Name))
	if err := os.MkdirAll(a.sys.path(appsDir), 0o711); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	envChanged, err := writeSecretIfChanged(dir+"/"+serviceEnvFile, serviceEnv(ap.Env))
	if err != nil {
		return err
	}
	unitChanged, _, err := a.writeIfChanged(serviceUnitPath(ap.Name), serviceUnit(ap))
	if err != nil {
		return err
	}
	prev, _ := readAppState(a.sys, ap.Name)
	st, _ := json.Marshal(appState{ID: ap.ID, Kind: "service", Stopped: ap.Stopped, Generation: ap.Generation})
	if err := os.WriteFile(dir+"/"+appStateFile, st, 0o600); err != nil {
		return err
	}
	unit := agentproto.ServiceUnit(ap.Name)
	if unitChanged {
		if _, err := a.sys.run(ctx, "systemctl", "daemon-reload"); err != nil {
			return err
		}
	}
	if ap.Stopped {
		if _, err := a.sys.run(ctx, "systemctl", "disable", "--now", unit); err != nil {
			return err
		}
	} else {
		if _, err := a.sys.run(ctx, "systemctl", "enable", unit); err != nil {
			return err
		}
		verb := "start" // a no-op when it already runs
		if unitChanged || envChanged || prev.Stopped {
			verb = "restart"
		}
		if _, err := a.sys.run(ctx, "systemctl", verb, unit); err != nil {
			return fmt.Errorf("the app did not start: %w", err)
		}
	}
	return a.writeAppCaddy(ap, nil)
}

// removeService stops a service app and removes its unit and variables. Its code stays in
// the website, whatever the user chose for the data.
func (a *applier) removeService(ctx context.Context, name string) error {
	unit := agentproto.ServiceUnit(name)
	if _, err := os.Stat(a.sys.path(serviceUnitPath(name))); err == nil {
		if _, err := a.sys.run(ctx, "systemctl", "disable", "--now", unit); err != nil {
			return err
		}
		if err := os.Remove(a.sys.path(serviceUnitPath(name))); err != nil {
			return err
		}
		a.sys.run(ctx, "systemctl", "daemon-reload")
	}
	return os.RemoveAll(a.sys.path(appDir(name)))
}

// nodeRuntime installs a Node.js major version once per run (see ensureNode).
func (a *applier) nodeRuntime(ctx context.Context, major string) error {
	if a.nodeReady == nil {
		a.nodeReady = map[string]error{}
	}
	err, done := a.nodeReady[major]
	if !done {
		err = ensureNode(ctx, a.sys, major)
		a.nodeReady[major] = err
	}
	return err
}

// ensureNode installs the newest release of a Node.js major version from nodejs.org, checked
// against the checksums published with it, with pnpm and Yarn. It is kept in
// nodeDir/<major>; a version already there is left as it is.
func ensureNode(ctx context.Context, sys *system, major string) error {
	home := nodeDir + "/" + major
	if _, err := os.Stat(sys.path(home + "/bin/node")); err == nil {
		return nil
	}
	arch := nodeArch[runtime.GOARCH]
	if arch == "" {
		return fmt.Errorf("Node.js is not available for %s", runtime.GOARCH)
	}
	index, err := sys.fetch(ctx, "https://nodejs.org/dist/index.json")
	if err != nil {
		return fmt.Errorf("list Node.js releases: %w", err)
	}
	var releases []struct {
		Version string   `json:"version"`
		Files   []string `json:"files"`
	}
	if err := json.Unmarshal(index, &releases); err != nil {
		return fmt.Errorf("read the Node.js release list: %w", err)
	}
	version := ""
	for _, r := range releases { // newest first
		if strings.HasPrefix(r.Version, "v"+major+".") && slices.Contains(r.Files, "linux-"+arch) {
			version = r.Version
			break
		}
	}
	if version == "" {
		return fmt.Errorf("no Node.js %s release for linux-%s", major, arch)
	}
	file := "node-" + version + "-linux-" + arch + ".tar.gz"
	sums, err := sys.fetch(ctx, "https://nodejs.org/dist/"+version+"/SHASUMS256.txt")
	if err != nil {
		return fmt.Errorf("download Node.js checksums: %w", err)
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == file {
			want = f[0]
		}
	}
	if want == "" {
		return fmt.Errorf("Node.js %s publishes no checksum for %s", version, file)
	}
	archive, err := sys.fetch(ctx, "https://nodejs.org/dist/"+version+"/"+file)
	if err != nil {
		return fmt.Errorf("download Node.js %s: %w", version, err)
	}
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("the Node.js %s download does not match its published checksum", version)
	}
	tmp := sys.path(home + ".new")
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	tarball := tmp + "/" + file
	if err := os.WriteFile(tarball, archive, 0o600); err != nil {
		return err
	}
	if _, err := sys.run(ctx, "tar", "-xzf", tarball, "-C", tmp, "--strip-components=1", "--no-same-owner"); err != nil {
		return fmt.Errorf("unpack Node.js: %w", err)
	}
	os.Remove(tarball)
	// pnpm and Yarn, as Node.js no longer brings them (Corepack left it in version 25).
	_, err = sys.exec(ctx, command{
		name: tmp + "/bin/npm",
		args: []string{"install", "--global", "--prefix", tmp, "--no-audit", "--no-fund", "--ignore-scripts", "pnpm@latest", "yarn@latest"},
		env:  []string{"PATH=" + tmp + "/bin:/usr/bin:/bin", "HOME=" + tmp, "npm_config_update_notifier=false", "npm_config_cache=" + tmp + "/.npm"},
	})
	if err != nil {
		return fmt.Errorf("install pnpm and Yarn: %w", err)
	}
	os.RemoveAll(tmp + "/.npm")
	if err := os.WriteFile(tmp+"/"+nodeVersionTag, []byte(version+"\n"), 0o644); err != nil {
		return err
	}
	os.RemoveAll(sys.path(home))
	return os.Rename(tmp, sys.path(home))
}

// ---- Operations on one service app ----

// serviceStatus describes the app's process as its one "container".
func serviceStatus(ctx context.Context, sys *system, name string) (*agentproto.AppStatusResult, *agentproto.OpError) {
	unit := agentproto.ServiceUnit(name)
	out, err := sys.run(ctx, "systemctl", "show", unit, "--property=ActiveState,SubState,NRestarts,MemoryCurrent,ActiveEnterTimestamp,MainPID")
	if err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	props := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	state := props["ActiveState"]
	switch state {
	case "active":
		state = "running"
	case "activating", "deactivating", "reloading":
		state = "restarting"
	case "inactive", "failed", "":
		state = "exited"
	}
	var status []string
	if props["ActiveState"] == "active" && props["ActiveEnterTimestamp"] != "" {
		status = append(status, "since "+props["ActiveEnterTimestamp"])
	} else if props["SubState"] != "" {
		status = append(status, props["SubState"])
	}
	if n, err := strconv.Atoi(props["NRestarts"]); err == nil && n > 0 {
		status = append(status, strconv.Itoa(n)+" restarts")
	}
	if b, err := strconv.ParseInt(props["MemoryCurrent"], 10, 64); err == nil && b > 0 {
		status = append(status, strconv.FormatInt(b>>20, 10)+" MB")
	}
	return &agentproto.AppStatusResult{Containers: []agentproto.AppContainer{{
		Service: agentproto.AppServiceName, Name: unit, State: state, Status: strings.Join(status, ", "),
	}}}, nil
}

func serviceLogs(ctx context.Context, sys *system, name string, tail int) (*agentproto.AppLogsResult, *agentproto.OpError) {
	var buf bytes.Buffer
	stderr, err := sys.exec(ctx, command{
		name: "journalctl", args: []string{"--unit", agentproto.ServiceUnit(name), "--lines", strconv.Itoa(tail), "--no-pager", "--output", "short-iso"},
		stdout: &buf,
	})
	if err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	logs := append(buf.Bytes(), stderr...)
	res := &agentproto.AppLogsResult{}
	if len(logs) > maxLogBytes {
		logs, res.Truncated = logs[len(logs)-maxLogBytes:], true
	}
	res.Logs = strings.ToValidUTF8(string(logs), "�")
	return res, nil
}

func serviceRestart(ctx context.Context, sys *system, name string) (any, *agentproto.OpError) {
	if _, err := sys.run(ctx, "systemctl", "restart", agentproto.ServiceUnit(name)); err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	return struct{}{}, nil
}

// appBuild runs a service app's build command once, as the app would run, and returns
// what it printed. It runs in a unit of its own that stops after serviceBuildTimeout.
func appBuild(ctx context.Context, sys *system, p agentproto.AppBuildParams) (*agentproto.AppBuildResult, *agentproto.OpError) {
	ap := p.App
	ap.Command = p.Command
	if err := validateApp(ap); err != nil || ap.Kind != "service" {
		return nil, opError(agentproto.ErrInvalidParams, "invalid build: %v", err)
	}
	applyMu.Lock() // the runtime may be installing for a state.apply
	st, err := readAppState(sys, ap.Name)
	if err != nil || st.ID != ap.ID {
		applyMu.Unlock()
		return nil, opError(agentproto.ErrNotAllowed, "the app is not set up on this server yet")
	}
	if ap.Runtime == "node" {
		err = ensureNode(ctx, sys, ap.RuntimeVersion)
	}
	applyMu.Unlock()
	if err != nil {
		return &agentproto.AppBuildResult{Log: err.Error() + "\n"}, nil
	}
	if info, err := os.Stat(sys.path(serviceWorkDir(ap))); err != nil || !info.IsDir() {
		return &agentproto.AppBuildResult{Log: "the folder " + serviceWorkDir(ap) + " does not exist\n"}, nil
	}
	args := []string{
		"--unit=ekpanel-build-" + ap.Name, "--description=ekPanel build of " + ap.Name,
		"--wait", "--pipe", "--collect", "--quiet", "--uid=" + ap.User, "--gid=" + ap.User,
		"--working-directory=" + serviceWorkDir(ap),
		"--setenv=HOME=" + serviceHome(ap.User), "--setenv=PATH=" + servicePath(ap),
		"--property=EnvironmentFile=" + appDir(ap.Name) + "/" + serviceEnvFile,
		"--property=RuntimeMaxSec=" + strconv.Itoa(int(serviceBuildTimeout.Seconds())),
	}
	for _, prop := range append(serviceLimits(ap), serviceSandbox(ap.User)...) {
		args = append(args, "--property="+prop)
	}
	args = append(args, "/bin/sh", "-c", ap.Command)
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(ctx, serviceBuildTimeout+time.Minute)
	defer cancel()
	_, runErr := sys.exec(ctx, command{name: "systemd-run", args: args, stdout: &out, stderr: &out})
	log := out.Bytes()
	res := &agentproto.AppBuildResult{OK: runErr == nil}
	if runErr != nil {
		msg := "The build failed"
		var exitErr *exec.ExitError
		switch {
		case errors.As(runErr, &exitErr):
			msg += fmt.Sprintf(" (exit code %d)", exitErr.ExitCode())
		case ctx.Err() != nil:
			msg += " (it took too long)"
		}
		log = append(log, []byte("\n"+msg+".\n")...)
	}
	if len(log) > maxBuildLog {
		log, res.Truncated = log[len(log)-maxBuildLog:], true
	}
	res.Log = strings.ToValidUTF8(string(log), "�")
	return res, nil
}
