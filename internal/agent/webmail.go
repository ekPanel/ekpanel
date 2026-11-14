package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"ekpanel/internal/agentproto"
)

// Webmail is Roundcube at https://<mail host name>/, run by the system's PHP-FPM as an
// account of its own. It signs users in to the mail server over loopback, so it never
// holds a password beyond the user's session.

const (
	roundcubeVersion = "1.7.5"
	// roundcubeSHA256 is the digest of roundcubemail-<version>-complete.tar.gz as listed
	// on roundcube.net/download.
	roundcubeSHA256 = "93a66c94e3efff7db93187c1c9d7f35546647a3e1e7b9327cc87911564edf9d1"
	roundcubeDir    = railpackDir + "/roundcube-" + roundcubeVersion
	webmailUser     = "ekpanel-webmail"
	webmailData     = "/var/lib/ekpanel-webmail"
	webmailKeyPath  = "/etc/ekpanel/webmail/des_key"
	webmailSocket   = "/run/php/ekpanel-webmail.sock"
	webmailCaddy    = caddySitesDir + "/webmail.caddy"
	webmailID       = "webmail" // its result among the sites' while applying
)

func webmailGroup() fpmGroup { return fpmGroup{version: agentproto.DefaultPHPVersion} }

func webmailPool() string { return webmailGroup().poolDir() + "/ekpanel-webmail.conf" }

var webmailPoolTemplate = template.Must(template.New("pool").Parse(`; Managed by ekPanel (webmail). Changes here are overwritten.
[ekpanel-webmail]
user = {{.User}}
group = {{.User}}
listen = {{.Socket}}
listen.owner = caddy
listen.group = caddy
listen.mode = 0660

pm = ondemand
pm.max_children = 10
pm.process_idle_timeout = 30s
pm.max_requests = 500
chdir = /

php_admin_value[open_basedir] = {{.Root}}/:{{.Data}}/:/usr/share/php/
php_admin_value[upload_tmp_dir] = {{.Data}}/temp
php_admin_value[session.save_path] = {{.Data}}/temp
php_admin_value[sys_temp_dir] = {{.Data}}/temp
php_admin_value[upload_max_filesize] = 25M
php_admin_value[post_max_size] = 26M
php_admin_value[memory_limit] = 128M
php_admin_flag[display_errors] = off
php_admin_value[error_log] = {{.Data}}/logs/php.log
`))

var webmailCaddyTemplate = template.Must(template.New("caddy").Parse(`# Managed by ekPanel (webmail). Changes here are overwritten.
{{.Host}} {
{{- if .Internal}}
	tls internal
{{- end}}
	root * {{.Root}}/public_html
	encode zstd gzip
	@hidden {
		path */.*
		not path /.well-known/*
	}
	respond @hidden 404
	header {
		X-Content-Type-Options nosniff
		Referrer-Policy same-origin
		-Server
	}
	php_fastcgi unix/{{.Socket}}
	file_server
}
`))

// webmailConfig is Roundcube's config.inc.php. Mail apps' connections to the mail server
// go over loopback, where the certificate (made for the host name) cannot match.
var webmailConfig = template.Must(template.New("config").Parse(`<?php
// Managed by ekPanel. Changes here are overwritten.
$config = [];
$config['db_dsnw'] = 'sqlite:///{{.Data}}/db/roundcube.sqlite?mode=0600';
$config['imap_host'] = 'ssl://127.0.0.1:993';
$config['smtp_host'] = 'ssl://127.0.0.1:465';
$config['smtp_user'] = '%u';
$config['smtp_pass'] = '%p';
$config['imap_conn_options'] = ['ssl' => ['verify_peer' => false, 'verify_peer_name' => false]];
$config['smtp_conn_options'] = ['ssl' => ['verify_peer' => false, 'verify_peer_name' => false]];
$config['des_key'] = '{{.Key}}';
$config['product_name'] = 'Webmail';
$config['support_url'] = '';
$config['skin'] = 'elastic';
$config['plugins'] = ['archive', 'zipdownload', 'markasjunk'];
$config['temp_dir'] = '{{.Data}}/temp/';
$config['log_dir'] = '{{.Data}}/logs/';
$config['enable_installer'] = false;
$config['use_https'] = true;
$config['ip_check'] = true;
$config['session_lifetime'] = 60;
$config['login_autocomplete'] = 2;
$config['mail_domain'] = '%d';
$config['login_username_filter'] = 'email';
$config['login_rate_limit'] = 3;
`))

