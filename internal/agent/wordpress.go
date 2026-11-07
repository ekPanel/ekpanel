package agent

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"ekpanel/internal/agentproto"
)

const (
	wpCLIPath = "/usr/local/lib/ekpanel/wp-cli.phar"
	wpCLIURL  = "https://raw.githubusercontent.com/wp-cli/builds/gh-pages/phar/wp-cli.phar"
)

var (
	wpLocaleRE    = regexp.MustCompile(`^[a-z]{2,3}(_[A-Z]{2})?$`)
	wpAdminUserRE = regexp.MustCompile(`^[A-Za-z0-9._@-]{3,60}$`)
	plainPassRE   = regexp.MustCompile(`^[A-Za-z0-9]{16,64}$`)
)

func validateWordPress(p agentproto.WordPressInstallParams) error {
	switch {
	case !userRE.MatchString(p.User) || slices.Contains(reserved, p.User):
		return errors.New("invalid site account")
	case !hostnameRE.MatchString(p.Domain):
		return errors.New("invalid domain")
	case !slices.Contains(agentproto.PHPVersions, p.PHPVersion):
		return errors.New("invalid PHP version")
	case !dbNameRE.MatchString(p.DBName) || !dbNameRE.MatchString(p.DBUser):
		return errors.New("invalid database name")
	case !plainPassRE.MatchString(p.DBPassword) || !plainPassRE.MatchString(p.AdminPassword):
		return errors.New("invalid generated password")
	case !wpAdminUserRE.MatchString(p.AdminUser):
		return errors.New("the admin user name may use letters, digits and . _ @ - (3–60 characters)")
	case !wpLocaleRE.MatchString(p.Locale):
		return errors.New("invalid language")
	}
	if a, err := mail.ParseAddress(p.AdminEmail); err != nil || a.Address != p.AdminEmail {
		return errors.New("invalid admin email address")
	}
	if p.Title == "" || utf8.RuneCountInString(p.Title) > 100 || strings.IndexFunc(p.Title, unicode.IsControl) >= 0 {
		return errors.New("the site title must be 1–100 characters on one line")
	}
	return nil
}

// ensureWPCLI downloads WP-CLI once and checks it against the published SHA-512.
func ensureWPCLI(ctx context.Context, sys *system) error {
	if _, err := os.Stat(sys.path(wpCLIPath)); err == nil {
		return nil
	}
	phar, err := sys.fetch(ctx, wpCLIURL)
	if err != nil {
		return fmt.Errorf("download WP-CLI: %w", err)
	}
	sumFile, err := sys.fetch(ctx, wpCLIURL+".sha512")
	if err != nil {
		return fmt.Errorf("download WP-CLI checksum: %w", err)
	}
	want := strings.Fields(string(sumFile))
	got := sha512.Sum512(phar)
	if len(want) == 0 || !strings.EqualFold(want[0], hex.EncodeToString(got[:])) {
		return errors.New("WP-CLI download does not match its published checksum")
	}
	if err := os.MkdirAll(filepath.Dir(sys.path(wpCLIPath)), 0o755); err != nil {
		return err
	}
	tmp := sys.path(wpCLIPath) + ".tmp"
	if err := os.WriteFile(tmp, phar, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, sys.path(wpCLIPath))
}

// emptyForInstall reports whether public_html holds nothing but ekPanel's placeholder page.
func emptyForInstall(sys *system, docroot string) (bool, error) {
	items, err := os.ReadDir(sys.path(docroot))
	if err != nil {
		return false, err
	}
	for _, it := range items {
		if it.Name() == "index.html" {
			b, _ := os.ReadFile(sys.path(docroot + "/index.html"))
			if bytes.Contains(b, []byte("is hosted with ekPanel")) {
				continue
			}
		}
		return false, nil
	}
	return true, nil
}

func installWordPress(ctx context.Context, sys *system, p agentproto.WordPressInstallParams) (*agentproto.WordPressInstallResult, *agentproto.OpError) {
	if err := validateWordPress(p); err != nil {
		return nil, opError(agentproto.ErrInvalidParams, "%v", err)
	}
	out, err := sys.run(ctx, "id", "-nG", p.User)
	if err != nil || !slices.Contains(strings.Fields(string(out)), sitesGroup) {
		return nil, opError(agentproto.ErrNotAllowed, "%q is not an ekPanel site account", p.User)
	}
	home := "/home/" + p.User
	docroot := home + "/public_html"
	empty, err := emptyForInstall(sys, docroot)
	if err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	if !empty {
		return nil, opError("not_empty", "public_html already has files; move or delete them first so nothing is overwritten")
	}
	if err := ensureWPCLI(ctx, sys); err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	os.Remove(sys.path(docroot + "/index.html"))

	// WP-CLI runs as the site's account; its cache lives in the site's tmp folder.
	wp := func(input string, args ...string) (string, error) {
		cmd := append([]string{"-u", p.User, "--", "env", "HOME=" + home + "/tmp", "WP_CLI_CACHE_DIR=" + home + "/tmp/.wp-cli-cache",
			"php" + p.PHPVersion, wpCLIPath, "--path=" + docroot}, args...)
		b, err := sys.runInput(ctx, input, "runuser", cmd...)
		return strings.TrimSpace(string(b)), err
	}
	steps := []struct {
		input string
		args  []string
	}{
		{"", []string{"core", "download"}},
		{p.DBPassword + "\n", []string{"config", "create", "--dbname=" + p.DBName, "--dbuser=" + p.DBUser, "--dbhost=localhost",
			"--locale=" + p.Locale, "--prompt=dbpass"}},
		// Files belong to the same account PHP runs as, so updates need no FTP details.
		{"", []string{"config", "set", "FS_METHOD", "direct"}},
		{p.AdminPassword + "\n", []string{"core", "install", "--url=https://" + p.Domain, "--title=" + p.Title,
			"--admin_user=" + p.AdminUser, "--admin_email=" + p.AdminEmail, "--locale=" + p.Locale, "--skip-email", "--prompt=admin_password"}},
	}
	for _, s := range steps {
		if _, err := wp(s.input, s.args...); err != nil {
			return nil, opError(agentproto.ErrFailed, "WordPress install step %q failed: %v", strings.Join(s.args[:2], " "), err)
		}
	}
	if p.Locale != "en_US" {
		// A missing translation is not worth failing the install over.
		wp("", "language", "core", "install", p.Locale, "--activate")
	}
	// Only the site's account (which PHP runs as) may read the database password.
	if _, err := sys.run(ctx, "chmod", "600", docroot+"/wp-config.php"); err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	version, _ := wp("", "core", "version")
	return &agentproto.WordPressInstallResult{Version: version}, nil
}
