package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"ekpanel/internal/agentproto"
)

// wpSite runs WP-CLI inside one site, as the site's account. Plugins and themes are not
// loaded unless needed, so a broken plugin cannot stop the toolkit from repairing it.
type wpSite struct {
	sys     *system
	site    agentproto.WordPressSite
	home    string
	docroot string
}

func openWPSite(ctx context.Context, sys *system, site agentproto.WordPressSite) (*wpSite, *agentproto.OpError) {
	if err := site.Validate(); err != nil {
		return nil, opError(agentproto.ErrInvalidParams, "%v", err)
	}
	out, err := sys.run(ctx, "id", "-nG", site.User)
	if err != nil || !slices.Contains(strings.Fields(string(out)), sitesGroup) {
		return nil, opError(agentproto.ErrNotAllowed, "%q is not an ekPanel site account", site.User)
	}
	if err := ensureWPCLI(ctx, sys); err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	home := "/home/" + site.User
	return &wpSite{sys: sys, site: site, home: home, docroot: home + "/public_html"}, nil
}

// wp runs WP-CLI; with loadAll, plugins and themes load too (activating one runs its code).
func (w *wpSite) wp(ctx context.Context, loadAll bool, args ...string) (string, error) {
	cmd := []string{"-u", w.site.User, "--", "env", "HOME=" + w.home + "/tmp", "WP_CLI_CACHE_DIR=" + w.home + "/tmp/.wp-cli-cache",
		"php" + w.site.PHPVersion, wpCLIPath, "--path=" + w.docroot}
	if !loadAll {
		cmd = append(cmd, "--skip-plugins", "--skip-themes")
	}
	b, err := w.sys.run(ctx, "runuser", append(cmd, args...)...)
	return strings.TrimSpace(string(b)), err
}

// as runs a command as the site's account.
func (w *wpSite) as(ctx context.Context, name string, args ...string) error {
	_, err := w.sys.run(ctx, "runuser", append([]string{"-u", w.site.User, "--", name}, args...)...)
	return err
}

func (w *wpSite) installed() bool {
	_, e1 := os.Stat(w.sys.path(w.docroot + "/wp-config.php"))
	_, e2 := os.Stat(w.sys.path(w.docroot + "/wp-includes/version.php"))
	return e1 == nil && e2 == nil
}

type wpListed struct {
	Name          string `json:"name"`
	Title         string `json:"title"`
	Status        string `json:"status"`
	Version       string `json:"version"`
	UpdateVersion string `json:"update_version"`
}

func (w *wpSite) list(ctx context.Context, kind string) ([]agentproto.WordPressItem, error) {
	out, err := w.wp(ctx, false, kind, "list", "--format=json", "--fields=name,title,status,version,update_version")
	if err != nil {
		return nil, err
	}
	var listed []wpListed
	if err := json.Unmarshal([]byte(lastJSON(out)), &listed); err != nil {
		return nil, fmt.Errorf("read the %s list: %w", kind, err)
	}
	items := make([]agentproto.WordPressItem, 0, len(listed))
	for _, l := range listed {
		items = append(items, agentproto.WordPressItem{Name: l.Name, Title: l.Title, Status: l.Status, Version: l.Version, Update: l.UpdateVersion})
	}
	return items, nil
}

// lastJSON drops any notices WP-CLI printed before its JSON.
func lastJSON(out string) string {
	if i := strings.LastIndex(out, "\n["); i >= 0 {
		return out[i+1:]
	}
	return out
}

func (w *wpSite) info(ctx context.Context) agentproto.WordPressInfo {
	info := agentproto.WordPressInfo{Plugins: []agentproto.WordPressItem{}, Themes: []agentproto.WordPressItem{}}
	if !w.installed() {
		return info
	}
	info.Installed = true
	v, err := w.wp(ctx, false, "core", "version")
	if err != nil {
		info.Error = "WP-CLI could not read this WordPress: " + lastLines([]byte(err.Error()), 3)
		return info
	}
	info.Version = v
	if out, err := w.wp(ctx, false, "core", "check-update", "--format=json"); err == nil && strings.Contains(out, "[") {
		var updates []struct {
			Version    string `json:"version"`
			UpdateType string `json:"update_type"`
		}
		if json.Unmarshal([]byte(lastJSON(out)), &updates) == nil {
			for _, u := range updates {
				if info.CoreUpdate == "" {
					info.CoreUpdate = u.Version
				}
				if u.UpdateType == "minor" && info.CoreMinor == "" {
					info.CoreMinor = u.Version
				}
			}
		}
	}
	if info.Plugins, err = w.list(ctx, "plugin"); err != nil {
		info.Error = err.Error()
	}
	if info.Themes, err = w.list(ctx, "theme"); err != nil {
		info.Error = err.Error()
	}
	_, err = w.wp(ctx, false, "maintenance-mode", "is-active")
	info.Maintenance = err == nil
	info.FileEditDisabled = w.constTrue(ctx, "DISALLOW_FILE_EDIT")
	info.Debug = w.constTrue(ctx, "WP_DEBUG")
	info.SiteURL, _ = w.wp(ctx, false, "option", "get", "siteurl")
	info.LoosePermissions = w.loosePermissions()
	return info
}

