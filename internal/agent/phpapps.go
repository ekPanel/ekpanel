package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"ekpanel/internal/agentproto"
)

// Release archives of the applications that publish them, checked against the SHA-256
// their publishers list (Joomla on GitHub, Moodle on packaging.moodle.org).
var phpAppArchives = map[string]struct {
	url, sha256, prefix string
}{
	"joomla": {"https://github.com/joomla/joomla-cms/releases/download/6.2.0/Joomla_6.2.0-Stable-Full_Package.tar.gz",
		"f7a4c3962c4a8682f5847ea9d47d2848f6eb475109805935c9e19a5c6511829d", ""},
	// 5.3 needs MariaDB 11.4; Ubuntu 24.04 and Debian 12 have 10.11, which 5.2 supports.
	"moodle": {"https://packaging.moodle.org/stable502/moodle-5.2.4.tgz",
		"8569b63f1e97416ecb67ec75767dd20675892c7a62a5e90d1647366fddf873cf", "moodle/"},
}

const (
	composerVersion = "2.11.1"
	composerSHA256  = "6f1b891e1d4927d62740276c540fb490e47689e19e102c49e8da849794882f8c"
	composerPath    = railpackDir + "/composer-" + composerVersion + ".phar"
)

// secretWrapper runs a PHP command-line script with extra options taken from the
// environment, which only the site's own account can read; options given as arguments
// would show in the process list of every account.
const secretWrapper = `<?php
// Written by ekPanel for one install, and removed after it.
$script = $argv[1];
$args = array_slice($argv, 2);
foreach (json_decode((string) getenv('EKPANEL_SECRET_ARGS'), true) ?: [] as $a) {
    $args[] = $a;
}
putenv('EKPANEL_SECRET_ARGS');
unset($_ENV['EKPANEL_SECRET_ARGS'], $_SERVER['EKPANEL_SECRET_ARGS']);
$argv = $_SERVER['argv'] = array_merge([$script], $args);
$argc = $_SERVER['argc'] = count($argv);
require $script;
`

// phpAppSite runs commands for an install as the site's account.
type phpAppSite struct {
	sys     *system
	p       agentproto.PHPAppInstallParams
	home    string
	docroot string // public_html
}

// php runs a PHP script as the site's account; secret options and environment variables
// travel in the environment only.
func (s *phpAppSite) php(ctx context.Context, dir string, secretArgs []string, env []string, args ...string) (string, error) {
	cmdEnv := append([]string{"HOME=" + s.home + "/tmp", "COMPOSER_HOME=" + s.home + "/tmp/.composer"}, env...)
	// -m keeps this environment (runuser would otherwise set HOME to the home folder, which
	// belongs to root); the command starts in dir.
	argv := []string{"-m", "-u", s.p.User, "--", "php" + s.p.PHPVersion, "-d", "memory_limit=-1", "-d", "max_input_vars=5000"}
	if len(secretArgs) > 0 {
		b, _ := json.Marshal(secretArgs)
		cmdEnv = append(cmdEnv, "EKPANEL_SECRET_ARGS="+string(b))
		argv = append(argv, s.home+"/tmp/ekpanel-run.php")
	}
	argv = append(argv, args...)
	out, err := s.sys.exec(ctx, command{name: "runuser", args: argv, env: cmdEnv, dir: s.sys.path(dir)})
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, lastLines(out, 6))
	}
	return strings.TrimSpace(string(out)), nil
}

