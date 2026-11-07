package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"text/template"
	"time"

	"ekpanel/internal/agentproto"
)

// sitesGroup marks Linux accounts that ekPanel created. The agent only ever changes or
// deletes accounts in this group, so a bad request can never touch root or a real user.
const sitesGroup = "ekpanel-sites"

const (
	caddyfile     = "/etc/caddy/Caddyfile"
	caddySitesDir = "/etc/ekpanel/caddy/sites"
)

// system is how site provisioning touches the machine. Tests swap in a temporary root
// directory and a fake command runner.
type system struct {
	root string // prefix for every path; "" on a real server
	run  func(ctx context.Context, name string, args ...string) ([]byte, error)
	// runInput is run with stdin; used for SQL, which must not appear in the process list.
	runInput func(ctx context.Context, input, name string, args ...string) ([]byte, error)
	// exec runs a command with extra environment and optional streams (backups).
	exec  func(ctx context.Context, c command) ([]byte, error)
	fetch func(ctx context.Context, url string) ([]byte, error)
	osID  func() (id, codename string)
	// socketWait is how long to wait for PHP-FPM to create a new pool's socket after a
	// reload; systemd returns before FPM has finished restarting. Zero skips the wait.
	socketWait time.Duration
}

func (s *system) path(p string) string { return filepath.Join(s.root, p) }

func runCommand(ctx context.Context, input *string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive", "NEEDRESTART_SUSPEND=1")
	if input != nil {
		cmd.Stdin = strings.NewReader(*input)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, lastLines(out, 5))
	}
	return out, nil
}

// command is a process for system.exec. With Stdout set, the returned bytes are stderr only.
type command struct {
	name   string
	args   []string
	env    []string
	stdin  io.Reader
	stdout io.Writer
}

func execCommand(ctx context.Context, c command) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.name, c.args...)
	cmd.Env = append(append(os.Environ(), "DEBIAN_FRONTEND=noninteractive"), c.env...)
	cmd.Stdin = c.stdin
	var out []byte
	var err error
	if c.stdout != nil {
		var stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = c.stdout, &stderr
		err = cmd.Run()
		out = stderr.Bytes()
	} else {
		out, err = cmd.CombinedOutput()
	}
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", c.name, strings.Join(c.args, " "), err, lastLines(out, 5))
	}
	return out, nil
}

var realSystem = &system{
	socketWait: 20 * time.Second,
	exec:       execCommand,
	run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return runCommand(ctx, nil, name, args...)
	},
	runInput: func(ctx context.Context, input, name string, args ...string) ([]byte, error) {
		return runCommand(ctx, &input, name, args...)
	},
	fetch: func(ctx context.Context, url string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	},
	osID: func() (string, string) {
		b, _ := os.ReadFile("/etc/os-release")
		var id, code string
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "ID="); ok {
				id = strings.Trim(v, `"`)
			}
			if v, ok := strings.CutPrefix(line, "VERSION_CODENAME="); ok {
				code = strings.Trim(v, `"`)
			}
		}
		return id, code
	},
}

func lastLines(b []byte, n int) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// applyMu serializes state.apply: two overlapping runs would fight over the same files.
var applyMu sync.Mutex

// ---- Validation (the agent trusts nothing it receives) ----

