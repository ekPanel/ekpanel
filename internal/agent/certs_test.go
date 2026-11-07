package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"ekpanel/internal/agentproto"
)

func selfSigned(t *testing.T, name string) (string, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalPKCS8PrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}))
}

func TestSiteExtras(t *testing.T) {
	f := newFakeSystem(t)
	sp := phpSite()
	sp.Aliases = []string{"www.example.com"}
	sp.Web = agentproto.SiteWeb{
		AliasRedirect: true,
		Redirects: []agentproto.Redirect{
			{From: "/old", To: "/new", Code: 301},
			{From: "/blog/*", To: "https://blog.example.com/", Code: 302, KeepPath: true},
			{From: "/shop/*", To: "https://shop.example.com", Code: 308},
			{From: "/*", To: "https://example.org", Code: 301, KeepPath: true},
		},
		Hotlink: &agentproto.Hotlink{Extensions: []string{"jpg", "png"}, Allowed: []string{"partner.com", "*.cdn.net"}},
		PHP:     map[string]string{"memory_limit": "512M", "display_errors": "On", "allow_url_fopen": "Off"},
	}
	sp.TLS = "custom"
	sp.CertPEM, sp.KeyPEM = selfSigned(t, sp.Domain)
	res, opErr := stateApply(context.Background(), f.system, agentproto.StateApplyParams{Sites: []agentproto.SiteSpec{sp}})
	if opErr != nil || !res.Sites[0].OK {
		t.Fatalf("apply: %v %+v", opErr, res.Sites)
	}
	conf, _ := os.ReadFile(filepath.Join(f.root, caddySitePath(sp.ID)))
	cert, key := certPaths(sp)
	for _, want := range []string{
		"www.example.com {\n\ttls " + cert + " " + key + "\n\tredir https://" + sp.Domain + "{uri} 301\n}",
		"\n" + sp.Domain + " {\n\ttls " + cert,
		"\t\tredir /old /new 301\n",
		"\t\troute /blog/* {\n\t\t\turi strip_prefix /blog\n\t\t\tredir https://blog.example.com{uri} 302\n\t\t}",
		"\t\tredir /shop/* https://shop.example.com 308\n",
		"\t\tredir * https://example.org{uri} 301\n",
		`path_regexp (?i)\.(jpg|png)$`,
		`not header_regexp Referer (?i)^https?://(` + strings.ReplaceAll(sp.Domain, ".", `\.`) + `|www\.example\.com|partner\.com|([a-z0-9-]+\.)+cdn\.net)(:[0-9]+)?(/|$)`,
	} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("caddy file lacks %q:\n%s", want, conf)
		}
	}
	pool, _ := os.ReadFile(filepath.Join(f.root, groupFor(sp).poolPath(sp.ID)))
	for _, want := range []string{"php_value[memory_limit] = 512M", "php_flag[display_errors] = On", "php_admin_flag[allow_url_fopen] = Off",
		"php_value[upload_max_filesize] = 64M", "php_value[date.timezone] = UTC"} {
		if !strings.Contains(string(pool), want) {
			t.Errorf("pool lacks %q:\n%s", want, pool)
		}
	}
	if info, err := os.Stat(filepath.Join(f.root, key)); err != nil || info.Mode().Perm() != 0o640 || !slices.Contains(f.calls, "chown root:caddy "+filepath.Join(f.root, key)) {
		t.Errorf("key file: %v %v", info, err)
	}

	// A new certificate replaces the old files; going back to a free one removes them.
	sp.CertPEM, sp.KeyPEM = selfSigned(t, sp.Domain)
	sp.Generation++
	stateApply(context.Background(), f.system, agentproto.StateApplyParams{Sites: []agentproto.SiteSpec{sp}})
	if files, _ := filepath.Glob(filepath.Join(f.root, certsDir, "*")); len(files) != 2 {
		t.Errorf("certificate files: %v", files)
	}
	sp.TLS, sp.CertPEM, sp.KeyPEM = "auto", "", ""
	sp.Generation++
	stateApply(context.Background(), f.system, agentproto.StateApplyParams{Sites: []agentproto.SiteSpec{sp}})
	if files, _ := filepath.Glob(filepath.Join(f.root, certsDir, "*")); len(files) != 0 {
		t.Errorf("left behind: %v", files)
	}

	// A key that is not the certificate's is refused.
	other, _ := selfSigned(t, sp.Domain)
	_, otherKey := selfSigned(t, sp.Domain)
	sp.TLS, sp.CertPEM, sp.KeyPEM = "custom", other, otherKey
	if err := validateSite(sp); err == nil {
		t.Error("a key that does not fit was accepted")
	}
}
