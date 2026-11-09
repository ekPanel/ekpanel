package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ekpanel/internal/agentproto"
)

var blogSite = agentproto.WordPressSite{User: "blog_ab12", Domain: "blog.example.com", PHPVersion: "8.4"}

// wpToolkitFake answers WP-CLI like a WordPress with one outdated plugin (akismet) and an
// update of the 6.7 branch. broken makes the site show an error after the plugin update.
func wpToolkitFake(t *testing.T, broken bool) *fakeSystem {
	f := wpFake(t)
	docroot := filepath.Join(f.root, "home/blog_ab12/public_html")
	os.MkdirAll(filepath.Join(docroot, "wp-includes"), 0o755)
	os.WriteFile(filepath.Join(docroot, "wp-includes/version.php"), []byte("<?php"), 0o644)
	os.WriteFile(filepath.Join(docroot, "wp-config.php"), []byte(`<?php
define( 'DB_NAME', 'wp_live' );
define('DB_USER', "wp_live");
define( 'DB_PASSWORD', 'old-secret' );
define( 'DB_HOST', 'localhost' );
`), 0o644)
	updated := false
	run := f.system.run
	f.system.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		if name != "runuser" || !strings.Contains(call, "wp-cli.phar") {
			return run(ctx, name, args...)
		}
		f.calls = append(f.calls, name+" "+call)
		_, cmd, _ := strings.Cut(call, "--skip-themes ")
		if !strings.Contains(call, "--skip-themes") {
			_, cmd, _ = strings.Cut(call, "public_html ")
		}
		switch {
		case cmd == "core version":
			return []byte("6.7.1"), nil
		case cmd == "core check-update --format=json":
			return []byte(`[{"version":"6.8","update_type":"major"},{"version":"6.7.2","update_type":"minor"}]`), nil
		case cmd == "plugin list --format=json --fields=name,title,status,version,update_version":
			if updated {
				return []byte(`[{"name":"akismet","title":"Akismet","status":"active","version":"5.4","update_version":""}]`), nil
			}
			return []byte(`[{"name":"akismet","title":"Akismet","status":"active","version":"5.3","update_version":"5.4"},{"name":"hello","title":"Hello","status":"inactive","version":"1.7","update_version":""}]`), nil
		case cmd == "theme list --format=json --fields=name,title,status,version,update_version":
			return []byte(`[]`), nil
		case cmd == "plugin path akismet":
			return []byte("/home/blog_ab12/public_html/wp-content/plugins/akismet/akismet.php"), nil
		case cmd == "plugin update akismet":
			updated = true
			return nil, nil
		case cmd == "config get DISALLOW_FILE_EDIT --type=constant":
			return []byte("1"), nil
		case strings.HasPrefix(cmd, "maintenance-mode is-active"), strings.HasPrefix(cmd, "config get"):
			return nil, os.ErrNotExist
		}
		return nil, nil
	}
	f.system.probe = func(context.Context, string) (int, []byte, error) {
		if broken && updated {
			return 500, []byte("There has been a critical error on this website."), nil
		}
		return 200, []byte("<html>blog</html>"), nil
	}
	return f
}

func TestWordPressInfo(t *testing.T) {
	f := wpToolkitFake(t, false)
	info, opErr := wordPressInfo(context.Background(), f.system, agentproto.WordPressInfoParams{Site: blogSite})
	if opErr != nil {
		t.Fatal(opErr)
	}
	if !info.Installed || info.Version != "6.7.1" || info.CoreUpdate != "6.8" || info.CoreMinor != "6.7.2" || !info.FileEditDisabled || info.Maintenance {
		t.Errorf("info: %+v", info)
	}
	if len(info.Plugins) != 2 || info.Plugins[0].Update != "5.4" || info.LoosePermissions != 1 { // wp-config.php is 644
		t.Errorf("plugins %+v, loose %d", info.Plugins, info.LoosePermissions)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "wp-cli.phar") && !strings.Contains(c, "runuser -u blog_ab12 -- ") {
			t.Errorf("WP-CLI not run as the site's account: %s", c)
		}
	}
	other := blogSite
	other.User = "root"
	if _, opErr := wordPressInfo(context.Background(), f.system, agentproto.WordPressInfoParams{Site: other}); opErr == nil {
		t.Error("an account outside ekPanel's sites must be refused")
	}
}

