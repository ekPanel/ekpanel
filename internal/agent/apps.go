package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"ekpanel/internal/agentproto"
)

// Each app lives in appsDir/<name>: the Compose file, ekPanel's override that publishes the
// app's port on 127.0.0.1, and ekpanel.json (the app's ID, environment and whether it is
// stopped). The folder is root's alone because Compose files and variables hold secrets.
const (
	appsDir         = "/srv/ekpanel/apps"
	appStateFile    = "ekpanel.json"
	appComposeFile  = "compose.yaml"
	appOverrideFile = "compose.ekpanel.yaml"
	dockerDaemonCfg = "/etc/docker/daemon.json"
	maxComposeBytes = 256 << 10
	maxLogBytes     = 1 << 20
)

var (
	appNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}[a-z0-9]$`)
	// Docker validates image references itself; this only keeps out whitespace and anything
	// that is not part of a reference. The value is written as JSON, so it cannot inject.
	imageRE     = regexp.MustCompile(`^[a-z0-9][a-zA-Z0-9._/:@-]{0,254}$`)
	envKeyRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	serviceRE   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)
	volumePathR = regexp.MustCompile(`^/[A-Za-z0-9._/-]{0,250}$`)
)

func appDir(name string) string     { return appsDir + "/" + name }
func appProject(name string) string { return "ekpanel-" + name }
func appCaddyPath(id string) string { return caddySitesDir + "/app-" + id + ".caddy" }

// appState is what the agent keeps about an app between runs.
type appState struct {
	ID      string              `json:"id"`
	Stopped bool                `json:"stopped"`
	Env     []agentproto.EnvVar `json:"env"`
}

func validateApp(ap agentproto.AppSpec) error {
	if !uuidRE.MatchString(ap.ID) {
		return errors.New("invalid app id")
	}
	if !appNameRE.MatchString(ap.Name) {
		return fmt.Errorf("invalid app name %q", ap.Name)
	}
	if ap.Owner != "" && !uuidRE.MatchString(ap.Owner) {
		return errors.New("invalid owner")
	}
	if ap.Deleted {
		return nil
	}
	switch ap.Kind {
	case "image":
		if !imageRE.MatchString(ap.Image) || strings.Contains(ap.Image, "..") {
			return fmt.Errorf("invalid image %q", ap.Image)
		}
		if ap.ContainerPort != 0 && ap.Service != agentproto.AppServiceName {
			return errors.New("an image app has one service, " + agentproto.AppServiceName)
		}
		for _, v := range ap.Volumes {
			if !volumePathR.MatchString(v) || strings.Contains(v, "..") {
				return fmt.Errorf("invalid volume path %q", v)
			}
		}
		if len(ap.Volumes) > 20 {
			return errors.New("too many volumes")
		}
	case "compose":
		if ap.Compose == "" || len(ap.Compose) > maxComposeBytes {
			return errors.New("the Compose file is empty or too large")
		}
	case "git":
		// No image yet means the app has not been deployed.
		if ap.Image != "" && !gitImageOK(ap.Name, ap.Image) {
			return fmt.Errorf("invalid build %q", ap.Image)
		}
		for _, img := range ap.KeepImages {
			if !gitImageOK(ap.Name, img) {
				return fmt.Errorf("invalid build %q", img)
			}
		}
		if ap.ContainerPort == 0 || ap.Service != agentproto.AppServiceName || ap.HostPort > 29998 {
			return errors.New("a Git app needs the port it listens on")
		}
		for _, v := range ap.Volumes {
			if !volumePathR.MatchString(v) || strings.Contains(v, "..") {
				return fmt.Errorf("invalid volume path %q", v)
			}
		}
		if len(ap.Volumes) > 20 {
			return errors.New("too many volumes")
		}
	default:
		return fmt.Errorf("unknown app kind %q", ap.Kind)
	}
	if ap.ContainerPort != 0 {
		if ap.ContainerPort < 1 || ap.ContainerPort > 65535 || ap.HostPort < 20000 || ap.HostPort > 29999 || !serviceRE.MatchString(ap.Service) {
			return errors.New("invalid port settings")
		}
	} else if ap.Domain != "" {
		return errors.New("a domain needs a container port")
	}
	if ap.Domain != "" && (len(ap.Domain) > 253 || !hostnameRE.MatchString(ap.Domain)) {
		return fmt.Errorf("invalid host name %q", ap.Domain)
	}
	if ap.TLS != "" && ap.TLS != "auto" && ap.TLS != "internal" {
		return fmt.Errorf("unknown TLS mode %q", ap.TLS)
	}
	if len(ap.Env) > 200 {
		return errors.New("too many environment variables")
	}
	for _, e := range ap.Env {
		if !envKeyRE.MatchString(e.Key) || strings.ContainsRune(e.Value, 0) || len(e.Value) > 32<<10 {
			return fmt.Errorf("invalid environment variable %q", e.Key)
		}
	}
	if ap.MemoryMB < 0 || ap.CPUs < 0 {
		return errors.New("invalid limits")
	}
	return nil
}

// ---- Docker ----

// ensureDocker installs Docker and the Compose plugin from the distribution's packages.
// Container logs are capped before the daemon first starts, so they cannot fill the disk.
func ensureDocker(ctx context.Context, sys *system) error {
	if _, err := sys.run(ctx, "docker", "compose", "version"); err == nil {
		return nil
	}
	if _, err := os.Stat(sys.path(dockerDaemonCfg)); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(sys.path(dockerDaemonCfg)), 0o755); err != nil {
			return err
		}
		cfg := `{"log-driver": "json-file", "log-opts": {"max-size": "10m", "max-file": "3"}}` + "\n"
		if err := os.WriteFile(sys.path(dockerDaemonCfg), []byte(cfg), 0o644); err != nil {
			return err
		}
	}
	if err := ensurePackage(ctx, sys, "docker.io"); err != nil {
		return fmt.Errorf("install Docker: %w", err)
	}
	// Ubuntu calls the Compose v2 plugin docker-compose-v2; Debian calls it docker-compose.
	if err := ensurePackage(ctx, sys, "docker-compose-v2"); err != nil {
		if err2 := ensurePackage(ctx, sys, "docker-compose"); err2 != nil {
			return fmt.Errorf("install Docker Compose: %w", err)
		}
	}
	if _, err := sys.run(ctx, "systemctl", "enable", "--now", "docker"); err != nil {
		return err
	}
	if _, err := sys.run(ctx, "docker", "compose", "version"); err != nil {
		return errors.New("Docker Compose v2 is not available from this system's packages")
	}
	return nil
}

// composeCommand is "docker compose" for an app, with its variables in the environment,
// where Compose reads them for ${VAR} in the file. Nothing secret is on the command line.
func composeCommand(sys *system, name string, env []agentproto.EnvVar, args ...string) command {
	dir := sys.path(appDir(name))
	base := []string{"compose", "-p", appProject(name), "--project-directory", dir, "-f", filepath.Join(dir, appComposeFile)}
	if _, err := os.Stat(filepath.Join(dir, appOverrideFile)); err == nil {
		base = append(base, "-f", filepath.Join(dir, appOverrideFile))
	}
	vars := make([]string, 0, len(env))
	for _, e := range env {
		vars = append(vars, e.Key+"="+e.Value)
	}
	return command{name: "docker", args: append(base, args...), env: vars}
}

// withoutOverride drops ekPanel's override file from compose arguments.
func withoutOverride(args []string) []string {
	if i := slices.Index(args, "-f"); i >= 0 && i+3 < len(args) && args[i+2] == "-f" && strings.HasSuffix(args[i+3], "/"+appOverrideFile) {
		return slices.Delete(slices.Clone(args), i+2, i+4)
	}
	return args
}

func compose(ctx context.Context, sys *system, name string, env []agentproto.EnvVar, args ...string) ([]byte, error) {
	return sys.exec(ctx, composeCommand(sys, name, env, args...))
}

// imageCompose is the Compose file of an image app (and of a Git app's build), written as JSON (valid YAML), so no
// value can change its structure. Dollar signs are doubled so Compose keeps them literally.
func imageCompose(ap agentproto.AppSpec) []byte {
	svc := map[string]any{
		"image":   ap.Image,
		"restart": "unless-stopped",
		"logging": map[string]any{"driver": "json-file", "options": map[string]string{"max-size": "10m", "max-file": "3"}},
	}
	env := map[string]string{}
	for _, e := range ap.Env {
		env[e.Key] = strings.ReplaceAll(e.Value, "$", "$$")
	}
	if _, set := env["PORT"]; ap.Kind == "git" && !set {
		// Apps built by Railpack (and many others) listen on $PORT.
		env["PORT"] = strconv.Itoa(ap.ContainerPort)
	}
	if len(env) > 0 {
		svc["environment"] = env
	}
	vols := map[string]any{}
	var mounts []string
	for i, p := range ap.Volumes {
		name := "data" + strconv.Itoa(i)
		vols[name] = map[string]any{}
		mounts = append(mounts, name+":"+p)
	}
	if len(mounts) > 0 {
		svc["volumes"] = mounts
	}
	if ap.Kind == "git" {
		// During a release the new container takes whichever of the two ports is free.
		svc["ports"] = []string{fmt.Sprintf("127.0.0.1:%d-%d:%d", ap.HostPort, ap.HostPort+1, ap.ContainerPort)}
		svc["stop_grace_period"] = "30s"
	} else if ap.ContainerPort != 0 {
		svc["ports"] = []string{fmt.Sprintf("127.0.0.1:%d:%d", ap.HostPort, ap.ContainerPort)}
	}
	if ap.MemoryMB > 0 {
		svc["mem_limit"] = strconv.Itoa(ap.MemoryMB) + "m"
	}
	if ap.CPUs > 0 {
		svc["cpus"] = ap.CPUs
	}
	if ap.Owner != "" {
		// The customer's slice holds the limits of their package.
		svc["cgroup_parent"] = customerSlice(ownerKey(ap.Owner))
	}
	doc := map[string]any{"services": map[string]any{agentproto.AppServiceName: svc}}
	if len(vols) > 0 {
		doc["volumes"] = vols
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	return append(b, '\n')
}

// composeOverride publishes the app's port on 127.0.0.1 and applies limits to the chosen
// service of a Compose app. A customer's app also puts every service (named in services) in
// the customer's slice. Nil means no override is needed.
func composeOverride(ap agentproto.AppSpec, services []string) []byte {
	all := map[string]any{}
	if ap.Owner != "" {
		for _, name := range services {
			all[name] = map[string]any{"cgroup_parent": customerSlice(ownerKey(ap.Owner))}
		}
	}
	if ap.ContainerPort != 0 || ap.MemoryMB > 0 || ap.CPUs > 0 {
		svc, _ := all[ap.Service].(map[string]any)
		if svc == nil {
			svc = map[string]any{}
		}
		if ap.ContainerPort != 0 {
			svc["ports"] = []string{fmt.Sprintf("127.0.0.1:%d:%d", ap.HostPort, ap.ContainerPort)}
		}
		if ap.MemoryMB > 0 {
			svc["mem_limit"] = strconv.Itoa(ap.MemoryMB) + "m"
		}
		if ap.CPUs > 0 {
			svc["cpus"] = ap.CPUs
		}
		all[ap.Service] = svc
	}
	if len(all) == 0 {
		return nil
	}
	b, _ := json.MarshalIndent(map[string]any{"services": all}, "", "  ")
	return append(b, '\n')
}

// composeService is what checkComposeConfig reads of one service in the resolved file.
type composeService struct {
	NetworkMode string `json:"network_mode"`
	Ports       []struct {
		HostIP    string `json:"host_ip"`
		Published string `json:"published"`
	} `json:"ports"`
	Privileged   bool              `json:"privileged"`
	CapAdd       []string          `json:"cap_add"`
	Devices      []json.RawMessage `json:"devices"`
	Pid          string            `json:"pid"`
	Ipc          string            `json:"ipc"`
	UsernsMode   string            `json:"userns_mode"`
	SecurityOpt  []string          `json:"security_opt"`
	VolumesFrom  []string          `json:"volumes_from"`
	CgroupParent string            `json:"cgroup_parent"`
	Volumes      []struct {
		Type   string `json:"type"`
		Source string `json:"source"`
	} `json:"volumes"`
}

// customerComposeRules keeps a customer's app (always a catalogue template) inside its
// containers: nothing of the host may be shared with it. Templates follow these rules
// already; this holds even if one did not.
func customerComposeRules(name string, s composeService, slice string) error {
	switch {
	case s.Privileged || len(s.CapAdd) > 0 || len(s.Devices) > 0 || len(s.SecurityOpt) > 0:
		return fmt.Errorf("service %s asks for extra privileges", name)
	case s.NetworkMode != "" || s.Pid != "" || s.Ipc != "" || s.UsernsMode != "" || len(s.VolumesFrom) > 0:
		return fmt.Errorf("service %s shares namespaces with the host or other containers", name)
	case s.CgroupParent != slice:
		return fmt.Errorf("service %s is not in the customer's slice", name)
	}
	for _, v := range s.Volumes {
		if v.Type != "volume" && v.Type != "tmpfs" {
			return fmt.Errorf("service %s mounts %s from the host", name, v.Source)
		}
	}
	return nil
}

// checkComposeConfig reads the resolved configuration and refuses ports published on
// public addresses (Docker opens those past the firewall) unless the user allowed them.
func checkComposeConfig(raw []byte, ap agentproto.AppSpec) error {
	var cfg struct {
		Services map[string]composeService `json:"services"`
		Volumes  map[string]struct {
			Driver     string            `json:"driver"`
			DriverOpts map[string]string `json:"driver_opts"`
			External   bool              `json:"external"`
		} `json:"volumes"`
		Secrets map[string]json.RawMessage `json:"secrets"`
		Configs map[string]json.RawMessage `json:"configs"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("read the Compose configuration: %w", err)
	}
	if len(cfg.Services) == 0 {
		return errors.New("the Compose file has no services")
	}
	if ap.ContainerPort != 0 {
		if _, ok := cfg.Services[ap.Service]; !ok {
			return fmt.Errorf("the Compose file has no service %q", ap.Service)
		}
	}
	names := make([]string, 0, len(cfg.Services))
	for n := range cfg.Services {
		names = append(names, n)
	}
	slices.Sort(names)
	if ap.Owner != "" {
		// Volumes with driver options can mount any folder of the host; secrets and configs
		// read files of the host.
		for vn, v := range cfg.Volumes {
			if (v.Driver != "" && v.Driver != "local") || len(v.DriverOpts) > 0 || v.External {
				return fmt.Errorf("volume %s is not a plain volume of the app", vn)
			}
		}
		if len(cfg.Secrets) > 0 || len(cfg.Configs) > 0 {
			return errors.New("the Compose file reads secrets or configs from the host")
		}
		for _, n := range names {
			if err := customerComposeRules(n, cfg.Services[n], customerSlice(ownerKey(ap.Owner))); err != nil {
				return err
			}
		}
	}
	if ap.PublicPorts {
		return nil
	}
	for _, n := range names {
		s := cfg.Services[n]
		if s.NetworkMode == "host" {
			return fmt.Errorf("service %s uses the host network, which skips the firewall; allow public ports to use it", n)
		}
		for _, p := range s.Ports {
			if p.Published == "" {
				continue
			}
			if p.HostIP != "127.0.0.1" && p.HostIP != "::1" {
				return fmt.Errorf("service %s publishes port %s on every address, which skips the firewall; publish it on 127.0.0.1 or allow public ports", n, p.Published)
			}
		}
	}
	return nil
}