var (
	uuidRE     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	userRE     = regexp.MustCompile(`^[a-z][a-z0-9_]{2,31}$`)
	hostnameRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+([a-z]{2,63}|xn--[a-z0-9-]{1,59})$`)
	reserved   = []string{"root", "daemon", "bin", "sys", "sync", "nobody", "www_data", "caddy", "postgres", "ekpanel", "ubuntu", "debian", "admin"}
)

func validateSite(sp agentproto.SiteSpec) error {
	if !uuidRE.MatchString(sp.ID) {
		return errors.New("invalid site id")
	}
	if !userRE.MatchString(sp.User) || slices.Contains(reserved, sp.User) {
		return fmt.Errorf("invalid system user %q", sp.User)
	}
	if sp.Deleted {
		return nil
	}
	for _, h := range append([]string{sp.Domain}, sp.Aliases...) {
		if len(h) > 253 || !hostnameRE.MatchString(h) {
			return fmt.Errorf("invalid host name %q", h)
		}
	}
	switch sp.Kind {
	case "static":
	case "php":
		if !slices.Contains(agentproto.PHPVersions, sp.PHPVersion) {
			return fmt.Errorf("unsupported PHP version %q", sp.PHPVersion)
		}
	default:
		return fmt.Errorf("unknown site kind %q", sp.Kind)
	}
	if sp.TLS != "auto" && sp.TLS != "internal" {
		return fmt.Errorf("unknown TLS mode %q", sp.TLS)
	}
	if sp.SFTPPasswordHash != "" && !cryptHashRE.MatchString(sp.SFTPPasswordHash) {
		return errors.New("invalid SFTP password hash")
	}
	if len(sp.SSHKeys) > maxSSHKeys {
		return errors.New("too many SSH keys")
	}
	for _, k := range sp.SSHKeys {
		if _, err := normalizeSSHKey(k); err != nil {
			return err
		}
	}
	return nil
}

// ---- Apply ----

func stateApply(ctx context.Context, sys *system, p agentproto.StateApplyParams) (agentproto.StateApplyResult, *agentproto.OpError) {
	applyMu.Lock()
	defer applyMu.Unlock()

	a := &applier{sys: sys, results: map[string]*agentproto.SiteResult{}, reloadPHP: map[string]bool{}, siteVersion: map[string]string{}}
	var res agentproto.StateApplyResult
	for _, sp := range p.Sites {
		r := &agentproto.SiteResult{ID: sp.ID, Generation: sp.Generation, OK: true}
		a.results[sp.ID] = r
		res.Sites = append(res.Sites, *r)
	}
	if err := a.prepare(ctx); err != nil {
		return res, opError(agentproto.ErrFailed, "%v", err)
	}
	for _, sp := range p.Sites {
		if err := validateSite(sp); err != nil {
			a.fail(sp.ID, err)
			continue
		}
		var err error
		if sp.Deleted {
			err = a.remove(ctx, sp)
		} else {
			err = a.ensure(ctx, sp)
		}
		if err != nil {
			a.fail(sp.ID, err)
		}
	}
	a.removeOrphans(p.Sites)
	for _, ap := range p.Apps {
		a.results[ap.ID] = &agentproto.SiteResult{ID: ap.ID, Generation: ap.Generation, OK: true}
		var err error
		switch {
		case validateApp(ap) != nil:
			err = validateApp(ap)
		case ap.Deleted:
			err = a.removeApp(ctx, ap)
		default:
			err = a.ensureApp(ctx, ap)
		}
		if err != nil {
			a.fail(ap.ID, err)
		}
	}
	a.removeAppOrphans(p.Apps)
	a.finish(ctx)
	for i := range res.Sites {
		res.Sites[i] = *a.results[res.Sites[i].ID]
	}
	for _, ap := range p.Apps {
		r := a.results[ap.ID]
		res.Apps = append(res.Apps, agentproto.AppResult{ID: r.ID, Generation: r.Generation, OK: r.OK, Error: r.Error})
	}
	res.Databases = applyDatabases(ctx, sys, p.Databases)
	if p.Firewall != nil {
		res.Firewall = applyFirewall(ctx, sys, *p.Firewall)
	}
	return res, nil
}

type applier struct {
	sys          *system
	siteVersion  map[string]string // site ID -> PHP version, for sites that use PHP
	results      map[string]*agentproto.SiteResult
	reloadPHP    map[string]bool   // PHP version -> true if it gained or changed a pool, false if it only lost pools
	caddyChanged map[string][]byte // site file path -> previous content (nil: did not exist)
	caddySites   map[string]string // site file path -> site ID
	sshdReady    *error            // result of installing the SFTP rules, once per run
	dockerReady  *error            // result of installing Docker, once per run
}

func (a *applier) fail(id string, err error) {
	if r := a.results[id]; r != nil && r.OK {
		r.OK, r.Error = false, err.Error()
	}
}

func (a *applier) prepare(ctx context.Context) error {
	a.caddyChanged = map[string][]byte{}
	a.caddySites = map[string]string{}
	if err := os.MkdirAll(a.sys.path(caddySitesDir), 0o755); err != nil {
		return err
	}
	for _, g := range []string{sitesGroup, sftpGroup} {
		if _, err := a.sys.run(ctx, "getent", "group", g); err != nil {
			if _, err := a.sys.run(ctx, "groupadd", "--system", g); err != nil {
				return err
			}
		}
	}
	return nil
}

// managedUser reports whether a Linux account exists, and refuses one that ekPanel did not create.
func (a *applier) managedUser(ctx context.Context, user string) (exists bool, err error) {
	out, err := a.sys.run(ctx, "id", "-nG", user)
	if err != nil {
		return false, nil
	}
	if !slices.Contains(strings.Fields(string(out)), sitesGroup) {
		return true, fmt.Errorf("the account %q exists but was not created by ekPanel; refusing to use it", user)
	}
	return true, nil
}

func (a *applier) ensure(ctx context.Context, sp agentproto.SiteSpec) error {
	exists, err := a.managedUser(ctx, sp.User)
	if err != nil {
		return err
	}
	home := "/home/" + sp.User
	if !exists {
		if _, err := a.sys.run(ctx, "useradd", "--create-home", "--home-dir", home, "--shell", "/usr/sbin/nologin",
			"--user-group", "--groups", sitesGroup, "--comment", "ekPanel site "+sp.Domain, sp.User); err != nil {
			return err
		}
	}
	if err := a.ensureDirs(ctx, sp, !exists); err != nil {
		return err
	}
	if sp.Kind == "php" {
		a.siteVersion[sp.ID] = sp.PHPVersion
		if err := a.ensurePHP(ctx, sp.PHPVersion); err != nil {
			return err
		}
	}
	if err := a.writePools(sp); err != nil {
		return err
	}
	if err := a.ensureSFTP(ctx, sp); err != nil {
		return err
	}
	return a.writeCaddy(sp)
}

func (a *applier) ensureDirs(ctx context.Context, sp agentproto.SiteSpec, created bool) error {
	home := "/home/" + sp.User
	// The home folder belongs to root (OpenSSH requires that of an SFTP chroot) with the
	// site's group, so the account can enter it but only write inside the folders below.
	dirs := []struct {
		path  string
		mode  os.FileMode
		owner string
	}{
		{home, 0o750, "root:" + sp.User},
		{home + "/public_html", 0o750, sp.User + ":" + sp.User},
		{home + "/logs", 0o750, sp.User + ":" + sp.User},
		{home + "/tmp", 0o700, sp.User + ":" + sp.User},
		{home + "/.trash", 0o700, sp.User + ":" + sp.User},
	}
	for _, d := range dirs {
		p := a.sys.path(d.path)
		if err := os.MkdirAll(p, d.mode); err != nil {
			return err
		}
		if err := os.Chmod(p, d.mode); err != nil {
			return err
		}
		if _, err := a.sys.run(ctx, "chown", d.owner, d.path); err != nil {
			return err
		}
	}
	// Caddy (the web server) may pass through the home folder and read public_html, and
	// nothing else. Other sites' accounts get no access at all.
	if _, err := a.sys.run(ctx, "setfacl", "-m", "u:caddy:x", home); err != nil {
		return err
	}
	args := []string{"-m", "u:caddy:rX,d:u:caddy:rX", home + "/public_html"}
	if created {
		args = append([]string{"-R"}, args...)
	}
	if _, err := a.sys.run(ctx, "setfacl", args...); err != nil {
		return err
	}
	index := home + "/public_html/index.html"
	if created {
		if _, err := os.Stat(a.sys.path(index)); errors.Is(err, os.ErrNotExist) {
			page := strings.ReplaceAll(placeholderPage, "{{domain}}", sp.Domain)
			if err := os.WriteFile(a.sys.path(index), []byte(page), 0o644); err != nil {
				return err
			}
			if _, err := a.sys.run(ctx, "chown", sp.User+":"+sp.User, index); err != nil {
				return err
			}
		}
	}
	return nil
}

const placeholderPage = `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{domain}}</title><body style="font-family:system-ui,sans-serif;max-width:40rem;margin:15vh auto;padding:0 1rem;color:#15211f">
<h1>{{domain}} is ready</h1><p>This site is hosted with ekPanel. Upload your files to <code>public_html</code> to replace this page.</p>
</body></html>
`

// ---- PHP ----

func poolName(siteID string) string { return "ekpanel-" + siteID }

// socketPath includes the PHP version: while a site switches versions, the old version's
// FPM removes its socket on reload, and must not remove the new one's.
func socketPath(siteID, version string) string {
	return "/run/php/ekpanel-" + siteID + "-php" + version + ".sock"
}

func poolPath(version, siteID string) string {
	return fmt.Sprintf("/etc/php/%s/fpm/pool.d/%s.conf", version, poolName(siteID))
}

func (a *applier) ensurePHP(ctx context.Context, version string) error {
	out, _ := a.sys.run(ctx, "dpkg-query", "-W", "-f=${Status}", "php"+version+"-fpm")
	if strings.Contains(string(out), "install ok installed") {
		return nil
	}
	if err := a.ensurePHPRepository(ctx); err != nil {
		return fmt.Errorf("add PHP package repository: %w", err)
	}
	pkg := func(ext string) string { return "php" + version + "-" + ext }
	base := []string{"fpm", "cli", "common", "mysql", "pgsql", "curl", "gd", "mbstring", "xml", "zip", "intl", "bcmath"}
	args := []string{"-o", "DPkg::Lock::Timeout=300", "install", "-y", "--no-install-recommends"}
	for _, e := range base {
		args = append(args, pkg(e))
	}
	if _, err := a.sys.run(ctx, "apt-get", args...); err != nil {
		return fmt.Errorf("install PHP %s: %w", version, err)
	}
	// Extensions that are built in or packaged differently in some versions.
	for _, e := range []string{"opcache", "imagick"} {
		a.sys.run(ctx, "apt-get", "-o", "DPkg::Lock::Timeout=300", "install", "-y", "--no-install-recommends", pkg(e))
	}
	_, err := a.sys.run(ctx, "systemctl", "enable", "--now", "php"+version+"-fpm")
	return err
}

// ensurePHPRepository adds Ondřej Surý's PHP packages, which provide every supported PHP
// version side by side on Ubuntu (Launchpad PPA) and Debian (packages.sury.org).
func (a *applier) ensurePHPRepository(ctx context.Context) error {
	if m, _ := filepath.Glob(a.sys.path("/etc/apt/sources.list.d/*ondrej*")); len(m) > 0 {
		return nil
	}
	list := "/etc/apt/sources.list.d/ekpanel-php.list"
	if _, err := os.Stat(a.sys.path(list)); err == nil {
		return nil
	}
	id, codename := a.sys.osID()
	switch id {
	case "ubuntu":
		// add-apt-repository fetches the PPA's signing key from Launchpad over HTTPS.
		if _, err := a.sys.run(ctx, "add-apt-repository", "-y", "ppa:ondrej/php"); err != nil {
			return err
		}
		return nil
	case "debian":
		if codename == "" {
			return errors.New("cannot detect the Debian release")
		}
		key, err := a.sys.fetch(ctx, "https://packages.sury.org/php/apt.gpg")
		if err != nil {
			return err
		}
		keyring := "/usr/share/keyrings/ekpanel-php.gpg" // binary keyring: apt requires the .gpg extension
		if err := os.MkdirAll(a.sys.path("/usr/share/keyrings"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(a.sys.path(keyring), key, 0o644); err != nil {
			return err
		}
		line := "deb [signed-by=" + keyring + "] https://packages.sury.org/php/ " + codename + " main\n"
		if err := os.WriteFile(a.sys.path(list), []byte(line), 0o644); err != nil {
			return err
		}
		if _, err := a.sys.run(ctx, "apt-get", "-o", "DPkg::Lock::Timeout=300", "update"); err != nil {
			os.Remove(a.sys.path(list))
			return err
		}
		return nil
	default:
		return fmt.Errorf("unsupported OS %q", id)
	}
}

var poolTemplate = template.Must(template.New("pool").Parse(`; Managed by ekPanel (site {{.ID}}). Changes here are overwritten.
[{{.Pool}}]
user = {{.User}}
group = {{.User}}
listen = {{.Socket}}
listen.owner = caddy
listen.group = caddy
listen.mode = 0660

