package agent

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ekpanel/internal/agentproto"
)

func wpParams() agentproto.WordPressInstallParams {
	return agentproto.WordPressInstallParams{
		User: "blog_ab12", Domain: "blog.example.com", PHPVersion: "8.4", DBName: "wp_blog_ab12", DBUser: "wp_blog_ab12",
		DBPassword: "DbPassword1234567890abcd", Title: "আমার ব্লগ", AdminUser: "admin", AdminPassword: "AdminPassword123456789xy",
		AdminEmail: "owner@example.com", Locale: "bn_BD",
	}
}

func wpFake(t *testing.T) *fakeSystem {
	f := newFakeSystem(t)
	f.users["blog_ab12"] = []string{"blog_ab12", sitesGroup}
	phar := []byte("fake phar")
	sum := sha512.Sum512(phar)
	f.fetch = func(_ context.Context, url string) ([]byte, error) {
		if strings.HasSuffix(url, ".sha512") {
			return []byte(hex.EncodeToString(sum[:]) + "  wp-cli.phar\n"), nil
		}
		return phar, nil
	}
	os.MkdirAll(filepath.Join(f.root, "home/blog_ab12/public_html"), 0o755)
	os.WriteFile(filepath.Join(f.root, "home/blog_ab12/public_html/index.html"), []byte(strings.ReplaceAll(placeholderPage, "{{domain}}", "blog.example.com")), 0o644)
	return f
}

func TestWordPressInstall(t *testing.T) {
	f := wpFake(t)
	res, opErr := installWordPress(context.Background(), f.system, wpParams())
	if opErr != nil {
		t.Fatal(opErr)
	}
	_ = res
	joined := strings.Join(f.calls, "\n")
	for _, want := range []string{
		"runuser -u blog_ab12 -- env HOME=/home/blog_ab12/tmp",
		"core download",
		"config create --dbname=wp_blog_ab12 --dbuser=wp_blog_ab12 --dbhost=localhost --locale=bn_BD --prompt=dbpass <<< DbPassword1234567890abcd",
		"config set FS_METHOD direct",
		"core install --url=https://blog.example.com --title=আমার ব্লগ --admin_user=admin",
		"--prompt=admin_password <<< AdminPassword123456789xy",
		"language core install bn_BD --activate",
		"chmod 600 /home/blog_ab12/public_html/wp-config.php",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	// Passwords appear only after "<<<" (stdin), never as arguments.
	for _, c := range f.calls {
		args, _, _ := strings.Cut(c, " <<< ")
		if strings.Contains(args, "DbPassword") || strings.Contains(args, "AdminPassword") {
			t.Fatalf("password on the command line: %s", c)
		}
	}
	if _, err := os.Stat(filepath.Join(f.root, wpCLIPath)); err != nil {
		t.Fatal("WP-CLI not installed")
	}
	if _, err := os.Stat(filepath.Join(f.root, "home/blog_ab12/public_html/index.html")); err == nil {
		t.Fatal("placeholder page must be removed")
	}
}

func TestWordPressRefusesNonEmptyFolder(t *testing.T) {
	f := wpFake(t)
	os.WriteFile(filepath.Join(f.root, "home/blog_ab12/public_html/shop.php"), []byte("<?php"), 0o644)
	if _, opErr := installWordPress(context.Background(), f.system, wpParams()); opErr == nil || opErr.Code != "not_empty" {
		t.Fatalf("want not_empty, got %v", opErr)
	}
	if strings.Contains(strings.Join(f.calls, "\n"), "core download") {
		t.Fatal("must not download into a folder with files")
	}
}

func TestWordPressRejectsBadDownload(t *testing.T) {
	f := wpFake(t)
	f.fetch = func(_ context.Context, url string) ([]byte, error) {
		if strings.HasSuffix(url, ".sha512") {
			return []byte(strings.Repeat("0", 128)), nil
		}
		return []byte("tampered"), nil
	}
	if _, opErr := installWordPress(context.Background(), f.system, wpParams()); opErr == nil || !strings.Contains(opErr.Message, "checksum") {
		t.Fatalf("tampered WP-CLI accepted: %v", opErr)
	}
}

func TestWordPressValidation(t *testing.T) {
	for _, mutate := range []func(*agentproto.WordPressInstallParams){
		func(p *agentproto.WordPressInstallParams) { p.AdminUser = "admin --role=x" },
		func(p *agentproto.WordPressInstallParams) { p.Title = "line1\nline2" },
		func(p *agentproto.WordPressInstallParams) { p.AdminEmail = "not-an-email" },
		func(p *agentproto.WordPressInstallParams) { p.Locale = "bn_BD; rm -rf /" },
		func(p *agentproto.WordPressInstallParams) { p.User = "root" },
		func(p *agentproto.WordPressInstallParams) { p.AdminPassword = "short" },
	} {
		p := wpParams()
		mutate(&p)
		if err := validateWordPress(p); err == nil {
			t.Errorf("accepted %+v", p)
		}
	}
}
