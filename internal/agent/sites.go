package agent

import (
	"bytes"
	"context"
	"crypto/tls"
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
	"syscall"
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
	// probe asks the local web server for a site's home page. Nil (tests) counts as healthy.
	probe func(ctx context.Context, domain string) (status int, body []byte, err error)
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

// command is a process for system.exec. With Stdout set, the returned bytes are stderr only;
// with Stderr set too, nothing is returned.
type command struct {
	name   string
	args   []string
	env    []string
	dir    string // working folder; "" for the agent's
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

func execCommand(ctx context.Context, c command) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.name, c.args...)
	cmd.Env = append(append(os.Environ(), "DEBIAN_FRONTEND=noninteractive"), c.env...)
	cmd.Stdin = c.stdin
	cmd.Dir = c.dir
	var out []byte
	var err error
	if c.stdout != nil && c.stderr != nil {
		cmd.Stdout, cmd.Stderr = c.stdout, c.stderr
		err = cmd.Run()
	} else if c.stdout != nil {
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
	probe:      probeSite,
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
		return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
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
	if sp.Owner != "" && !uuidRE.MatchString(sp.Owner) {
		return errors.New("invalid owner")
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
	switch sp.TLS {
	case "auto", "internal":
	case "custom":
		if _, err := tls.X509KeyPair([]byte(sp.CertPEM), []byte(sp.KeyPEM)); err != nil {
			return fmt.Errorf("the certificate and key do not fit together: %w", err)
		}
	default:
		return fmt.Errorf("unknown TLS mode %q", sp.TLS)
	}
	if err := sp.Web.Validate(); err != nil {
		return err
	}
	if sp.SFTPPasswordHash != "" && !cryptHashRE.MatchString(sp.SFTPPasswordHash) {
		return errors.New("invalid SFTP password hash")
	}
	if sp.FTPPasswordHash != "" && !cryptHashRE.MatchString(sp.FTPPasswordHash) {
		return errors.New("invalid FTP password hash")
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

	a := &applier{sys: sys, results: map[string]*agentproto.SiteResult{}, reloadPHP: map[fpmGroup]bool{}, siteGroup: map[string]fpmGroup{}}
	var res agentproto.StateApplyResult
	for _, sp := range p.Sites {
		r := &agentproto.SiteResult{ID: sp.ID, Generation: sp.Generation, OK: true}
		a.results[sp.ID] = r
		res.Sites = append(res.Sites, *r)
	}
	if err := a.prepare(ctx); err != nil {
		return res, opError(agentproto.ErrFailed, "%v", err)
	}
	if err := a.applyOwners(ctx, p.Owners); err != nil {
		return res, opError(agentproto.ErrFailed, "customer limits: %v", err)
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
	a.applyFTP(ctx, p.Sites, p.FTPPassiveIP)
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
	res.Cron = a.applyCron(ctx, p.Cron)
	// Webmail is a PHP site of its own, reloaded with the others.
	a.results[webmailID] = &agentproto.SiteResult{ID: webmailID, OK: true}
	if err := a.prepareWebmail(ctx, p.Mail); err != nil {
		a.fail(webmailID, fmt.Errorf("webmail: %w", err))
	}
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
	if p.Security != nil {
		res.Security = applySecurity(ctx, sys, *p.Security)
	}
	res.NameServer = applyNameServer(ctx, sys, p.NameServer)
	res.Mail = applyMail(ctx, sys, p.Mail)
	if w := a.results[webmailID]; res.Mail != nil && !w.OK {
		res.Mail.OK = false
		res.Mail.Error = strings.TrimPrefix(strings.Join([]string{res.Mail.Error, w.Error}, "; "), "; ")
	}
	return res, nil
}

type applier struct {
	sys          *system
	siteGroup    map[string]fpmGroup // site ID -> its PHP-FPM master, for sites that use PHP
	results      map[string]*agentproto.SiteResult
	reloadPHP    map[fpmGroup]bool // master -> true if it gained or changed a pool, false if it only lost pools
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
		a.siteGroup[sp.ID] = groupFor(sp)
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
	if err := a.writeCert(ctx, sp); err != nil {
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
{{- range .PHP}}
{{.}}
{{- end}}
`))

// phpLines are the pool lines of a site's PHP settings: the defaults with its own values.
func phpLines(web agentproto.SiteWeb) []string {
	var lines []string
	for _, name := range agentproto.PHPSettingNames() {
		def := agentproto.PHPSettings[name]
		value, ok := web.PHP[name]
		if !ok {
			value = def.Default
		}
		kind := "php_value"
		switch {
		case def.Admin && def.Flag:
			kind = "php_admin_flag"
		case def.Admin:
			kind = "php_admin_value"
		case def.Flag:
			kind = "php_flag"
		}
		lines = append(lines, fmt.Sprintf("%s[%s] = %s", kind, name, value))
	}
	return lines
}

// writePools writes the site's pool for its PHP master and removes it from every other
// master (the site may have switched versions, changed owner, or become static).
func (a *applier) writePools(sp agentproto.SiteSpec) error {
	keep := ""
	if sp.Kind == "php" {
		g := groupFor(sp)
		var buf bytes.Buffer
		poolTemplate.Execute(&buf, map[string]any{"ID": sp.ID, "Pool": poolName(sp.ID), "User": sp.User, "Socket": g.socket(sp.ID), "PHP": phpLines(sp.Web)})
		changed, _, err := a.writeIfChanged(g.poolPath(sp.ID), buf.Bytes())
		if err != nil {
			return err
		}
		if changed {
			a.reloadPHP[g] = true
		}
		keep = a.sys.path(g.poolPath(sp.ID))
	}
	for path, g := range a.poolFiles(sp.ID) {
		if path != keep {
			a.removePool(path, g)
		}
	}
	return nil
}

// ---- Caddy ----

var caddyTemplate = template.Must(template.New("caddy").Funcs(template.FuncMap{"join": strings.Join}).Parse(
	`# Managed by ekPanel (site {{.ID}}). Changes here are overwritten.
{{- define "tls"}}
{{- if .Cert}}
	tls {{.Cert}} {{.Key}}
{{- else if .Internal}}
	tls internal
{{- end}}
{{- end}}
{{- if .AliasHosts}}
{{join .AliasHosts ", "}} {
{{- template "tls" .}}
	redir https://{{.Domain}}{uri} 301
}
{{- end}}
{{join .Hosts ", "}} {
{{- template "tls" .}}
	root * /home/{{.User}}/public_html{{if .DocRoot}}/{{.DocRoot}}{{end}}
	encode zstd gzip
	@hidden {
		path */.*
		not path /.well-known/*
	}
	respond @hidden 404
{{- if .BlockXMLRPC}}
	respond /xmlrpc.php 403
{{- end}}
{{- if .NoUploadsPHP}}
	@uploadsphp path_regexp (?i)^/wp-content/uploads/.*\.(php[0-9]?|phtml|phar)$
	respond @uploadsphp 403
{{- end}}
{{- if .Hotlink}}
	@hotlink {
		path_regexp (?i)\.({{.Hotlink.Extensions}})$
		header Referer *
		not header_regexp Referer (?i)^https?://({{.Hotlink.Hosts}})(:[0-9]+)?(/|$)
	}
	respond @hotlink 403
{{- end}}
{{- if .Redirects}}
	route {
{{- range .Redirects}}
{{- if .Strip}}
		route {{.From}} {
			uri strip_prefix {{.Strip}}
			redir {{.To}}{uri} {{.Code}}
		}
{{- else}}
		redir {{.From}} {{.To}} {{.Code}}
{{- end}}
{{- end}}
	}
{{- end}}
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

// caddyRedirect is one redirect as the template writes it.
type caddyRedirect struct {
	From, To, Strip string
	Code            int
}

func caddyRedirects(web agentproto.SiteWeb) []caddyRedirect {
	var out []caddyRedirect
	for _, r := range web.Redirects {
		cr := caddyRedirect{From: r.From, To: r.To, Code: r.Code}
		prefix, isPrefix := strings.CutSuffix(r.From, "/*")
		switch {
		case isPrefix && r.KeepPath && prefix == "":
			// Everything, with its path: no prefix to strip.
			cr.From, cr.To = "*", strings.TrimSuffix(r.To, "/")+"{uri}"
		case isPrefix && r.KeepPath:
			cr.Strip, cr.To = prefix, strings.TrimSuffix(r.To, "/")
		}
		out = append(out, cr)
	}
	return out
}

// hotlinkRule is the file types and allowed referring hosts as regular expressions.
func hotlinkRule(sp agentproto.SiteSpec) map[string]string {
	h := sp.Web.Hotlink
	if h == nil {
		return nil
	}
	var hosts []string
	for _, d := range append(append([]string{sp.Domain}, sp.Aliases...), h.Allowed...) {
		if rest, ok := strings.CutPrefix(d, "*."); ok {
			hosts = append(hosts, `([a-z0-9-]+\.)+`+regexp.QuoteMeta(rest))
		} else {
			hosts = append(hosts, regexp.QuoteMeta(d))
		}
	}
	return map[string]string{"Extensions": strings.Join(h.Extensions, "|"), "Hosts": strings.Join(hosts, "|")}
}

func (a *applier) writeCaddy(sp agentproto.SiteSpec) error {
	var buf bytes.Buffer
	hosts, aliases := append([]string{sp.Domain}, sp.Aliases...), []string(nil)
	if sp.Web.AliasRedirect && len(sp.Aliases) > 0 {
		hosts, aliases = []string{sp.Domain}, sp.Aliases
	}
	cert, key := "", ""
	if sp.TLS == "custom" {
		cert, key = certPaths(sp)
	}
	caddyTemplate.Execute(&buf, map[string]any{
		"ID": sp.ID, "Domain": sp.Domain, "Hosts": hosts, "AliasHosts": aliases, "Internal": sp.TLS == "internal",
		"Cert": cert, "Key": key, "User": sp.User, "PHP": sp.Kind == "php", "Socket": groupFor(sp).socket(sp.ID),
		"Hotlink": hotlinkRule(sp), "Redirects": caddyRedirects(sp.Web), "BlockXMLRPC": sp.Web.BlockXMLRPC, "NoUploadsPHP": sp.Web.NoUploadsPHP,
		"DocRoot": sp.Web.DocRoot,
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
	for id, g := range a.siteGroup {
		if r := a.results[id]; r == nil || !r.OK {
			continue
		}
		sock := a.sys.path(g.socket(id))
		v := g.version
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
	for path, g := range a.poolFiles(sp.ID) {
		a.removePool(path, g)
	}
	a.removeCerts(sp.ID, "")
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
	// userdel leaves a home folder the account does not own, and site homes belong to root
	// (for the SFTP chroot). Remove it when it is this site's: the account was ekPanel's, or
	// (after an earlier attempt) the folder still has a site's layout. A person's home is
	// never root's.
	home := a.sys.path("/home/" + sp.User)
	if exists || isSiteHome(home) {
		if err := os.RemoveAll(home); err != nil {
			return fmt.Errorf("remove the site's files: %w", err)
		}
	}
	return nil
}

func isSiteHome(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 {
		return false
	}
	_, err = os.Stat(filepath.Join(path, "public_html"))
	return err == nil
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
	for path, g := range a.poolFiles("*") {
		id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "ekpanel-"), ".conf")
		if uuidRE.MatchString(id) && !known[id] {
			a.removePool(path, g)
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
	failGroup := func(g fpmGroup, err error) {
		for id, sg := range a.siteGroup {
			if sg == g {
				a.fail(id, fmt.Errorf("PHP %s could not load the new settings: %w", g.version, err))
			}
		}
	}
	reloadPHP := func(gained bool) {
		if gained {
			// Customers' masters need their configuration and unit before they can start.
			daemonReload := false
			for g, gg := range a.reloadPHP {
				if !gg {
					continue
				}
				changed, err := a.prepareMaster(g)
				if err != nil {
					failGroup(g, err)
				}
				daemonReload = daemonReload || changed
			}
			if daemonReload {
				if _, err := a.sys.run(ctx, "systemctl", "daemon-reload"); err != nil {
					for g := range a.reloadPHP {
						failGroup(g, err)
					}
				}
			}
		}
		for g, gg := range a.reloadPHP {
			if gg != gained || (!gained && a.retireMaster(ctx, g)) {
				continue
			}
			if g.key != "" {
				a.sys.run(ctx, "systemctl", "enable", g.unit())
			}
			if _, err := a.sys.run(ctx, "systemctl", "reload-or-restart", g.unit()); err != nil {
				failGroup(g, err)
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
