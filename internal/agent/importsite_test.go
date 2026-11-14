package agent

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"ekpanel/internal/agentproto"
)

func zipOf(files []tarFile) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		if f.dir {
			zw.Create(f.name + "/")
			continue
		}
		w, _ := zw.Create(f.name)
		w.Write([]byte(f.body))
	}
	zw.Close()
	return buf.Bytes()
}

func gzOf(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

const testWPConfig = `<?php
define( 'DB_NAME', 'olddb' );
define( 'DB_USER', "olduser" );
define( 'DB_PASSWORD', 'p\'w"d' );
define( 'DB_HOST', 'mysql.oldhost.example' );
$table_prefix = 'wpx_';
require_once ABSPATH . 'wp-settings.php';
`

func TestImportSiteWordPressZip(t *testing.T) {
	var loaded []string
	s := transferSystem(t, &loaded)
	os.MkdirAll(s.path(agentproto.ImportDir), 0o700)
	os.WriteFile(s.path(agentproto.ImportDir+"/blog.example.zip"), zipOf([]tarFile{
		{name: "backup/public_html", dir: true},
		{name: "backup/public_html/wp-config.php", body: testWPConfig},
		{name: "backup/public_html/index.php", body: "<?php require 'wp-blog-header.php';"},
		{name: "backup/public_html/wp-includes/version.php", body: "<?php $wp_version = '6.8';"},
		{name: "backup/public_html/db.sql", body: "CREATE DATABASE /*!32312 IF NOT EXISTS*/ `olddb`;\nUSE `olddb`;\nCREATE TABLE wpx_posts (id int);\nINSERT INTO wpx_posts VALUES (1); -- USE nothing\n"},
		{name: "backup/public_html/wp-content/old.sql.gz", body: "an older dump, not chosen"},
		{name: "backup/notes.txt", body: "outside the website"},
	}), 0o600)
	os.WriteFile(s.path(agentproto.ImportDir+"/readme.md"), []byte("not for import"), 0o600)

	out, opErr := importList(context.Background(), s, struct{}{})
	if opErr != nil || len(out.(agentproto.ImportListResult).Files) != 1 || out.(agentproto.ImportListResult).Files[0].Kind != agentproto.ImportKindZip {
		t.Fatalf("list %+v %v", out, opErr)
	}
	out, opErr = importSiteInspect(context.Background(), s, agentproto.ImportSiteInspectParams{Archive: "blog.example.zip"})
	if opErr != nil {
		t.Fatal(opErr)
	}
	plan := out.(agentproto.SitePlan)
	wp := plan.WordPress
	if plan.Root != "backup/public_html" || plan.Kind != "wordpress" || plan.Files != 5 || wp == nil ||
		wp.DBName != "olddb" || wp.DBUser != "olduser" || wp.DBHost != "mysql.oldhost.example" || wp.TablePrefix != "wpx_" || plan.Domain != "blog.example" ||
		wp.PasswordHash != mysqlNativeHash(`p'w"d`) || len(plan.Dumps) != 2 || plan.Dumps[0].Name != "backup/public_html/db.sql" {
		t.Fatalf("plan %+v wp %+v", plan, wp)
	}

	os.MkdirAll(s.path("/home/blog_1/public_html"), 0o750)
	writeFile(t, s.path("/home/blog_1/public_html/index.html"), "placeholder")
	res, opErr := importSiteApply(context.Background(), s, agentproto.ImportSiteApplyParams{Archive: "blog.example.zip", Root: plan.Root, User: "blog_1",
		Database: &agentproto.TransferDatabase{Engine: "mariadb", Name: "blog_wp", User: "blog_wp"}, DumpInArchive: "backup/public_html/db.sql", WordPress: true})
	if opErr != nil {
		t.Fatal(opErr)
	}
	r := res.(agentproto.ImportApplyResult)
	read := func(p string) string { b, _ := os.ReadFile(s.path(p)); return string(b) }
	cfg := read("/home/blog_1/public_html/wp-config.php")
	for _, want := range []string{"define( 'DB_NAME', 'blog_wp' )", "define( 'DB_USER', 'blog_wp' )", "define( 'DB_HOST', 'localhost' )", `define( 'DB_PASSWORD', 'p\'w"d' )`, "$table_prefix = 'wpx_';"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("wp-config.php lacks %q:\n%s", want, cfg)
		}
	}
	for _, f := range []string{"db.sql", "wp-content/old.sql.gz"} {
		if _, err := os.Stat(s.path("/home/blog_1/public_html/" + f)); err == nil {
			t.Fatalf("the dump %s was published in the website", f)
		}
	}
	if len(loaded) == 1 && (strings.Contains(loaded[0], "USE `olddb`") || strings.Contains(loaded[0], "CREATE DATABASE") || !strings.Contains(loaded[0], "-- USE nothing")) {
		t.Fatalf("the dump still switches databases, or lost a line:\n%s", loaded[0])
	}
	if _, err := os.Stat(s.path("/home/blog_1/public_html/index.html")); err == nil {
		t.Fatal("the placeholder page is still there")
	}
	if read("/home/blog_1/public_html/wp-includes/version.php") == "" || r.Files != 3 ||
		len(loaded) != 1 || !strings.Contains(loaded[0], "wpx_posts") || !slices.Equal(r.Databases, []string{"blog_wp"}) ||
		!slices.ContainsFunc(r.Skipped, func(x agentproto.ImportSkip) bool { return x.Name == "outside_website" && x.Count == 1 }) ||
		!slices.ContainsFunc(r.Skipped, func(x agentproto.ImportSkip) bool { return x.Name == "dumps" && x.Count == 1 }) {
		t.Fatalf("result %+v loaded %q", r, loaded)
	}
}

