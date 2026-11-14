package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"ekpanel/internal/agentproto"
)

type tarFile struct {
	name, body string
	link       string
	dir        bool
}

func tarOf(files []tarFile) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		switch {
		case f.dir:
			tw.WriteHeader(&tar.Header{Name: f.name + "/", Typeflag: tar.TypeDir, Mode: 0o755})
		case f.link != "":
			tw.WriteHeader(&tar.Header{Name: f.name, Typeflag: tar.TypeSymlink, Linkname: f.link, Mode: 0o777})
		default:
			tw.WriteHeader(&tar.Header{Name: f.name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(f.body))})
			tw.Write([]byte(f.body))
		}
	}
	tw.Close()
	return buf.Bytes()
}

// cpmove builds a small cPanel backup the way pkgacct lays it out.
func cpmove(homeAsTar bool) []byte {
	home := []tarFile{
		{name: "public_html", dir: true},
		{name: "public_html/index.php", body: "<?php echo 'main';"},
		{name: "public_html/wp-config.php", body: "define('DB_NAME', 'shop_wp');\ndefine('UP', '/home/shop/public_html/up');\ndefine('ADDON', '/home/shop/public_html/addon.com/x');\ndefine('OLD', '/home/shop/public_html_old');\ndefine('PRIV', \"/home/shop\");\ndefine('OTHER', '/home/shopper/x');"},
		{name: "public_html/logo.png", body: "PNG\x00/home/shop/public_html"},
		{name: "public_html/addon.com", dir: true},
		{name: "public_html/addon.com/index.html", body: "addon site"},
		{name: "blog", dir: true},
		{name: "blog/index.php", body: "blog"},
		{name: "blog/current", link: "index.php"},
		{name: "blog/escape", link: "/etc/passwd"},
		{name: "private/keys.txt", body: "kept outside the web root"},
		{name: "mail/shop.com/info/cur/1", body: "a mail"},
		{name: "etc/shop.com/passwd", body: "info:x:1:1::/home/shop/mail/shop.com/info:/bin/false\nsales:x:1:1::/x:/bin/false\n"},
		{name: ".cpanel/datastore", body: "cpanel's own"},
	}
	files := []tarFile{
		{name: "cpmove-shop/cp/shop", body: "USER=shop\nDNS=shop.com\n"},
		{name: "cpmove-shop/userdata/main", body: `---
main_domain: shop.com
addon_domains:
  addon.com: addon.shop.com
parked_domains:
  - shop.net
sub_domains:
  - addon.shop.com
  - blog.shop.com
`},
		{name: "cpmove-shop/userdata/shop.com", body: "documentroot: /home/shop/public_html\nphpversion: ea-php81\nserveralias: www.shop.com\n"},
		{name: "cpmove-shop/userdata/shop.com_SSL", body: "documentroot: /home/shop/public_html\n"},
		{name: "cpmove-shop/userdata/addon.shop.com", body: "documentroot: /home/shop/public_html/addon.com\nphpversion: ea-php74\n"},
		{name: "cpmove-shop/userdata/blog.shop.com", body: "documentroot: /home/shop/blog\nphpversion: inherit\n"},
		{name: "cpmove-shop/mysql.sql", body: "GRANT USAGE ON *.* TO 'shop'@'localhost' IDENTIFIED BY PASSWORD '*0123456789ABCDEF0123456789ABCDEF01234567';\n" +
			"GRANT USAGE ON *.* TO 'shop_wpu'@'localhost' IDENTIFIED BY PASSWORD '*AAAABBBBCCCCDDDDEEEEFFFF0000111122223333';\n" +
			"GRANT ALL PRIVILEGES ON `shop\\_wp`.* TO 'shop'@'localhost';\n" +
			"GRANT ALL PRIVILEGES ON `shop\\_wp`.* TO 'shop_wpu'@'localhost';\n"},
		{name: "cpmove-shop/mysql/shop_wp.sql", body: "CREATE TABLE wp_posts (id int);\n"},
		{name: "cpmove-shop/cron/shop", body: "MAILTO=\"me@shop.com\"\n# a comment\n*/5 * * * * php /home/shop/public_html/wp-cron.php\n"},
		{name: "cpmove-shop/dnszones/shop.com.db", body: "$ORIGIN shop.com."},
		{name: "cpmove-shop/va/shop.com", body: "info@shop.com: me@example.org\n"},
	}
	if homeAsTar {
		files = append(files, tarFile{name: "cpmove-shop/homedir.tar", body: string(tarOf(home))})
	} else {
		for _, f := range home {
			f.name = "cpmove-shop/homedir/" + f.name
			files = append(files, f)
		}
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(tarOf(files))
	zw.Close()
	return gz.Bytes()
}

func TestImportCPanel(t *testing.T) {
	for _, asTar := range []bool{true, false} {
		var loaded []string
		s := transferSystem(t, &loaded)
		os.MkdirAll(s.path(agentproto.ImportDir), 0o700)
		os.WriteFile(s.path(agentproto.ImportDir+"/cpmove-shop.tar.gz"), cpmove(asTar), 0o600)

		out, opErr := importInspect(context.Background(), s, agentproto.ImportInspectParams{File: "cpmove-shop.tar.gz"})
		if opErr != nil {
			t.Fatal(opErr)
		}
		plan := out.(agentproto.CPanelPlan)
		if plan.User != "shop" || plan.MainDomain != "shop.com" || len(plan.Domains) != 3 {
			t.Fatalf("plan %+v", plan)
		}
		main, addon, blog := plan.Domains[0], plan.Domains[1], plan.Domains[2]
		if main.DocRoot != "public_html" || main.PHPVersion != "8.1" || !main.HasCert || !slices.Contains(main.Aliases, "shop.net") ||
			addon.Domain != "addon.com" || addon.DocRoot != "public_html/addon.com" || addon.PHPVersion != "7.4" ||
			blog.Domain != "blog.shop.com" || blog.DocRoot != "blog" || blog.PHPVersion != "" {
			t.Fatalf("domains %+v", plan.Domains)
		}
		if len(plan.Databases) != 1 || plan.Databases[0].Name != "shop_wp" || plan.Databases[0].User != "shop_wpu" ||
			plan.Databases[0].PasswordHash != "*AAAABBBBCCCCDDDDEEEEFFFF0000111122223333" {
			t.Fatalf("databases %+v", plan.Databases)
		}
		if len(plan.Cron) != 1 || !strings.Contains(plan.Cron[0], "wp-cron.php") || !slices.Equal(plan.CronSettings, []string{`MAILTO="me@shop.com"`}) {
			t.Fatalf("cron %q settings %q", plan.Cron, plan.CronSettings)
		}
		if len(plan.Mail) != 1 || len(plan.Mail[0].Mailboxes) != 2 || len(plan.Zones) != 1 {
			t.Fatalf("mail %+v zones %+v", plan.Mail, plan.Zones)
		}

		// The panel made the websites first: their homes are there.
		for _, u := range []string{"shop_1", "addon_2", "blog_3"} {
			os.MkdirAll(s.path("/home/"+u+"/public_html"), 0o750)
		}
		writeFile(t, s.path("/home/shop_1/public_html/index.html"), "placeholder")
		res, opErr := importApply(context.Background(), s, agentproto.ImportApplyParams{File: "cpmove-shop.tar.gz",
			Sites:     []agentproto.ImportSite{{DocRoot: "public_html", User: "shop_1"}, {DocRoot: "public_html/addon.com", User: "addon_2"}, {DocRoot: "blog", User: "blog_3"}},
			Databases: []agentproto.TransferDatabase{{Engine: "mariadb", Name: "shop_wp", User: "shop_wpu"}}, MainUser: "shop_1", OldUser: "shop"})
		if opErr != nil {
			t.Fatal(opErr)
		}
		r := res.(agentproto.ImportApplyResult)
		read := func(p string) string { b, _ := os.ReadFile(s.path(p)); return string(b) }
		if read("/home/shop_1/public_html/index.php") != "<?php echo 'main';" || read("/home/addon_2/public_html/index.html") != "addon site" ||
			read("/home/blog_3/public_html/index.php") != "blog" || read("/home/shop_1/private/keys.txt") != "kept outside the web root" {
			t.Fatalf("files not where they belong (as tar: %v)", asTar)
		}
		if _, err := os.Stat(s.path("/home/shop_1/public_html/addon.com")); err == nil {
			t.Fatal("the addon's folder was also put into the main website")
		}
		if _, err := os.Stat(s.path("/home/shop_1/public_html/index.html")); err == nil {
			t.Fatal("the placeholder page is still there")
		}
		if _, err := os.Stat(s.path("/home/shop_1/mail")); err == nil {
			t.Fatal("mail was copied into the website")
		}
		if l, err := os.Readlink(s.path("/home/blog_3/public_html/current")); err != nil || l != "index.php" {
			t.Fatalf("link inside the site: %q %v", l, err)
		}
		if _, err := os.Lstat(s.path("/home/blog_3/public_html/escape")); err == nil {
			t.Fatal("a link to /etc/passwd was created")
		}
		// The old home folder's path follows the files; other text and binary files stay.
		wantConfig := "define('DB_NAME', 'shop_wp');\ndefine('UP', '/home/shop_1/public_html/up');\ndefine('ADDON', '/home/addon_2/public_html/x');\n" +
			"define('OLD', '/home/shop_1/public_html_old');\ndefine('PRIV', \"/home/shop_1\");\ndefine('OTHER', '/home/shopper/x');"
		if got := read("/home/shop_1/public_html/wp-config.php"); got != wantConfig {
			t.Fatalf("wp-config.php:\n%s", got)
		}
		if read("/home/shop_1/public_html/logo.png") != "PNG\x00/home/shop/public_html" || r.PathsChangedCount != 1 ||
			!slices.Equal(r.PathsChanged, []string{"shop_1/public_html/wp-config.php"}) {
			t.Fatalf("paths changed %d %v", r.PathsChangedCount, r.PathsChanged)
		}
		if len(r.Databases) != 1 || len(loaded) != 1 || !strings.Contains(loaded[0], "wp_posts") {
			t.Fatalf("databases %v loaded %q", r.Databases, loaded)
		}
		if slices.ContainsFunc(r.Skipped, func(x agentproto.ImportSkip) bool { return x.Name == "mail" || x.Name == "etc" || x.Name == ".cpanel" }) ||
			!slices.ContainsFunc(r.Skipped, func(x agentproto.ImportSkip) bool { return x.Name == "links_outside" && x.Count == 1 }) {
			t.Fatalf("skipped %v", r.Skipped)
		}
	}
}

func TestImportRefusesOtherFiles(t *testing.T) {
	var loaded []string
	s := transferSystem(t, &loaded)
	for _, name := range []string{"../etc/passwd", "/etc/shadow", "x.zip", ".hidden.tar.gz"} {
		if _, opErr := importInspect(context.Background(), s, agentproto.ImportInspectParams{File: name}); opErr == nil {
			t.Errorf("accepted %q", name)
		}
	}
	os.MkdirAll(s.path(agentproto.ImportDir), 0o700)
	if _, opErr := importInspect(context.Background(), s, agentproto.ImportInspectParams{File: "missing.tar.gz"}); opErr == nil || opErr.Code != agentproto.ErrNoImportFile {
		t.Fatalf("a missing backup: %v", opErr)
	}
	os.WriteFile(filepath.Join(s.path(agentproto.ImportDir), "not-cpanel.tar.gz"), func() []byte {
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		zw.Write(tarOf([]tarFile{{name: "x/readme", body: "hi"}}))
		zw.Close()
		return gz.Bytes()
	}(), 0o600)
	if _, opErr := importInspect(context.Background(), s, agentproto.ImportInspectParams{File: "not-cpanel.tar.gz"}); opErr == nil || opErr.Code != agentproto.ErrNotCPanel || !strings.Contains(opErr.Message, "cPanel") {
		t.Fatalf("not a cPanel backup: %v", opErr)
	}
}

// A chain of links that looks inside the website (each checked alone) must not let a later
// entry write outside it: the agent writes as root.
func TestImportLinksCannotEscape(t *testing.T) {
	var loaded []string
	s := transferSystem(t, &loaded)
	home := []tarFile{
		{name: "public_html/a", dir: true},
		{name: "public_html/a/up", link: ".."},                // → public_html
		{name: "public_html/out", link: "a/up/../.."},         // looks like public_html, is /home
		{name: "public_html/out/victim/pwned.txt", body: "x"}, // would be /home/victim/pwned.txt
		{name: "public_html/a/up/ok.txt", body: "inside"},     // through a link that stays inside
	}
	files := []tarFile{
		{name: "cpmove-shop/cp/shop", body: "USER=shop\nDNS=shop.com\n"},
		{name: "cpmove-shop/userdata/main", body: "---\nmain_domain: shop.com\n"},
		{name: "cpmove-shop/userdata/shop.com", body: "documentroot: /home/shop/public_html\n"},
		{name: "cpmove-shop/homedir.tar", body: string(tarOf(home))},
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(tarOf(files))
	zw.Close()
	os.MkdirAll(s.path(agentproto.ImportDir), 0o700)
	os.WriteFile(s.path(agentproto.ImportDir+"/cpmove-shop.tar.gz"), gz.Bytes(), 0o600)
	os.MkdirAll(s.path("/home/victim"), 0o750)
	os.MkdirAll(s.path("/home/shop_1/public_html"), 0o750)
	res, opErr := importApply(context.Background(), s, agentproto.ImportApplyParams{File: "cpmove-shop.tar.gz",
		Sites: []agentproto.ImportSite{{DocRoot: "public_html", User: "shop_1"}}, MainUser: "shop_1", OldUser: "shop"})
	if opErr != nil {
		t.Fatal(opErr)
	}
	if sk := res.(agentproto.ImportApplyResult).Skipped; !slices.ContainsFunc(sk, func(x agentproto.ImportSkip) bool { return x.Name == "links_outside" }) {
		t.Fatalf("skipped %+v", sk)
	}
	if _, err := os.Stat(s.path("/home/victim/pwned.txt")); err == nil {
		t.Fatal("a file was written outside the website through a chain of links")
	}
	if b, _ := os.ReadFile(s.path("/home/shop_1/public_html/ok.txt")); string(b) != "inside" {
		t.Fatalf("a file through a link inside the website: %q", b)
	}
}
