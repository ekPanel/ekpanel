package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"ekpanel/internal/agentproto"
)

// fakeSystem simulates the commands provisioning runs, keeping Linux accounts in memory.
type fakeSystem struct {
	*system
	calls     []string
	users     map[string][]string // user -> groups
	installed map[string]bool     // dpkg packages
	badCaddy  bool                // make `caddy validate` fail
}

func newFakeSystem(t *testing.T) *fakeSystem {
	f := &fakeSystem{users: map[string][]string{}, installed: map[string]bool{"php8.4-fpm": true}}
	f.system = &system{
		root:  t.TempDir(),
		osID:  func() (string, string) { return "ubuntu", "noble" },
		fetch: func(context.Context, string) ([]byte, error) { return []byte("key"), nil },
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			call := name + " " + strings.Join(args, " ")
			f.calls = append(f.calls, call)
			switch name {
			case "getent":
				return nil, errors.New("no such group")
			case "id":
				groups, ok := f.users[args[len(args)-1]]
				if !ok {
					return nil, errors.New("no such user")
				}
				return []byte(strings.Join(groups, " ")), nil
			case "useradd":
				user := args[len(args)-1]
				f.users[user] = []string{user, sitesGroup}
				os.MkdirAll(filepath.Join(f.root, "home", user), 0o755)
			case "userdel":
				delete(f.users, args[len(args)-1])
			case "dpkg-query":
				if f.installed[args[len(args)-1]] {
					return []byte("install ok installed"), nil
				}
				return nil, errors.New("not installed")
			case "apt-get":
				for _, a := range args {
					if strings.HasSuffix(a, "-fpm") {
						f.installed[a] = true
					}
				}
			case "runuser":
				if slices.Contains(args, "validate") && f.badCaddy {
					return nil, errors.New("Caddyfile:3 - unrecognized directive")
				}
			}
			return nil, nil
		},
	}
	return f
}