func (w *wpSite) constTrue(ctx context.Context, name string) bool {
	v, err := w.wp(ctx, false, "config", "get", name, "--type=constant")
	return err == nil && (v == "1" || strings.EqualFold(v, "true"))
}

// loosePermissions counts what other accounts could write, and a readable wp-config.php.
func (w *wpSite) loosePermissions() int {
	n := 0
	filepath.WalkDir(w.sys.path(w.docroot), func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Mode().Perm()&0o002 != 0 && d.Type()&fs.ModeSymlink == 0 {
			n++
		}
		return nil
	})
	if info, err := os.Stat(w.sys.path(w.docroot + "/wp-config.php")); err == nil && info.Mode().Perm()&0o077 != 0 {
		n++
	}
	return n
}

func wordPressInfo(ctx context.Context, sys *system, p agentproto.WordPressInfoParams) (*agentproto.WordPressInfo, *agentproto.OpError) {
	w, opErr := openWPSite(ctx, sys, p.Site)
	if opErr != nil {
		return nil, opErr
	}
	info := w.info(ctx)
	return &info, nil
}

// ---- Updates ----

// healthy reports whether the site's home page answers without a server error or
// WordPress's "critical error" page.
func (w *wpSite) healthy(ctx context.Context) bool {
	if w.sys.probe == nil {
		return true
	}
	status, body, err := w.sys.probe(ctx, w.site.Domain)
	return err == nil && status < 500 && !bytes.Contains(body, []byte("There has been a critical error")) &&
		!bytes.Contains(body, []byte("<b>Fatal error</b>"))
}

// probeSite fetches https://domain/ from the local web server, without following redirects.
func probeSite(ctx context.Context, domain string) (int, []byte, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	client := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, "127.0.0.1:443")
			},
			// The local web server; its certificate may be self-signed or not issued yet.
			TLSClientConfig: &tls.Config{ServerName: domain, InsecureSkipVerify: true},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+domain+"/", nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", "ekPanel update check")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	return resp.StatusCode, body, nil
}

const rollbackDir = "/tmp/.ekpanel-rollback"

// updateItem updates one plugin or theme, keeping a copy of its files to put back if the
// update fails or breaks the site.
func (w *wpSite) updateItem(ctx context.Context, kind string, it agentproto.WordPressItem, wasHealthy bool) agentproto.WordPressUpdated {
	res := agentproto.WordPressUpdated{Kind: kind, Name: it.Name, From: it.Version, To: it.Update}
	path, err := w.wp(ctx, false, kind, "path", it.Name)
	if err != nil {
		res.Outcome, res.Error = "failed", "cannot find its files"
		return res
	}
	// A plugin is a folder, or one file directly in wp-content/plugins.
	dir := filepath.Dir(path)
	if base := filepath.Base(dir); base != "plugins" && base != "themes" {
		path = dir
	}
	if !strings.HasPrefix(path, w.docroot+"/wp-content/") || strings.Contains(path, "..") {
		res.Outcome, res.Error = "failed", "its files are outside wp-content"
		return res
	}
	backup := w.home + rollbackDir + "/" + kind + "-" + it.Name
	w.as(ctx, "rm", "-rf", "--", backup)
	if err := w.as(ctx, "mkdir", "-p", w.home+rollbackDir); err != nil {
		res.Outcome, res.Error = "failed", err.Error()
		return res
	}
	if err := w.as(ctx, "cp", "-a", "--", path, backup); err != nil {
		res.Outcome, res.Error = "failed", "could not keep a copy: "+err.Error()
		return res
	}
	defer w.as(ctx, "rm", "-rf", "--", backup)
	restore := func() error {
		if err := w.as(ctx, "rm", "-rf", "--", path); err != nil {
			return err
		}
		return w.as(ctx, "mv", "--", backup, path)
	}
	if _, err := w.wp(ctx, false, kind, "update", it.Name); err != nil {
		res.Outcome, res.Error = "failed", lastLines([]byte(err.Error()), 2)
		restore()
		return res
	}
	if wasHealthy && !w.healthy(ctx) {
		if err := restore(); err != nil {
			res.Outcome, res.Error = "failed", "the update broke the site and the old files could not be put back: "+err.Error()
			return res
		}
		res.Outcome, res.Error = "rolled_back", "the site showed an error after the update, so the previous version is back"
		return res
	}
	res.Outcome = "updated"
	return res
}

