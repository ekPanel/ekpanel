package agent

import (
	"context"
	"strings"
	"testing"

	"ekpanel/internal/agentproto"
)

func TestPHPAppSecretsStayOutOfArguments(t *testing.T) {
	f := newFakeSystem(t)
	var got []command
	f.system.exec = func(_ context.Context, c command) ([]byte, error) {
		got = append(got, c)
		return nil, nil
	}
	s := &phpAppSite{sys: f.system, home: "/home/blog_ab12", docroot: "/home/blog_ab12/public_html",
		p: agentproto.PHPAppInstallParams{User: "blog_ab12", PHPVersion: "8.4"}}
	if _, err := s.php(context.Background(), s.docroot, []string{"--admin-password=Secret123456789012"}, []string{"EKPANEL_ADMIN_PASSWORD=Other12345678901234"},
		"installation/joomla.php", "install", "--site-name=Blog"); err != nil {
		t.Fatal(err)
	}
	c := got[0]
	args := strings.Join(c.args, " ")
	if strings.Contains(args, "Secret") || strings.Contains(args, "Other") {
		t.Fatalf("a password in the arguments: %s", args)
	}
	if !strings.HasPrefix(args, "-m -u blog_ab12 -- php8.4 ") || !strings.Contains(args, "/home/blog_ab12/tmp/ekpanel-run.php installation/joomla.php install --site-name=Blog") {
		t.Errorf("arguments: %s", args)
	}
	env := strings.Join(c.env, "\n")
	if !strings.Contains(env, `EKPANEL_SECRET_ARGS=["--admin-password=Secret123456789012"]`) || !strings.Contains(env, "EKPANEL_ADMIN_PASSWORD=Other") ||
		!strings.Contains(env, "HOME=/home/blog_ab12/tmp") || !strings.HasSuffix(c.dir, "/home/blog_ab12/public_html") {
		t.Errorf("environment %q, folder %q", env, c.dir)
	}
}

func TestPHPAppParams(t *testing.T) {
	good := agentproto.PHPAppInstallParams{App: "joomla", User: "blog_ab12", Domain: "blog.example.com", PHPVersion: "8.4",
		DB: agentproto.CloneDB{Name: "jo_blog_ab12", User: "jo_blog_ab12", Password: "DbPassword1234567890"}, Title: "Blog",
		AdminUser: "owner", AdminPassword: "AdminPassword1234567890", AdminEmail: "a@example.com"}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(p *agentproto.PHPAppInstallParams){
		"unknown app":  func(p *agentproto.PHPAppInstallParams) { p.App = "prestashop" },
		"old PHP":      func(p *agentproto.PHPAppInstallParams) { p.PHPVersion = "8.2" },
		"bad admin":    func(p *agentproto.PHPAppInstallParams) { p.AdminUser = "a b" },
		"bad password": func(p *agentproto.PHPAppInstallParams) { p.AdminPassword = "short' or 1=1" },
		"bad title":    func(p *agentproto.PHPAppInstallParams) { p.Title = "two\nlines" },
	} {
		p := good
		change(&p)
		if p.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	lv := good
	lv.App, lv.AdminUser, lv.AdminPassword, lv.AdminEmail = "laravel", "", "", ""
	if err := lv.Validate(); err != nil {
		t.Errorf("Laravel needs no admin: %v", err)
	}
}