pm = ondemand
pm.max_children = 5
pm.process_idle_timeout = 30s
pm.max_requests = 500
chdir = /

php_admin_value[open_basedir] = /home/{{.User}}/:/usr/share/php/
php_admin_value[upload_tmp_dir] = /home/{{.User}}/tmp
php_admin_value[session.save_path] = /home/{{.User}}/tmp
php_admin_value[sys_temp_dir] = /home/{{.User}}/tmp
php_admin_value[error_log] = /home/{{.User}}/logs/php-error.log
php_admin_flag[log_errors] = on
php_value[memory_limit] = 256M
php_value[upload_max_filesize] = 64M
php_value[post_max_size] = 64M
php_value[max_execution_time] = 120
`))

// writePools writes the site's pool for its PHP version and removes it from every other
// version (the site may have switched, or become static).
func (a *applier) writePools(sp agentproto.SiteSpec) error {
	for _, v := range agentproto.PHPVersions {
		path := poolPath(v, sp.ID)
		if sp.Kind == "php" && v == sp.PHPVersion {
			var buf bytes.Buffer
			poolTemplate.Execute(&buf, map[string]string{"ID": sp.ID, "Pool": poolName(sp.ID), "User": sp.User, "Socket": socketPath(sp.ID, v)})
			changed, _, err := a.writeIfChanged(path, buf.Bytes())
			if err != nil {
				return err
			}
			if changed {
				a.reloadPHP[v] = true
			}
			continue
		}
		if err := os.Remove(a.sys.path(path)); err == nil && !a.reloadPHP[v] {
			a.reloadPHP[v] = false
		}
	}
	return nil
}

// ---- Caddy ----

var caddyTemplate = template.Must(template.New("caddy").Funcs(template.FuncMap{"join": strings.Join}).Parse(
	`# Managed by ekPanel (site {{.ID}}). Changes here are overwritten.
{{join .Hosts ", "}} {
{{- if .Internal}}
	tls internal
{{- end}}
	root * /home/{{.User}}/public_html
	encode zstd gzip
	@hidden {
		path */.*
		not path /.well-known/*
	}
	respond @hidden 404
{{- if .PHP}}
	php_fastcgi unix/{{.Socket}}
{{- end}}
	file_server
	log {
		output file /var/log/caddy/site-{{.ID}}.log {
			roll_size 20MiB
			roll_keep 5
		}
	}
}
`))

func caddySitePath(id string) string { return caddySitesDir + "/" + id + ".caddy" }

func (a *applier) writeCaddy(sp agentproto.SiteSpec) error {
	var buf bytes.Buffer
	caddyTemplate.Execute(&buf, map[string]any{
		"ID": sp.ID, "Hosts": append([]string{sp.Domain}, sp.Aliases...), "Internal": sp.TLS == "internal",
		"User": sp.User, "PHP": sp.Kind == "php", "Socket": socketPath(sp.ID, sp.PHPVersion),
	})
	path := caddySitePath(sp.ID)
	changed, prev, err := a.writeIfChanged(path, buf.Bytes())
	if err != nil {
		return err
	}
	if changed {
		a.caddyChanged[path] = prev
		a.caddySites[path] = sp.ID
	}
	return nil
}

// writeIfChanged writes content to path unless it already holds exactly that. It returns
// the previous content (nil if the file did not exist) so a failed reload can restore it.
func (a *applier) writeIfChanged(path string, content []byte) (changed bool, prev []byte, err error) {
	p := a.sys.path(path)
	old, err := os.ReadFile(p)
	if err == nil && bytes.Equal(old, content) {
		return false, nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false, nil, err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return false, nil, err
	}
	if err := os.Rename(tmp, p); err != nil {
		return false, nil, err
	}
	return true, old, nil
}

// waitForSockets blocks until every PHP site's socket exists, so Caddy is never pointed at
// a socket PHP-FPM has not created yet. A socket that never appears is reported on its site.
func (a *applier) waitForSockets(ctx context.Context) {
	if a.sys.socketWait == 0 {
		return
	}
	deadline := time.Now().Add(a.sys.socketWait)
	for id, v := range a.siteVersion {
		if r := a.results[id]; r == nil || !r.OK {
			continue
		}
		sock := a.sys.path(socketPath(id, v))
		for {
			if _, err := os.Stat(sock); err == nil {
				break
			}
			if time.Now().After(deadline) || ctx.Err() != nil {
				a.fail(id, fmt.Errorf("PHP %s did not start this site's pool; see journalctl -u php%s-fpm", v, v))
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// ---- Remove ----

func (a *applier) remove(ctx context.Context, sp agentproto.SiteSpec) error {
	path := caddySitePath(sp.ID)
	if old, err := os.ReadFile(a.sys.path(path)); err == nil {
		if err := os.Remove(a.sys.path(path)); err != nil {
			return err
		}
		a.caddyChanged[path] = old
		a.caddySites[path] = sp.ID
	}
	for _, v := range agentproto.PHPVersions {
		if err := os.Remove(a.sys.path(poolPath(v, sp.ID))); err == nil && !a.reloadPHP[v] {
			a.reloadPHP[v] = false
		}
	}
	os.Remove(a.sys.path(sshKeysDir + "/" + sp.User))
	exists, err := a.managedUser(ctx, sp.User)
	if err != nil {
		return err
	}
	if exists {
		a.sys.run(ctx, "pkill", "-KILL", "-u", sp.User) // no processes is not an error
		if _, err := a.sys.run(ctx, "userdel", "--remove", sp.User); err != nil {
			return err
		}
	}
	return nil
}

// removeOrphans drops web and PHP configuration for sites the panel no longer knows.
// Their files and accounts stay: only an explicit delete removes data.
func (a *applier) removeOrphans(sites []agentproto.SiteSpec) {
	known := map[string]bool{}
	for _, sp := range sites {
		known[sp.ID] = true
	}
	entries, _ := os.ReadDir(a.sys.path(caddySitesDir))
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".caddy")
		if !ok || !uuidRE.MatchString(id) || known[id] {
			continue
		}
		path := caddySitePath(id)
		if old, err := os.ReadFile(a.sys.path(path)); err == nil && os.Remove(a.sys.path(path)) == nil {
			a.caddyChanged[path] = old
		}
	}
	for _, v := range agentproto.PHPVersions {
		pools, _ := filepath.Glob(a.sys.path(fmt.Sprintf("/etc/php/%s/fpm/pool.d/ekpanel-*.conf", v)))
		for _, p := range pools {
			id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "ekpanel-"), ".conf")
			if uuidRE.MatchString(id) && !known[id] && os.Remove(p) == nil && !a.reloadPHP[v] {
				a.reloadPHP[v] = false
			}
		}
	}
}

// ---- Reloads ----

// finish reloads PHP-FPM and Caddy once for the whole run, in the order that keeps sites
// up: PHP versions that gained pools first (their sockets exist before Caddy uses them),
// then Caddy, then versions that only lost pools (their old sockets are no longer used).
// If Caddy rejects the new configuration, every changed site file is restored and those
// sites report the error, so one bad site cannot take the web server down for the others.
func (a *applier) finish(ctx context.Context) {
	reloadPHP := func(gained bool) {
		for v, g := range a.reloadPHP {
			if g != gained {
				continue
			}
			if _, err := a.sys.run(ctx, "systemctl", "reload-or-restart", "php"+v+"-fpm"); err != nil {
				for id, version := range a.siteVersion {
					if version == v {
						a.fail(id, fmt.Errorf("PHP %s could not load the new settings: %w", v, err))
					}
				}
			}
		}
	}
	reloadPHP(true)
	defer reloadPHP(false)
	a.waitForSockets(ctx)
	if len(a.caddyChanged) == 0 {
		return
	}
	reloadCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// Validate as the caddy user: validation opens log files, and a file it created as root
	// would be unwritable for the real web server.
	_, err := a.sys.run(reloadCtx, "runuser", "-u", "caddy", "--", "caddy", "validate", "--config", caddyfile, "--adapter", "caddyfile")
	if err == nil {
		_, err = a.sys.run(reloadCtx, "caddy", "reload", "--config", caddyfile, "--adapter", "caddyfile")
		if err == nil {
			return
		}
	}
	for path, prev := range a.caddyChanged {
		if prev == nil {
			os.Remove(a.sys.path(path))
		} else {
			os.WriteFile(a.sys.path(path), prev, 0o644)
		}
		if id := a.caddySites[path]; id != "" {
			a.fail(id, fmt.Errorf("web server rejected the configuration: %w", err))
		}
	}
}