func (w *wpSite) updateCore(ctx context.Context, info agentproto.WordPressInfo, which string, wasHealthy bool) agentproto.WordPressUpdated {
	target := info.CoreMinor
	if which == "latest" {
		target = info.CoreUpdate
	}
	res := agentproto.WordPressUpdated{Kind: "core", Name: "wordpress", From: info.Version, To: target}
	if target == "" {
		res.Outcome = "current"
		return res
	}
	if _, err := w.wp(ctx, false, "core", "update", "--version="+target); err != nil {
		res.Outcome, res.Error = "failed", lastLines([]byte(err.Error()), 2)
		return res
	}
	w.wp(ctx, false, "core", "update-db")
	if wasHealthy && !w.healthy(ctx) {
		// Core files of the old version, over the new ones; wp-content stays.
		if _, err := w.wp(ctx, false, "core", "download", "--version="+info.Version, "--force", "--skip-content"); err != nil {
			res.Outcome, res.Error = "failed", "the update broke the site and the old version could not be put back"
			return res
		}
		res.Outcome, res.Error = "rolled_back", "the site showed an error after the update, so the previous version is back"
		return res
	}
	res.Outcome = "updated"
	return res
}

func wordPressUpdate(ctx context.Context, sys *system, p agentproto.WordPressUpdateParams) (*agentproto.WordPressUpdateResult, *agentproto.OpError) {
	if err := p.Validate(); err != nil {
		return nil, opError(agentproto.ErrInvalidParams, "%v", err)
	}
	w, opErr := openWPSite(ctx, sys, p.Site)
	if opErr != nil {
		return nil, opErr
	}
	info := w.info(ctx)
	if !info.Installed || info.Error != "" {
		return nil, opError(agentproto.ErrFailed, "no working WordPress here: %s", info.Error)
	}
	res := &agentproto.WordPressUpdateResult{Items: []agentproto.WordPressUpdated{}}
	healthy := w.healthy(ctx)
	if p.Core != "" {
		res.Items = append(res.Items, w.updateCore(ctx, info, p.Core, healthy))
	}
	for _, group := range []struct {
		kind  string
		want  []string
		items []agentproto.WordPressItem
	}{{"plugin", p.Plugins, info.Plugins}, {"theme", p.Themes, info.Themes}} {
		for _, it := range group.items {
			if it.Update == "" || (!slices.Contains(group.want, "*") && !slices.Contains(group.want, it.Name)) {
				continue
			}
			res.Items = append(res.Items, w.updateItem(ctx, group.kind, it, healthy))
		}
	}
	w.as(ctx, "rm", "-rf", "--", w.home+rollbackDir)
	res.Info = w.info(ctx)
	return res, nil
}

// ---- Settings ----

func wordPressManage(ctx context.Context, sys *system, p agentproto.WordPressManageParams) (*agentproto.WordPressInfo, *agentproto.OpError) {
	if err := p.Validate(); err != nil {
		return nil, opError(agentproto.ErrInvalidParams, "%v", err)
	}
	w, opErr := openWPSite(ctx, sys, p.Site)
	if opErr != nil {
		return nil, opErr
	}
	if !w.installed() {
		return nil, opError(agentproto.ErrFailed, "WordPress is not installed here")
	}
	var err error
	switch p.Action {
	case "maintenance_on":
		_, err = w.wp(ctx, false, "maintenance-mode", "activate")
	case "maintenance_off":
		_, err = w.wp(ctx, false, "maintenance-mode", "deactivate")
	case "plugin_activate":
		_, err = w.wp(ctx, true, "plugin", "activate", p.Name)
	case "plugin_deactivate":
		// Without loading plugins, so a plugin that breaks the site can still be turned off.
		_, err = w.wp(ctx, false, "plugin", "deactivate", p.Name)
	case "file_edit_off":
		_, err = w.wp(ctx, false, "config", "set", "DISALLOW_FILE_EDIT", "true", "--raw", "--type=constant")
	case "file_edit_on":
		if w.constTrue(ctx, "DISALLOW_FILE_EDIT") {
			_, err = w.wp(ctx, false, "config", "delete", "DISALLOW_FILE_EDIT", "--type=constant")
		}
	case "fix_permissions":
		// Nobody but the site's account may write; wp-config.php (the database password)
		// is for its eyes only. The web server reads through its own ACL entry.
		if err = w.as(ctx, "find", w.docroot, "-perm", "-o+w", "!", "-type", "l", "-exec", "chmod", "o-w", "{}", "+"); err == nil {
			err = w.as(ctx, "chmod", "600", w.docroot+"/wp-config.php")
		}
	}
	if err != nil {
		return nil, opError(agentproto.ErrFailed, "%s", lastLines([]byte(err.Error()), 3))
	}
	info := w.info(ctx)
	return &info, nil
}