func (f *fakeSystem) read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(f.path(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func (f *fakeSystem) exists(path string) bool {
	_, err := os.Stat(f.path(path))
	return err == nil
}

func (f *fakeSystem) ran(prefix string) bool {
	return slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

const siteID = "11111111-2222-3333-4444-555555555555"

func phpSite() agentproto.SiteSpec {
	return agentproto.SiteSpec{ID: siteID, Generation: 1, Domain: "example.com", Aliases: []string{"www.example.com"},
		Kind: "php", PHPVersion: "8.4", TLS: "auto", User: "example_ab12"}
}

func apply(t *testing.T, f *fakeSystem, sites ...agentproto.SiteSpec) []agentproto.SiteResult {
	t.Helper()
	f.calls = nil
	res, opErr := stateApply(context.Background(), f.system, agentproto.StateApplyParams{Sites: sites})
	if opErr != nil {
		t.Fatal(opErr)
	}
	return res.Sites
}

func TestCreatePHPSite(t *testing.T) {
	f := newFakeSystem(t)
	res := apply(t, f, phpSite())
	if !res[0].OK {
		t.Fatalf("apply failed: %s", res[0].Error)
	}
	if !f.ran("useradd --create-home --home-dir /home/example_ab12 --shell /usr/sbin/nologin --user-group --groups ekpanel-sites") {
		t.Fatalf("site account not created; calls: %v", f.calls)
	}
	caddy := f.read(t, caddySitePath(siteID))
	for _, want := range []string{"example.com, www.example.com {", "root * /home/example_ab12/public_html",
		"php_fastcgi unix//run/php/ekpanel-" + siteID + "-php8.4.sock", "respond @hidden 404"} {
		if !strings.Contains(caddy, want) {
			t.Errorf("caddy config lacks %q:\n%s", want, caddy)
		}
	}
	if strings.Contains(caddy, "tls internal") {
		t.Error("auto TLS site must not use the internal CA")
	}
	pool := f.read(t, poolPath("8.4", siteID))
	for _, want := range []string{"user = example_ab12", "open_basedir] = /home/example_ab12/", "listen.owner = caddy"} {
		if !strings.Contains(pool, want) {
			t.Errorf("pool lacks %q", want)
		}
	}
	if !strings.Contains(f.read(t, "/home/example_ab12/public_html/index.html"), "example.com is ready") {
		t.Error("placeholder page missing")
	}
	if !f.ran("setfacl -R -m u:caddy:rX,d:u:caddy:rX /home/example_ab12/public_html") {
		t.Error("web server read access (ACL) not granted")
	}
	if !f.ran("systemctl reload-or-restart php8.4-fpm") || !f.ran("caddy reload") {
		t.Errorf("services not reloaded: %v", f.calls)
	}

	// Applying the same state again changes nothing and reloads nothing.
	if res := apply(t, f, phpSite()); !res[0].OK || f.ran("caddy reload") || f.ran("systemctl reload") || f.ran("useradd") {
		t.Fatalf("second apply was not a no-op: %v", f.calls)
	}
}

func TestInstallMissingPHPVersion(t *testing.T) {
	f := newFakeSystem(t)
	sp := phpSite()
	sp.PHPVersion = "7.4"
	if res := apply(t, f, sp); !res[0].OK {
		t.Fatal(res[0].Error)
	}
	if !f.ran("add-apt-repository -y ppa:ondrej/php") || !f.ran("apt-get -o DPkg::Lock::Timeout=300 install -y --no-install-recommends php7.4-fpm") {
		t.Fatalf("PHP 7.4 not installed: %v", f.calls)
	}
	if !strings.Contains(f.read(t, poolPath("7.4", siteID)), "listen = /run/php/ekpanel-"+siteID+"-php7.4.sock") {
		t.Fatal("each PHP version must use its own socket")
	}
}

func TestVersionSwitchReloadOrder(t *testing.T) {
	f := newFakeSystem(t)
	f.installed["php8.3-fpm"] = true
	apply(t, f, phpSite())
	sp := phpSite()
	sp.PHPVersion, sp.Generation = "8.3", 2
	apply(t, f, sp)
	order := func(prefix string) int {
		return slices.IndexFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
	}
	gained, caddy, lost := order("systemctl reload-or-restart php8.3-fpm"), order("caddy reload"), order("systemctl reload-or-restart php8.4-fpm")
	if gained < 0 || caddy < 0 || lost < 0 || !(gained < caddy && caddy < lost) {
		t.Fatalf("want new PHP, then Caddy, then old PHP; calls: %v", f.calls)
	}
}

func TestSwitchToStaticAndDelete(t *testing.T) {
	f := newFakeSystem(t)
	apply(t, f, phpSite())

	static := phpSite()
	static.Kind, static.PHPVersion, static.TLS, static.Generation = "static", "", "internal", 2
	if res := apply(t, f, static); !res[0].OK {
		t.Fatal(res[0].Error)
	}
	if f.exists(poolPath("8.4", siteID)) {
		t.Error("PHP pool must be removed when a site becomes static")
	}
	caddy := f.read(t, caddySitePath(siteID))
	if strings.Contains(caddy, "php_fastcgi") || !strings.Contains(caddy, "tls internal") {
		t.Errorf("static/internal config wrong:\n%s", caddy)
	}

	gone := static
	gone.Deleted, gone.Generation = true, 3
	if res := apply(t, f, gone); !res[0].OK {
		t.Fatal(res[0].Error)
	}
	if f.exists(caddySitePath(siteID)) || !f.ran("userdel --remove example_ab12") {
		t.Fatalf("site not removed: %v", f.calls)
	}
}

func TestRefusesForeignAndReservedAccounts(t *testing.T) {
	f := newFakeSystem(t)
	f.users["alice"] = []string{"alice", "sudo"} // a real person's account
	sp := phpSite()
	sp.User = "alice"
	if res := apply(t, f, sp); res[0].OK || !strings.Contains(res[0].Error, "not created by ekPanel") {
		t.Fatalf("foreign account accepted: %+v", res[0])
	}
	del := sp
	del.Deleted = true
	apply(t, f, del)
	if f.ran("userdel") || f.ran("pkill") {
		t.Fatal("must never delete an account ekPanel did not create")
	}
	sp.User = "root"
	if res := apply(t, f, sp); res[0].OK {
		t.Fatal("reserved account accepted")
	}
}

func TestRejectsConfigInjection(t *testing.T) {
	f := newFakeSystem(t)
	for _, host := range []string{"evil.com {\n\trespond 200\n}\nx.com", "a b.com", "../etc", "EXAMPLE.COM", "localhost"} {
		sp := phpSite()
		sp.Domain = host
		if res := apply(t, f, sp); res[0].OK {
			t.Errorf("host %q accepted", host)
		}
	}
	sp := phpSite()
	sp.PHPVersion = "8.4; rm -rf /"
	if res := apply(t, f, sp); res[0].OK {
		t.Error("bad PHP version accepted")
	}
	if f.exists(caddySitePath(siteID)) {
		t.Error("an invalid site must not write any configuration")
	}
}

func TestCaddyRejectionRollsBack(t *testing.T) {
	f := newFakeSystem(t)
	f.badCaddy = true
	res := apply(t, f, phpSite())
	if res[0].OK || !strings.Contains(res[0].Error, "web server rejected") {
		t.Fatalf("want rejection, got %+v", res[0])
	}
	if f.exists(caddySitePath(siteID)) {
		t.Fatal("a rejected configuration must be rolled back")
	}
	if f.ran("caddy reload") {
		t.Fatal("must not reload after a failed validation")
	}
}

func TestOrphanConfigIsRemovedButDataKept(t *testing.T) {
	f := newFakeSystem(t)
	apply(t, f, phpSite())
	apply(t, f) // the panel no longer lists the site
	if f.exists(caddySitePath(siteID)) || f.exists(poolPath("8.4", siteID)) {
		t.Fatal("orphan configuration must be removed")
	}
	if f.ran("userdel") || !f.exists("/home/example_ab12/public_html/index.html") {
		t.Fatal("an orphan's files and account must be kept")
	}
}
