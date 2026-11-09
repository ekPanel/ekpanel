package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ekpanel/internal/agentproto"
)

func TestApplyFTP(t *testing.T) {
	f := newFakeSystem(t)
	f.installed["pure-ftpd"] = true
	f.users["blog_ab12"] = []string{"blog_ab12", sitesGroup}
	run := f.system.run
	f.system.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "id" && (args[0] == "-u" || args[0] == "-g") {
			f.calls = append(f.calls, name+" "+strings.Join(args, " "))
			return []byte("1003\n"), nil
		}
		if name == "openssl" {
			for i, a := range args {
				if a == "-keyout" || a == "-out" {
					os.MkdirAll(filepath.Dir(filepath.Join(f.root, args[i+1])), 0o755)
					os.WriteFile(filepath.Join(f.root, args[i+1]), []byte(a+"\n"), 0o600)
				}
			}
		}
		return run(ctx, name, args...)
	}
	hash := "$6$saltsalt$" + strings.Repeat("a", 86)
	a := &applier{sys: f.system, results: map[string]*agentproto.SiteResult{}, caddyChanged: map[string][]byte{}}
	sites := []agentproto.SiteSpec{
		{ID: "s1", User: "blog_ab12", FTPEnabled: true, FTPPasswordHash: hash},
		{ID: "s2", User: "shop_cd34", FTPEnabled: false},
	}
	for _, sp := range sites {
		a.results[sp.ID] = &agentproto.SiteResult{ID: sp.ID, OK: true}
	}
	a.applyFTP(context.Background(), sites, "203.0.113.5")
	if !a.results["s1"].OK {
		t.Fatalf("FTP failed: %s", a.results["s1"].Error)
	}
	b, _ := os.ReadFile(filepath.Join(f.root, ftpPasswd))
	if string(b) != "blog_ab12:"+hash+":1003:1003::/home/blog_ab12/./::::::::::::\n" {
		t.Errorf("users file: %q", b)
	}
	for file, want := range map[string]string{"TLS": "2\n", "UnixAuthentication": "no\n", "PAMAuthentication": "no\n", "ForcePassiveIP": "203.0.113.5\n"} {
		if got, _ := os.ReadFile(filepath.Join(f.root, ftpConfDir, file)); string(got) != want {
			t.Errorf("%s: %q", file, got)
		}
	}
	if target, _ := os.Readlink(filepath.Join(f.root, ftpAuthDir, "50puredb")); target != "../conf/PureDB" {
		t.Errorf("auth link: %q", target)
	}
	if cert, _ := os.ReadFile(filepath.Join(f.root, ftpCert)); string(cert) != "-keyout\n-out\n" {
		t.Errorf("certificate file: %q", cert)
	}
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, "pure-pw mkdb "+ftpPDB) || !strings.Contains(joined, "systemctl restart pure-ftpd") {
		t.Errorf("calls:\n%s", joined)
	}

	// The last site without FTP: no users, Pure-FTPd stopped.
	f.calls = nil
	sites[0].FTPEnabled = false
	a.applyFTP(context.Background(), sites, "")
	if b, _ := os.ReadFile(filepath.Join(f.root, ftpPasswd)); len(b) != 0 || !strings.Contains(strings.Join(f.calls, "\n"), "systemctl stop pure-ftpd") {
		t.Errorf("FTP off: %q\n%s", b, strings.Join(f.calls, "\n"))
	}
}