func TestWordPressUpdateRollsBack(t *testing.T) {
	for _, broken := range []bool{false, true} {
		f := wpToolkitFake(t, broken)
		res, opErr := wordPressUpdate(context.Background(), f.system, agentproto.WordPressUpdateParams{Site: blogSite, Plugins: []string{"*"}})
		if opErr != nil {
			t.Fatal(opErr)
		}
		if len(res.Items) != 1 || res.Items[0].Name != "akismet" {
			t.Fatalf("items: %+v", res.Items)
		}
		want := "updated"
		if broken {
			want = "rolled_back"
		}
		if res.Items[0].Outcome != want {
			t.Errorf("broken=%v: outcome %+v", broken, res.Items[0])
		}
		joined := strings.Join(f.calls, "\n")
		copyCall := "runuser -u blog_ab12 -- cp -a -- /home/blog_ab12/public_html/wp-content/plugins/akismet /home/blog_ab12/tmp/.ekpanel-rollback/plugin-akismet"
		restore := "runuser -u blog_ab12 -- mv -- /home/blog_ab12/tmp/.ekpanel-rollback/plugin-akismet /home/blog_ab12/public_html/wp-content/plugins/akismet"
		if !strings.Contains(joined, copyCall) || strings.Contains(joined, restore) != broken {
			t.Errorf("broken=%v: copy and restore:\n%s", broken, joined)
		}
	}
}

func TestWordPressParamsRefused(t *testing.T) {
	bad := []error{
		agentproto.WordPressUpdateParams{Site: blogSite, Plugins: []string{"../../etc"}}.Validate(),
		agentproto.WordPressUpdateParams{Site: blogSite, Core: "nightly"}.Validate(),
		agentproto.WordPressManageParams{Site: blogSite, Action: "shell"}.Validate(),
		agentproto.WordPressManageParams{Site: blogSite, Action: "plugin_activate", Name: "-rf"}.Validate(),
		agentproto.WordPressCloneParams{From: blogSite, To: blogSite, Files: true}.Validate(),
		agentproto.WordPressCloneParams{From: blogSite, To: agentproto.WordPressSite{User: "stage_x1", Domain: "staging.blog.example.com", PHPVersion: "8.4"}}.Validate(),
		agentproto.WordPressCloneParams{From: blogSite, To: agentproto.WordPressSite{User: "stage_x1", Domain: "staging.blog.example.com", PHPVersion: "8.4"},
			Files: true, ToDB: &agentproto.CloneDB{Name: "wp_x", User: "wp_x", Password: "short'"}}.Validate(),
	}
	for i, err := range bad {
		if err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestStagingConfig(t *testing.T) {
	f := wpToolkitFake(t, false)
	w := &wpSite{sys: f.system, site: blogSite, home: "/home/blog_ab12", docroot: "/home/blog_ab12/public_html"}
	if err := w.setDBConfig(agentproto.CloneDB{Name: "wp_stage", User: "wp_stage", Password: "NewPassword1234567890"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(f.root, "home/blog_ab12/public_html/wp-config.php"))
	for _, want := range []string{"define( 'DB_NAME', 'wp_stage' )", "define( 'DB_USER', 'wp_stage' )", "define( 'DB_PASSWORD', 'NewPassword1234567890' )", "'DB_HOST', 'localhost'"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("wp-config.php lacks %q:\n%s", want, b)
		}
	}
	if info, _ := os.Stat(filepath.Join(f.root, "home/blog_ab12/public_html/wp-config.php")); info.Mode().Perm() != 0o600 {
		t.Errorf("wp-config.php mode %v", info.Mode().Perm())
	}
}
