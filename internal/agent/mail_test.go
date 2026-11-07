package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ekpanel/internal/agentproto"
)

func TestSieveScript(t *testing.T) {
	if s := sieveScript(agentproto.MailboxSpec{KeepCopy: true}); s != "" {
		t.Errorf("nothing to do needs no script: %q", s)
	}
	s := sieveScript(agentproto.MailboxSpec{
		ForwardTo: []string{"a@example.org", "b@example.org"},
		AutoReply: &agentproto.AutoReply{Subject: `Away "for now"`, Body: "Back on\nMonday \\o/", Start: "2026-11-01", End: "2026-11-30"},
	})
	for _, want := range []string{
		`require ["vacation", "copy", "date", "relational"];`,
		`if allof(currentdate :value "ge" "date" "2026-11-01", currentdate :value "le" "date" "2026-11-30") {`,
		`vacation :days 1 :subject "Away \"for now\"" "Back on` + "\n" + `Monday \\o/";`,
		`redirect :copy "a@example.org";`,
		"redirect \"b@example.org\";\n", // no copy kept: the last forward takes the message
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
	s = sieveScript(agentproto.MailboxSpec{KeepCopy: true, ForwardTo: []string{"a@example.org"}})
	if !strings.Contains(s, `redirect :copy "a@example.org";`) || strings.Contains(s, "vacation :") {
		t.Errorf("forward with a copy:\n%s", s)
	}
}

func tarGz(t *testing.T, entries ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(h.Name))
		}
		tw.WriteHeader(h)
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(h.Name))
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestUntarAll(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out")
	good := tarGz(t, &tar.Header{Name: "app-1/", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "app-1/bin/run.sh", Typeflag: tar.TypeReg, Mode: 0o755},
		&tar.Header{Name: "app-1/index.php", Typeflag: tar.TypeReg, Mode: 0o600})
	if err := untarAll(good, "app-1/", dest); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dest, "bin/run.sh")); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("program: %v %v", info, err)
	}
	if info, err := os.Stat(filepath.Join(dest, "index.php")); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("readable file: %v %v", info, err)
	}
	for name, archive := range map[string][]byte{
		"escape":       tarGz(t, &tar.Header{Name: "app-1/../../etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644}),
		"link":         tarGz(t, &tar.Header{Name: "app-1/x", Typeflag: tar.TypeSymlink, Linkname: "/etc/shadow"}),
		"other folder": tarGz(t, &tar.Header{Name: "other/x", Typeflag: tar.TypeReg, Mode: 0o644}),
		"device":       tarGz(t, &tar.Header{Name: "app-1/dev", Typeflag: tar.TypeChar}),
	} {
		if err := untarAll(archive, "app-1/", filepath.Join(t.TempDir(), "x")); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFirewallMailPorts(t *testing.T) {
	rules, err := firewallRules(agentproto.FirewallSpec{Enabled: true, Mail: true}, []int{22})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rules {
		got = append(got, strings.Join(r, " "))
	}
	for _, p := range []string{"allow 25/tcp", "allow 465/tcp", "allow 587/tcp", "allow 993/tcp", "allow 995/tcp"} {
		if !strings.Contains(strings.Join(got, ","), p) {
			t.Errorf("missing %s in %v", p, got)
		}
	}
	rules, _ = firewallRules(agentproto.FirewallSpec{Enabled: true}, []int{22})
	for _, r := range rules {
		if r[1] == "25/tcp" {
			t.Error("mail ports open without email")
		}
	}
}

func TestWebmailTemplates(t *testing.T) {
	vars := map[string]any{"Data": webmailData, "Key": "0123456789abcdef01234567", "Root": roundcubeDir, "User": webmailUser,
		"Socket": webmailSocket, "Host": "mail.example.com", "Internal": true}
	var b bytes.Buffer
	if err := webmailCaddyTemplate.Execute(&b, vars); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mail.example.com {", "tls internal", "php_fastcgi unix/" + webmailSocket, "root * " + roundcubeDir + "/public_html"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("Caddy site lacks %q:\n%s", want, b.String())
		}
	}
	b.Reset()
	webmailConfig.Execute(&b, vars)
	for _, want := range []string{"'sqlite:////var/lib/ekpanel-webmail/db/roundcube.sqlite?mode=0600'", "'ssl://127.0.0.1:993'", "$config['enable_installer'] = false;"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("config lacks %q", want)
		}
	}
}