// ---- Staging copies ----

const cloneTmpDir = "/var/lib/ekpanel-agent/tmp"

var (
	wpDefineRE = func(name string) *regexp.Regexp {
		return regexp.MustCompile(`define\(\s*['"]` + name + `['"]\s*,\s*['"][^'"]*['"]\s*\)`)
	}
	tableNameRE = regexp.MustCompile(`^[A-Za-z0-9_$]{1,64}$`)
)

// dbOf reads the database a site's wp-config.php uses, which must be on this server.
func (w *wpSite) dbOf(ctx context.Context) (string, error) {
	host, err := w.wp(ctx, false, "config", "get", "DB_HOST", "--type=constant")
	if err != nil {
		return "", errors.New("cannot read wp-config.php")
	}
	if !slices.Contains([]string{"localhost", "127.0.0.1", "localhost:3306", "127.0.0.1:3306"}, host) {
		return "", fmt.Errorf("its database is on %s, not on this server", host)
	}
	name, err := w.wp(ctx, false, "config", "get", "DB_NAME", "--type=constant")
	if err != nil || !dbNameRE.MatchString(name) {
		return "", fmt.Errorf("unexpected database name %q", name)
	}
	return name, nil
}

// setDBConfig points a copied wp-config.php at the copy's own database.
func (w *wpSite) setDBConfig(db agentproto.CloneDB) error {
	path := w.sys.path(w.docroot + "/wp-config.php")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for name, value := range map[string]string{"DB_NAME": db.Name, "DB_USER": db.User, "DB_PASSWORD": db.Password} {
		re := wpDefineRE(name)
		if !re.Match(b) {
			return fmt.Errorf("wp-config.php has no %s line ekPanel can change", name)
		}
		b = re.ReplaceAllLiteral(b, []byte("define( '"+name+"', '"+value+"' )"))
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func wordPressClone(ctx context.Context, sys *system, p agentproto.WordPressCloneParams) (*agentproto.WordPressCloneResult, *agentproto.OpError) {
	if err := p.Validate(); err != nil {
		return nil, opError(agentproto.ErrInvalidParams, "%v", err)
	}
	from, opErr := openWPSite(ctx, sys, p.From)
	if opErr != nil {
		return nil, opErr
	}
	to, opErr := openWPSite(ctx, sys, p.To)
	if opErr != nil {
		return nil, opErr
	}
	fail := func(format string, a ...any) (*agentproto.WordPressCloneResult, *agentproto.OpError) {
		return nil, opError(agentproto.ErrFailed, format, a...)
	}
	if !from.installed() {
		return fail("there is no WordPress to copy")
	}
	fromDB, err := from.dbOf(ctx)
	if err != nil {
		return fail("the source site: %v", err)
	}
	existing := to.installed()
	if !existing && (p.ToDB == nil || !p.Files || !p.Database) {
		return fail("the target has no WordPress yet: copy files and database, with a database for it")
	}
	res := &agentproto.WordPressCloneResult{}
	stamp := time.Now().UTC().Format("20060102-150405")
	toDB, blogPublic := "", ""
	if existing {
		if toDB, err = to.dbOf(ctx); err != nil {
			return fail("the target site: %v", err)
		}
		if toDB == fromDB {
			return fail("both sites use the same database")
		}
		fp, _ := from.wp(ctx, false, "config", "get", "table_prefix", "--type=variable")
		tp, _ := to.wp(ctx, false, "config", "get", "table_prefix", "--type=variable")
		if p.Database && fp != tp {
			return fail("the sites use different table prefixes (%q and %q)", fp, tp)
		}
		blogPublic, _ = to.wp(ctx, false, "option", "get", "blog_public")
		to.wp(ctx, false, "maintenance-mode", "activate")
		defer to.wp(ctx, false, "maintenance-mode", "deactivate")
	} else {
		toDB = p.ToDB.Name
	}
	if err := os.MkdirAll(sys.path(cloneTmpDir), 0o700); err != nil {
		return fail("%v", err)
	}

	if p.Database && existing {
		// The target's database as it was, kept in its own tmp folder.
		res.Backup = to.home + "/tmp/ekpanel-before-copy-" + stamp + ".sql"
		if err := dumpDB(ctx, sys, toDB, res.Backup); err != nil {
			return fail("save the target's database first: %v", err)
		}
		sys.run(ctx, "chown", p.To.User+":"+p.To.User, res.Backup)
	}
	if p.Files {
		if err := ensurePackage(ctx, sys, "rsync"); err != nil {
			return fail("install rsync: %v", err)
		}
		args := []string{"-a", "--delete", "--exclude=/.maintenance"}
		if existing {
			// The target keeps its own configuration; files the copy replaces or removes
			// are kept aside in its tmp folder.
			args = append(args, "--exclude=/wp-config.php", "--backup", "--backup-dir="+to.home+"/tmp/ekpanel-before-copy-"+stamp+"-files")
		}
		args = append(args, from.docroot+"/", to.docroot+"/")
		if _, err := sys.run(ctx, "rsync", args...); err != nil {
			return fail("copy files: %s", lastLines([]byte(err.Error()), 3))
		}
		if _, err := sys.run(ctx, "chown", "-R", p.To.User+":"+p.To.User, to.docroot, to.home+"/tmp"); err != nil {
			return fail("%v", err)
		}
		if _, err := sys.run(ctx, "setfacl", "-R", "-m", "u:caddy:rX,d:u:caddy:rX", to.docroot); err != nil {
			return fail("%v", err)
		}
		if !existing {
			if err := to.setDBConfig(*p.ToDB); err != nil {
				return fail("%v", err)
			}
		}
	}
	if p.Database {
		dump := cloneTmpDir + "/" + p.From.User + "-" + stamp + ".sql"
		defer os.Remove(sys.path(dump))
		if err := dumpDB(ctx, sys, fromDB, dump); err != nil {
			return fail("read the source database: %v", err)
		}
		if err := emptyDB(ctx, sys, toDB); err != nil {
			return fail("clear the target database: %v", err)
		}
		f, err := os.Open(sys.path(dump))
		if err != nil {
			return fail("%v", err)
		}
		_, err = sys.exec(ctx, command{name: "mariadb", args: []string{toDB}, stdin: f})
		f.Close()
		if err != nil {
			return fail("write the target database: %s", lastLines([]byte(err.Error()), 3))
		}
		// Addresses in content and settings, also inside serialized data.
		for _, pair := range [][2]string{{"//" + p.From.Domain, "//" + p.To.Domain}, {`\/\/` + p.From.Domain, `\/\/` + p.To.Domain}} {
			if _, err := to.wp(ctx, false, "search-replace", pair[0], pair[1], "--all-tables-with-prefix", "--skip-columns=guid", "--quiet"); err != nil {
				return fail("change addresses to %s: %s", p.To.Domain, lastLines([]byte(err.Error()), 2))
			}
		}
		if blogPublic != "" {
			to.wp(ctx, false, "option", "update", "blog_public", blogPublic)
		}
	}
	if p.NoIndex {
		to.wp(ctx, false, "option", "update", "blog_public", "0")
	}
	to.wp(ctx, false, "cache", "flush")
	to.as(ctx, "chmod", "600", to.docroot+"/wp-config.php")
	res.Info = to.info(ctx)
	return res, nil
}

// dumpDB writes a MariaDB database to a file only root can read.
func dumpDB(ctx context.Context, sys *system, db, path string) error {
	f, err := os.OpenFile(sys.path(path), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	_, err = sys.exec(ctx, command{name: "mariadb-dump", args: []string{"--single-transaction", "--quick", "--no-tablespaces", db}, stdout: f, stderr: &stderr})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("%v %s", err, lastLines(stderr.Bytes(), 2))
	}
	return nil
}

// emptyDB drops every table of a database, so a copy leaves none of the old ones behind.
func emptyDB(ctx context.Context, sys *system, db string) error {
	out, err := sys.run(ctx, "mariadb", "-N", "-B", "-e", "SELECT table_name FROM information_schema.tables WHERE table_schema = '"+db+"'")
	if err != nil {
		return err
	}
	var tables []string
	for t := range strings.FieldsSeq(string(out)) {
		if !tableNameRE.MatchString(t) {
			return fmt.Errorf("unexpected table name %q", t)
		}
		tables = append(tables, "`"+t+"`")
	}
	if len(tables) == 0 {
		return nil
	}
	_, err = sys.run(ctx, "mariadb", db, "-e", "SET FOREIGN_KEY_CHECKS=0; DROP TABLE IF EXISTS "+strings.Join(tables, ", "))
	return err
}