// writeAsSite writes a file owned by the site's account, readable by it only.
func (s *phpAppSite) writeAsSite(path string, content []byte) error {
	if err := os.WriteFile(s.sys.path(path), content, 0o600); err != nil {
		return err
	}
	_, err := s.sys.run(context.Background(), "chown", s.p.User+":"+s.p.User, path)
	return err
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func installPHPApp(ctx context.Context, sys *system, p agentproto.PHPAppInstallParams) (*agentproto.PHPAppInstallResult, *agentproto.OpError) {
	if err := p.Validate(); err != nil {
		return nil, opError(agentproto.ErrInvalidParams, "%v", err)
	}
	out, err := sys.run(ctx, "id", "-nG", p.User)
	if err != nil || !slices.Contains(strings.Fields(string(out)), sitesGroup) {
		return nil, opError(agentproto.ErrNotAllowed, "%q is not an ekPanel site account", p.User)
	}
	s := &phpAppSite{sys: sys, p: p, home: "/home/" + p.User, docroot: "/home/" + p.User + "/public_html"}
	empty, err := emptyForInstall(sys, s.docroot)
	if err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	if !empty {
		return nil, opError("not_empty", "public_html already has files; move or delete them first so nothing is overwritten")
	}
	os.Remove(sys.path(s.docroot + "/index.html"))
	if err := s.writeAsSite(s.home+"/tmp/ekpanel-run.php", []byte(secretWrapper)); err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	defer os.Remove(sys.path(s.home + "/tmp/ekpanel-run.php"))
	var version string
	switch p.App {
	case "joomla":
		version, err = s.joomla(ctx)
	case "moodle":
		version, err = s.moodle(ctx)
	case "drupal":
		version, err = s.drupal(ctx)
	case "laravel":
		version, err = s.laravel(ctx)
	}
	if err != nil {
		// public_html was empty before: make it so again, with an empty database, so the
		// install can simply be tried again.
		s.cleanUp(ctx)
		return nil, opError(agentproto.ErrFailed, "%s install failed: %v", p.App, err)
	}
	// The web server reads the new files through its ACL entry.
	if _, err := sys.run(ctx, "setfacl", "-R", "-m", "u:caddy:rX,d:u:caddy:rX", s.docroot); err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	return &agentproto.PHPAppInstallResult{Version: version}, nil
}

// unpack downloads an application's release archive, checks it and unpacks it into
// public_html, owned by the site's account.
func (s *phpAppSite) unpack(ctx context.Context) error {
	a := phpAppArchives[s.p.App]
	archive, err := s.sys.fetch(ctx, a.url)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != a.sha256 {
		return errors.New("the download does not match its published checksum")
	}
	if err := untarAll(archive, a.prefix, s.sys.path(s.docroot)); err != nil {
		return fmt.Errorf("unpack: %w", err)
	}
	_, err = s.sys.run(ctx, "chown", "-R", s.p.User+":"+s.p.User, s.docroot)
	return err
}

func (s *phpAppSite) joomla(ctx context.Context) (string, error) {
	if err := s.unpack(ctx); err != nil {
		return "", err
	}
	p := s.p
	_, err := s.php(ctx, s.docroot, []string{"--admin-password=" + p.AdminPassword, "--db-pass=" + p.DB.Password}, nil,
		"installation/joomla.php", "install", "--no-interaction", "--site-name="+p.Title, "--admin-user="+p.AdminUser,
		"--admin-username="+p.AdminUser, "--admin-email="+p.AdminEmail, "--db-type=mysqli", "--db-host=localhost",
		"--db-user="+p.DB.User, "--db-name="+p.DB.Name, "--db-prefix=j"+randomHex(2)+"_", "--db-encryption=0")
	if err != nil {
		return "", err
	}
	// The installer removes itself after a web install only.
	os.RemoveAll(s.sys.path(s.docroot + "/installation"))
	s.sys.run(ctx, "chmod", "600", s.docroot+"/configuration.php")
	app, _ := agentproto.FindPHPApp("joomla")
	return app.Version, nil
}

func (s *phpAppSite) moodle(ctx context.Context) (string, error) {
	if err := s.unpack(ctx); err != nil {
		return "", err
	}
	p := s.p
	// Uploaded files live outside the web root, in the site's home folder (which belongs
	// to root, so the agent makes the folder).
	data := s.home + "/moodledata"
	if err := os.MkdirAll(s.sys.path(data), 0o700); err != nil {
		return "", err
	}
	if _, err := s.sys.run(ctx, "chown", p.User+":"+p.User, data); err != nil {
		return "", err
	}
	short := strings.Map(func(r rune) rune {
		if r == ' ' || unicode.IsControl(r) {
			return -1
		}
		return r
	}, p.Title)
	if r := []rune(short); len(r) > 20 {
		short = string(r[:20])
	}
	if short == "" {
		short = "moodle"
	}
	_, err := s.php(ctx, s.docroot, []string{"--dbpass=" + p.DB.Password, "--adminpass=" + p.AdminPassword}, nil,
		"admin/cli/install.php", "--non-interactive", "--agree-license", "--lang=en", "--wwwroot=https://"+p.Domain, "--dataroot="+data,
		"--dbtype=mariadb", "--dbhost=localhost", "--dbname="+p.DB.Name, "--dbuser="+p.DB.User, "--fullname="+p.Title,
		"--shortname="+short, "--adminuser="+strings.ToLower(p.AdminUser), "--adminemail="+p.AdminEmail)
	if err != nil {
		return "", err
	}
	s.sys.run(ctx, "chmod", "600", s.docroot+"/config.php")
	return "5.2.4", nil
}

// ensureComposer downloads the pinned Composer once.
func ensureComposer(ctx context.Context, sys *system) error {
	if _, err := os.Stat(sys.path(composerPath)); err == nil {
		return nil
	}
	phar, err := sys.fetch(ctx, "https://github.com/composer/composer/releases/download/"+composerVersion+"/composer.phar")
	if err != nil {
		return fmt.Errorf("download Composer: %w", err)
	}
	if got := sha256.Sum256(phar); hex.EncodeToString(got[:]) != composerSHA256 {
		return errors.New("the Composer download does not match its published checksum")
	}
	if err := os.MkdirAll(filepath.Dir(sys.path(composerPath)), 0o755); err != nil {
		return err
	}
	tmp := sys.path(composerPath) + ".tmp"
	if err := os.WriteFile(tmp, phar, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, sys.path(composerPath))
}

func (s *phpAppSite) composer(ctx context.Context, args ...string) error {
	_, err := s.php(ctx, s.docroot, nil, []string{"COMPOSER_NO_INTERACTION=1", "COMPOSER_ALLOW_SUPERUSER=0"},
		append([]string{composerPath, "--no-interaction", "--no-progress"}, args...)...)
	return err
}

var drupalVersionRE = regexp.MustCompile(`(?m)^Drupal version\s*:\s*([0-9.]+)`)

func (s *phpAppSite) drupal(ctx context.Context) (string, error) {
	if err := ensureComposer(ctx, s.sys); err != nil {
		return "", err
	}
	if err := s.composer(ctx, "create-project", "drupal/recommended-project:^11", "."); err != nil {
		return "", err
	}
	if err := s.composer(ctx, "require", "drush/drush:^13"); err != nil {
		return "", err
	}
	p := s.p
	// The database settings go into settings.php directly, so Drush needs no password.
	sites := s.docroot + "/web/sites/default"
	base, err := os.ReadFile(s.sys.path(sites + "/default.settings.php"))
	if err != nil {
		return "", err
	}
	host := strings.ReplaceAll(p.Domain, ".", `\.`)
	extra := fmt.Sprintf(`
// Added by ekPanel.
$databases['default']['default'] = [
  'database' => '%s',
  'username' => '%s',
  'password' => '%s',
  'host' => 'localhost',
  'driver' => 'mysql',
  'prefix' => '',
  'collation' => 'utf8mb4_general_ci',
];
$settings['hash_salt'] = '%s';
$settings['trusted_host_patterns'] = ['^%s$'];
$settings['config_sync_directory'] = '../config/sync';
`, p.DB.Name, p.DB.User, p.DB.Password, randomHex(32), host)
	if err := s.writeAsSite(sites+"/settings.php", append(base, []byte(extra)...)); err != nil {
		return "", err
	}
	// vendor/bin/drush and vendor/drush/drush/drush are shell wrappers; PHP runs drush.php.
	drush := s.docroot + "/vendor/drush/drush/drush.php"
	// A throwaway password for the install (which takes it as an argument), replaced at once.
	if _, err := s.php(ctx, s.docroot, nil, nil, drush, "site:install", "standard", "--yes", "--site-name="+p.Title,
		"--account-name="+p.AdminUser, "--account-mail="+p.AdminEmail, "--account-pass="+randomHex(16)); err != nil {
		return "", err
	}
	if _, err := s.php(ctx, s.docroot, nil, []string{"EKPANEL_ADMIN_PASSWORD=" + p.AdminPassword}, drush, "php:eval",
		`\Drupal\user\Entity\User::load(1)->setPassword(getenv('EKPANEL_ADMIN_PASSWORD'))->save();`); err != nil {
		return "", err
	}
	s.sys.run(ctx, "chmod", "400", sites+"/settings.php")
	out, _ := s.php(ctx, s.docroot, nil, nil, drush, "status")
	m := drupalVersionRE.FindStringSubmatch(out)
	if m == nil || !strings.Contains(out, "Successful") {
		return "", fmt.Errorf("Drupal does not start after the install: %s", lastLines([]byte(out), 4))
	}
	return m[1], nil
}

var laravelVersionRE = regexp.MustCompile(`([0-9]+\.[0-9]+\.[0-9]+)`)

func (s *phpAppSite) laravel(ctx context.Context) (string, error) {
	if err := ensureComposer(ctx, s.sys); err != nil {
		return "", err
	}
	if err := s.composer(ctx, "create-project", "laravel/laravel:^13", ".", "--no-scripts"); err != nil {
		return "", err
	}
	p := s.p
	example, err := os.ReadFile(s.sys.path(s.docroot + "/.env.example"))
	if err != nil {
		return "", err
	}
	set := map[string]string{"APP_NAME": `"` + strings.ReplaceAll(p.Title, `"`, ``) + `"`, "APP_ENV": "production", "APP_DEBUG": "false",
		"APP_URL": "https://" + p.Domain, "DB_CONNECTION": "mariadb", "DB_HOST": "localhost", "DB_PORT": "3306",
		"DB_DATABASE": p.DB.Name, "DB_USERNAME": p.DB.User, "DB_PASSWORD": p.DB.Password}
	var lines []string
	for line := range strings.Lines(string(example)) {
		line = strings.TrimRight(line, "\n")
		key, _, _ := strings.Cut(strings.TrimPrefix(line, "# "), "=")
		if v, ok := set[key]; ok {
			line = key + "=" + v
			delete(set, key)
		}
		lines = append(lines, line)
	}
	for _, k := range []string{"DB_HOST", "DB_PORT", "DB_DATABASE", "DB_USERNAME", "DB_PASSWORD"} {
		if v, ok := set[k]; ok {
			lines = append(lines, k+"="+v)
		}
	}
	if err := s.writeAsSite(s.docroot+"/.env", []byte(strings.Join(lines, "\n")+"\n")); err != nil {
		return "", err
	}
	for _, args := range [][]string{{"key:generate", "--force"}, {"migrate", "--force"}, {"storage:link"}} {
		if _, err := s.php(ctx, s.docroot, nil, nil, append([]string{"artisan"}, args...)...); err != nil {
			return "", err
		}
	}
	out, _ := s.php(ctx, s.docroot, nil, nil, "artisan", "--version")
	if m := laravelVersionRE.FindStringSubmatch(out); m != nil {
		return m[1], nil
	}
	return "13", nil
}

// cleanUp removes what a failed install left: files in public_html and moodledata, and
// tables in its database.
func (s *phpAppSite) cleanUp(ctx context.Context) {
	for _, dir := range []string{s.docroot, s.home + "/moodledata"} {
		entries, _ := os.ReadDir(s.sys.path(dir))
		for _, e := range entries {
			os.RemoveAll(s.sys.path(filepath.Join(dir, e.Name())))
		}
	}
	os.Remove(s.sys.path(s.home + "/moodledata"))
	emptyDB(ctx, s.sys, s.p.DB.Name)
}