// ---- Apply ----

func (a *applier) ensureApp(ctx context.Context, ap agentproto.AppSpec) error {
	if a.dockerReady == nil {
		err := ensureDocker(ctx, a.sys)
		a.dockerReady = &err
	}
	if *a.dockerReady != nil {
		return *a.dockerReady
	}
	dir := a.sys.path(appDir(ap.Name))
	if st, err := readAppState(a.sys, ap.Name); err == nil && st.ID != ap.ID {
		return fmt.Errorf("the folder %s belongs to another app", appDir(ap.Name))
	}
	if err := os.MkdirAll(a.sys.path(appsDir), 0o711); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if ap.Kind == "git" && ap.Image == "" {
		// Not deployed (or its first release failed): nothing runs, and the domain waits
		// for a release.
		if _, err := os.Stat(filepath.Join(dir, appComposeFile)); err == nil {
			if _, err := compose(ctx, a.sys, ap.Name, ap.Env, "down", "--remove-orphans"); err != nil {
				return err
			}
			os.Remove(filepath.Join(dir, appComposeFile))
		}
		st, _ := json.Marshal(appState{ID: ap.ID, Stopped: ap.Stopped, Env: ap.Env})
		if _, err := writeSecretIfChanged(filepath.Join(dir, appStateFile), st); err != nil {
			return err
		}
		a.removeCaddyFile(appCaddyPath(ap.ID), ap.ID)
		return nil
	}
	file := []byte(ap.Compose)
	if ap.Kind != "compose" {
		file = imageCompose(ap)
	}
	changed, err := writeSecretIfChanged(filepath.Join(dir, appComposeFile), file)
	if err != nil {
		return err
	}
	var override []byte
	if ap.Kind == "compose" {
		var services []string
		if ap.Owner != "" {
			// The override names every service, so read them from the file alone first.
			cmd := composeCommand(a.sys, ap.Name, ap.Env, "config", "--services")
			cmd.args = withoutOverride(cmd.args)
			out, err := a.sys.exec(ctx, cmd)
			if err != nil {
				return fmt.Errorf("the Compose file is not valid: %w", err)
			}
			services = strings.Fields(string(out))
		}
		override = composeOverride(ap, services)
	}
	if c, err := writeSecretIfChanged(filepath.Join(dir, appOverrideFile), override); err != nil {
		return err
	} else if c {
		changed = true
	}
	st, _ := json.Marshal(appState{ID: ap.ID, Stopped: ap.Stopped, Env: ap.Env})
	if c, err := writeSecretIfChanged(filepath.Join(dir, appStateFile), st); err != nil {
		return err
	} else if c {
		changed = true
	}

	out, err := compose(ctx, a.sys, ap.Name, ap.Env, "config", "--format", "json")
	if err != nil {
		return fmt.Errorf("the Compose file is not valid: %w", err)
	}
	if err := checkComposeConfig(out, ap); err != nil {
		return err
	}
	if ap.Stopped {
		if _, err := compose(ctx, a.sys, ap.Name, ap.Env, "stop"); err != nil {
			return err
		}
	} else if ap.Kind == "git" {
		if err := releaseGitApp(ctx, a.sys, ap); err != nil {
			return err
		}
		pruneBuilds(ctx, a.sys, ap)
	} else {
		args := []string{"up", "--detach", "--remove-orphans"}
		if changed {
			// Report a crashing app now instead of calling it running.
			args = append(args, "--wait", "--wait-timeout", "120")
		}
		if out, err := compose(ctx, a.sys, ap.Name, ap.Env, args...); err != nil {
			logs, _ := compose(ctx, a.sys, ap.Name, ap.Env, "logs", "--no-color", "--tail", "5")
			return fmt.Errorf("%w %s", err, lastLines(append(out, logs...), 5))
		}
	}
	var ports []int
	if ap.Kind == "git" {
		ports = gitPorts(ctx, a.sys, ap)
	}
	return a.writeAppCaddy(ap, ports)
}