// prepareWebmail installs Roundcube and writes its PHP pool and Caddy site; finish reloads
// them with the websites. With spec nil (no email) it removes the pool and site.
func (a *applier) prepareWebmail(ctx context.Context, spec *agentproto.MailSpec) error {
	if spec == nil {
		if old, err := os.ReadFile(a.sys.path(webmailCaddy)); err == nil && os.Remove(a.sys.path(webmailCaddy)) == nil {
			a.caddyChanged[webmailCaddy] = old
		}
		a.removePool(a.sys.path(webmailPool()), webmailGroup())
		return nil
	}
	if !agentproto.ValidMailHost(spec.Hostname) {
		return fmt.Errorf("invalid mail host name %q", spec.Hostname)
	}
	g := webmailGroup()
	if err := a.ensurePHP(ctx, g.version); err != nil {
		return err
	}
	if err := ensurePackage(ctx, a.sys, "php"+g.version+"-sqlite3"); err != nil {
		return fmt.Errorf("install PHP SQLite: %w", err)
	}
	if err := installRoundcube(ctx, a.sys); err != nil {
		return err
	}
	if _, err := a.sys.run(ctx, "id", "-u", webmailUser); err != nil {
		if _, err := a.sys.run(ctx, "useradd", "--system", "--home-dir", webmailData, "--no-create-home", "--shell", "/usr/sbin/nologin", webmailUser); err != nil {
			return fmt.Errorf("create the webmail account: %w", err)
		}
	}
	for _, d := range []string{webmailData, webmailData + "/db", webmailData + "/temp", webmailData + "/logs"} {
		if err := os.MkdirAll(a.sys.path(d), 0o700); err != nil {
			return err
		}
	}
	if _, err := a.sys.run(ctx, "chown", "-R", webmailUser+":"+webmailUser, webmailData); err != nil {
		return err
	}
	key, err := webmailKey(a.sys)
	if err != nil {
		return err
	}
	vars := map[string]any{"Data": webmailData, "Key": key, "Root": roundcubeDir, "User": webmailUser, "Socket": webmailSocket,
		"Host": spec.Hostname, "Internal": spec.TLS == "internal"}
	var conf bytes.Buffer
	webmailConfig.Execute(&conf, vars)
	confPath := roundcubeDir + "/config/config.inc.php"
	if _, _, err := a.writeIfChanged(confPath, conf.Bytes()); err != nil {
		return err
	}
	if err := os.Chmod(a.sys.path(confPath), 0o640); err != nil {
		return err
	}
	if _, err := a.sys.run(ctx, "chown", "root:"+webmailUser, confPath); err != nil {
		return err
	}
	if err := webmailDatabase(ctx, a.sys, g.version); err != nil {
		return err
	}
	var pool bytes.Buffer
	webmailPoolTemplate.Execute(&pool, vars)
	if changed, _, err := a.writeIfChanged(webmailPool(), pool.Bytes()); err != nil {
		return err
	} else if changed {
		a.reloadPHP[g] = true
	}
	var site bytes.Buffer
	webmailCaddyTemplate.Execute(&site, vars)
	changed, prev, err := a.writeIfChanged(webmailCaddy, site.Bytes())
	if err != nil {
		return err
	}
	if changed {
		a.caddyChanged[webmailCaddy] = prev
		a.caddySites[webmailCaddy] = webmailID
	}
	return nil
}