func TestImportSiteAaPanel(t *testing.T) {
	var loaded []string
	s := transferSystem(t, &loaded)
	os.MkdirAll(s.path(agentproto.ImportDir), 0o700)
	archive := "web_shop.example.com_20250101_120000.tar.gz"
	os.WriteFile(s.path(agentproto.ImportDir+"/"+archive), gzOf(tarOf([]tarFile{
		{name: "shop.example.com", dir: true},
		{name: "shop.example.com/index.html", body: "<h1>shop</h1>"},
		{name: "shop.example.com/.user.ini", body: "open_basedir=/www/wwwroot/shop.example.com/"},
		{name: "shop.example.com/up", link: "index.html"},
	})), 0o600)
	os.WriteFile(s.path(agentproto.ImportDir+"/db_shop_20250101_120000.sql.gz"), gzOf([]byte("CREATE TABLE orders (id int);")), 0o600)

	out, opErr := importSiteInspect(context.Background(), s, agentproto.ImportSiteInspectParams{Archive: archive})
	if opErr != nil {
		t.Fatal(opErr)
	}
	plan := out.(agentproto.SitePlan)
	if plan.Root != "shop.example.com" || plan.Kind != "static" || plan.Domain != "shop.example.com" || plan.WordPress != nil || len(plan.Dumps) != 0 {
		t.Fatalf("plan %+v", plan)
	}
	os.MkdirAll(s.path("/home/shop_1/public_html"), 0o750)
	res, opErr := importSiteApply(context.Background(), s, agentproto.ImportSiteApplyParams{Archive: archive, Root: plan.Root, User: "shop_1",
		Database: &agentproto.TransferDatabase{Engine: "mariadb", Name: "shop", User: "shop"}, Dump: "db_shop_20250101_120000.sql.gz"})
	if opErr != nil {
		t.Fatal(opErr)
	}
	r := res.(agentproto.ImportApplyResult)
	if b, _ := os.ReadFile(s.path("/home/shop_1/public_html/index.html")); string(b) != "<h1>shop</h1>" {
		t.Fatalf("index.html %q", b)
	}
	if l, err := os.Readlink(s.path("/home/shop_1/public_html/up")); err != nil || l != "index.html" {
		t.Fatalf("link %q %v", l, err)
	}
	if b, _ := os.ReadFile(s.path("/home/shop_1/public_html/.user.ini")); strings.Contains(string(b), "open_basedir") ||
		!slices.ContainsFunc(r.Skipped, func(x agentproto.ImportSkip) bool { return x.Name == "open_basedir" }) {
		t.Fatalf(".user.ini %q skipped %+v", b, r.Skipped)
	}
	if len(loaded) != 1 || !strings.Contains(loaded[0], "orders") || r.Files != 2 {
		t.Fatalf("result %+v loaded %q", r, loaded)
	}

	// Missing files and other kinds are refused with their own codes.
	if _, opErr := importSiteInspect(context.Background(), s, agentproto.ImportSiteInspectParams{Archive: "nope.zip"}); opErr == nil || opErr.Code != agentproto.ErrNoImportFile {
		t.Fatalf("missing archive: %v", opErr)
	}
	if _, opErr := importSiteInspect(context.Background(), s, agentproto.ImportSiteInspectParams{Archive: "db_shop_20250101_120000.sql.gz"}); opErr == nil || opErr.Code != agentproto.ErrInvalidParams {
		t.Fatalf("a dump as the archive: %v", opErr)
	}
}

func TestWithoutDatabaseSwitch(t *testing.T) {
	long := strings.Repeat("x", 200<<10) // longer than the read buffer
	in := "USE `a`;\nINSERT INTO t VALUES ('" + long + "USE `b`;');\ncreate database c;\nSELECT 1;"
	r := withoutDatabaseSwitch(strings.NewReader(in))
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil || string(b) != "INSERT INTO t VALUES ('"+long+"USE `b`;');\nSELECT 1;" {
		t.Fatalf("%v: %d bytes, starts %q", err, len(b), string(b[:min(len(b), 40)]))
	}
}