var appCaddyTemplate = template.Must(template.New("app").Parse(
	`# Managed by ekPanel (app {{.ID}}). Changes here are overwritten.
{{.Domain}} {
{{- if .Internal}}
	tls internal
{{- end}}
	encode zstd gzip
	reverse_proxy{{range .Ports}} 127.0.0.1:{{.}}{{end}}
{{- if gt (len .Ports) 1}} {
		# The running release's port comes first; during a release both answer.
		lb_policy first
		lb_try_duration 10s
		lb_try_interval 20ms
		fail_duration 30s
	}
{{- end}}
	log {
		output file /var/log/caddy/app-{{.ID}}.log {
			roll_size 20MiB
			roll_keep 5
		}
	}
}
`))

// writeAppCaddy writes the app's web server configuration, proxying to ports in order
// (the app's host port, if nil).
func (a *applier) writeAppCaddy(ap agentproto.AppSpec, ports []int) error {
	path := appCaddyPath(ap.ID)
	if ap.Domain == "" {
		a.removeCaddyFile(path, ap.ID)
		return nil
	}
	var buf bytes.Buffer
	if ports == nil {
		ports = []int{ap.HostPort}
	}
	appCaddyTemplate.Execute(&buf, map[string]any{"ID": ap.ID, "Domain": ap.Domain, "Internal": ap.TLS == "internal", "Ports": ports})
	changed, prev, err := a.writeIfChanged(path, buf.Bytes())
	if err != nil {
		return err
	}
	if changed {
		a.caddyChanged[path] = prev
		a.caddySites[path] = ap.ID
	}
	return nil
}