func webmailKey(sys *system) (string, error) {
	if b, err := os.ReadFile(sys.path(webmailKeyPath)); err == nil && len(strings.TrimSpace(string(b))) == 24 {
		return strings.TrimSpace(string(b)), nil
	}
	buf := make([]byte, 12)
	rand.Read(buf)
	key := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(sys.path(webmailKeyPath)), 0o700); err != nil {
		return "", err
	}
	return key, os.WriteFile(sys.path(webmailKeyPath), []byte(key+"\n"), 0o600)
}

// webmailDatabase creates Roundcube's database the first time and updates its tables
// when a new Roundcube version is installed.
func webmailDatabase(ctx context.Context, sys *system, phpVersion string) error {
	db := webmailData + "/db/roundcube.sqlite"
	marker := webmailData + "/db/version"
	php := "php" + phpVersion
	if _, err := os.Stat(sys.path(db)); err != nil {
		// Roundcube's SQLite driver makes the tables itself when it opens a new file, so
		// initdb can find them there already: that is the database made, not a failure.
		if _, err := sys.run(ctx, "runuser", "-u", webmailUser, "--", php, roundcubeDir+"/bin/initdb.sh", "--dir="+roundcubeDir+"/SQL"); err != nil {
			if _, statErr := os.Stat(sys.path(db)); statErr != nil || !strings.Contains(err.Error(), "already exists") {
				return fmt.Errorf("create the webmail database: %w", err)
			}
		}
	} else if b, _ := os.ReadFile(sys.path(marker)); strings.TrimSpace(string(b)) != roundcubeVersion {
		args := []string{"-u", webmailUser, "--", php, roundcubeDir + "/bin/updatedb.sh", "--package=roundcube", "--dir=" + roundcubeDir + "/SQL"}
		if v := strings.TrimSpace(string(b)); v != "" {
			args = append(args, "--version="+v)
		}
		if _, err := sys.run(ctx, "runuser", args...); err != nil {
			return fmt.Errorf("update the webmail database: %w", err)
		}
	}
	return os.WriteFile(sys.path(marker), []byte(roundcubeVersion+"\n"), 0o644)
}

func installRoundcube(ctx context.Context, sys *system) error {
	if _, err := os.Stat(sys.path(roundcubeDir + "/public_html/index.php")); err == nil {
		return nil
	}
	url := fmt.Sprintf("https://github.com/roundcube/roundcubemail/releases/download/%[1]s/roundcubemail-%[1]s-complete.tar.gz", roundcubeVersion)
	archive, err := sys.fetch(ctx, url)
	if err != nil {
		return fmt.Errorf("download webmail: %w", err)
	}
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != roundcubeSHA256 {
		return errors.New("the webmail download does not match its published checksum")
	}
	tmp := sys.path(roundcubeDir + ".tmp")
	os.RemoveAll(tmp)
	if err := untarAll(archive, "roundcubemail-"+roundcubeVersion+"/", tmp); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("unpack webmail: %w", err)
	}
	os.RemoveAll(filepath.Join(tmp, "installer"))
	os.Remove(filepath.Join(tmp, "public_html", "installer.php"))
	os.RemoveAll(sys.path(roundcubeDir))
	return os.Rename(tmp, sys.path(roundcubeDir))
}

// untarAll unpacks the regular files and folders under prefix in a .tar.gz into dest,
// owned by root and readable by everyone. Anything else (links, devices) is refused.
func untarAll(archive []byte, prefix, dest string) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue // archive-wide metadata, not a file
		}
		rel, ok := strings.CutPrefix(strings.TrimPrefix(h.Name, "./"), prefix)
		if !ok {
			return fmt.Errorf("unexpected path %q", h.Name)
		}
		if rel == "" {
			continue
		}
		clean := filepath.Clean(rel)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("unsafe path %q", h.Name)
		}
		target := filepath.Join(dest, clean)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, io.LimitReader(tr, 64<<20))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported entry %q", h.Name)
		}
	}
}