func (a *applier) removeCaddyFile(path, id string) {
	if old, err := os.ReadFile(a.sys.path(path)); err == nil && os.Remove(a.sys.path(path)) == nil {
		a.caddyChanged[path] = old
		if id != "" {
			a.caddySites[path] = id
		}
	}
}

// removeApp stops and removes the app's containers. Volumes and the app folder (which may
// hold bind-mounted data) are deleted only when the user asked for the data to go too.
func (a *applier) removeApp(ctx context.Context, ap agentproto.AppSpec) error {
	a.removeCaddyFile(appCaddyPath(ap.ID), ap.ID)
	st, err := readAppState(a.sys, ap.Name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.ID != ap.ID {
		return nil // the folder is another app's; this one is already gone
	}
	if _, err := a.sys.run(ctx, "docker", "version"); err != nil {
		return err
	}
	args := []string{"down", "--remove-orphans"}
	if ap.DeleteData {
		args = append(args, "--volumes")
	}
	if _, err := os.Stat(a.sys.path(appDir(ap.Name) + "/" + appComposeFile)); err == nil {
		if _, err := compose(ctx, a.sys, ap.Name, st.Env, args...); err != nil {
			// A Compose file Compose no longer accepts must not keep the app from going:
			// its containers and networks carry the project's label.
			if err := a.removeProject(ctx, ap.Name); err != nil {
				return err
			}
		}
	}
	if ap.Kind == "git" {
		// Builds can be made again from the repository.
		if out, err := a.sys.run(ctx, "docker", "images", "--quiet", agentproto.GitImageRepo(ap.Name)); err == nil {
			if ids := strings.Fields(string(out)); len(ids) > 0 {
				a.sys.run(ctx, "docker", append([]string{"rmi", "--force"}, ids...)...)
			}
		}
	}
	if ap.DeleteData {
		// "down --volumes" only knows the volumes in the current file; earlier versions of
		// the app may have left others with the project's label.
		out, err := a.sys.run(ctx, "docker", "volume", "ls", "--quiet", "--filter", "label=com.docker.compose.project="+appProject(ap.Name))
		if err != nil {
			return err
		}
		if vols := strings.Fields(string(out)); len(vols) > 0 {
			if _, err := a.sys.run(ctx, "docker", append([]string{"volume", "rm"}, vols...)...); err != nil {
				return err
			}
		}
		return os.RemoveAll(a.sys.path(appDir(ap.Name)))
	}
	// Keep the data, but mark the folder so a new app with this name does not take it over.
	return os.Rename(a.sys.path(appDir(ap.Name)+"/"+appStateFile), a.sys.path(appDir(ap.Name)+"/"+appStateFile+".deleted"))
}

// removeProject removes an app's containers and networks by the Compose project label,
// without its Compose file.
func (a *applier) removeProject(ctx context.Context, name string) error {
	label := "label=com.docker.compose.project=" + appProject(name)
	out, err := a.sys.run(ctx, "docker", "ps", "--all", "--quiet", "--filter", label)
	if err != nil {
		return err
	}
	if ids := strings.Fields(string(out)); len(ids) > 0 {
		if _, err := a.sys.run(ctx, "docker", append([]string{"rm", "--force"}, ids...)...); err != nil {
			return err
		}
	}
	if out, err := a.sys.run(ctx, "docker", "network", "ls", "--quiet", "--filter", label); err == nil {
		if ids := strings.Fields(string(out)); len(ids) > 0 {
			a.sys.run(ctx, "docker", append([]string{"network", "rm"}, ids...)...)
		}
	}
	return nil
}

// removeAppOrphans drops the web configuration of apps the panel no longer knows.
// Containers keep running: only an explicit delete stops them.
func (a *applier) removeAppOrphans(apps []agentproto.AppSpec) {
	known := map[string]bool{}
	for _, ap := range apps {
		known[ap.ID] = true
	}
	entries, _ := os.ReadDir(a.sys.path(caddySitesDir))
	for _, e := range entries {
		id, ok := strings.CutPrefix(strings.TrimSuffix(e.Name(), ".caddy"), "app-")
		if ok && uuidRE.MatchString(id) && !known[id] {
			a.removeCaddyFile(appCaddyPath(id), "")
		}
	}
}

func readAppState(sys *system, name string) (appState, error) {
	var st appState
	b, err := os.ReadFile(sys.path(appDir(name) + "/" + appStateFile))
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

// writeSecretIfChanged writes a root-only file, or removes it for nil content.
func writeSecretIfChanged(path string, content []byte) (bool, error) {
	old, err := os.ReadFile(path)
	if content == nil {
		if err != nil {
			return false, nil
		}
		return true, os.Remove(path)
	}
	if err == nil && bytes.Equal(old, content) {
		return false, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

// ---- Operations on one app ----

func loadApp(sys *system, p agentproto.AppParams) (appState, *agentproto.OpError) {
	if !appNameRE.MatchString(p.Name) {
		return appState{}, opError(agentproto.ErrInvalidParams, "invalid app name")
	}
	st, err := readAppState(sys, p.Name)
	if err != nil {
		return st, opError(agentproto.ErrNotAllowed, "no app called %s on this server", p.Name)
	}
	return st, nil
}

func appStatus(ctx context.Context, sys *system, p agentproto.AppParams) (*agentproto.AppStatusResult, *agentproto.OpError) {
	st, opErr := loadApp(sys, p)
	if opErr != nil {
		return nil, opErr
	}
	out, err := compose(ctx, sys, p.Name, st.Env, "ps", "--all", "--format", "json")
	if err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	res := &agentproto.AppStatusResult{Containers: []agentproto.AppContainer{}}
	type psLine struct {
		Service, Name, Image, State, Status, Health string
	}
	add := func(l psLine) {
		res.Containers = append(res.Containers, agentproto.AppContainer{Service: l.Service, Name: l.Name, Image: l.Image, State: l.State, Status: l.Status, Health: l.Health})
	}
	// Older Compose versions print one JSON array, newer ones one object per line.
	trimmed := bytes.TrimSpace(out)
	if bytes.HasPrefix(trimmed, []byte("[")) {
		var list []psLine
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, opError(agentproto.ErrFailed, "read container list: %v", err)
		}
		for _, l := range list {
			add(l)
		}
		return res, nil
	}
	sc := bufio.NewScanner(bytes.NewReader(trimmed))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var l psLine
		if json.Unmarshal(sc.Bytes(), &l) == nil {
			add(l)
		}
	}
	return res, nil
}

func appLogs(ctx context.Context, sys *system, p agentproto.AppParams) (*agentproto.AppLogsResult, *agentproto.OpError) {
	st, opErr := loadApp(sys, p)
	if opErr != nil {
		return nil, opErr
	}
	if p.Service != "" && !serviceRE.MatchString(p.Service) {
		return nil, opError(agentproto.ErrInvalidParams, "invalid service")
	}
	tail := p.Tail
	if tail <= 0 || tail > 5000 {
		tail = 200
	}
	args := []string{"logs", "--no-color", "--timestamps", "--tail", strconv.Itoa(tail)}
	if p.Service != "" {
		args = append(args, "--", p.Service)
	}
	var buf bytes.Buffer
	cmd := composeCommand(sys, p.Name, st.Env, args...)
	cmd.stdout = &buf // logs can be large; keep them apart from errors
	stderr, err := sys.exec(ctx, cmd)
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

func appRestart(ctx context.Context, sys *system, p agentproto.AppParams) (any, *agentproto.OpError) {
	st, opErr := loadApp(sys, p)
	if opErr != nil {
		return nil, opErr
	}
	if st.Stopped {
		return nil, opError(agentproto.ErrNotAllowed, "the app is stopped; start it first")
	}
	if _, err := compose(ctx, sys, p.Name, st.Env, "restart"); err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	return struct{}{}, nil
}

// appPull fetches newer images and recreates the containers whose image changed.
func appPull(ctx context.Context, sys *system, p agentproto.AppParams) (any, *agentproto.OpError) {
	applyMu.Lock() // a concurrent state.apply would run "up" on the same project
	defer applyMu.Unlock()
	st, opErr := loadApp(sys, p)
	if opErr != nil {
		return nil, opErr
	}
	if _, err := compose(ctx, sys, p.Name, st.Env, "pull", "--quiet"); err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	args := []string{"up", "--detach", "--remove-orphans", "--wait", "--wait-timeout", "120"}
	if st.Stopped {
		args = []string{"up", "--no-start", "--remove-orphans"}
	}
	if _, err := compose(ctx, sys, p.Name, st.Env, args...); err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	return struct{}{}, nil
}
