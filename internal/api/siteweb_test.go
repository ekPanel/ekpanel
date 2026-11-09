package api_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"testing"
	"time"
)

func testCert(t *testing.T, names []string, from, until time.Time) (string, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: from, NotAfter: until}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalPKCS8PrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}))
}

func TestSiteExtras(t *testing.T) {
	p := newTestPanel(t)
	admin := newClient(t, p.web.URL)
	setUp(t, admin)
	serverID, _, _, _ := enrolled(t, p, admin, "web-1")
	type site struct {
		ID  string `json:"id"`
		TLS string `json:"tls"`
		Web struct {
			AliasRedirect bool                           `json:"alias_redirect"`
			Redirects     []map[string]any               `json:"redirects"`
			Hotlink       *struct{ Extensions []string } `json:"hotlink"`
			PHP           map[string]string              `json:"php"`
		} `json:"web"`
		TLSExpiresAt *time.Time `json:"tls_expires_at"`
		TLSNames     []string   `json:"tls_names"`
	}
	var s site
	if st := admin.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "shop.example.com", "aliases": []string{"www.shop.example.com"},
		"kind": "php", "php_version": "8.4"}, &s); st != http.StatusCreated {
		t.Fatalf("site: %d", st)
	}
	base := "/api/v1/sites/" + s.ID

	// Redirects, hotlink protection and PHP settings.
	var e errBody
	for name, bad := range map[string]map[string]any{
		"brace":         {"redirects": []map[string]any{{"from": "/a", "to": "/{x}", "code": 301}}},
		"unknown php":   {"php": map[string]string{"disable_functions": "x"}},
		"bad file type": {"hotlink": map[string]any{"extensions": []string{"j pg"}}},
	} {
		if st := admin.do("PUT", base+"/web", bad, &e); st != http.StatusBadRequest || e.Code != "invalid_site_web" {
			t.Errorf("%s: %d %s", name, st, e.Code)
		}
	}
	web := map[string]any{"alias_redirect": true,
		"redirects": []map[string]any{{"from": "/old", "to": "/new", "code": 301}, {"from": "/blog/*", "to": "https://blog.example.com", "code": 302, "keep_path": true}},
		"hotlink":   map[string]any{"extensions": []string{".JPG", "png"}, "allowed": []string{"Partner.com"}},
		"php":       map[string]string{"memory_limit": "512M", "upload_max_filesize": "64M", "display_errors": ""}}
	if st := admin.do("PUT", base+"/web", web, &s); st != http.StatusOK {
		t.Fatalf("web: %d", st)
	}
	if !s.Web.AliasRedirect || len(s.Web.Redirects) != 2 || s.Web.Hotlink == nil || s.Web.Hotlink.Extensions[0] != "jpg" ||
		len(s.Web.PHP) != 1 || s.Web.PHP["memory_limit"] != "512M" {
		t.Fatalf("saved web settings (defaults and empty values dropped): %+v", s.Web)
	}

	// Certificates: they must fit their key, be valid now and cover the domain.
	now := time.Now()
	cert, key := testCert(t, []string{"shop.example.com"}, now.Add(-time.Hour), now.Add(60*24*time.Hour))
	_, otherKey := testCert(t, []string{"shop.example.com"}, now.Add(-time.Hour), now.Add(time.Hour))
	expired, expiredKey := testCert(t, []string{"shop.example.com"}, now.Add(-48*time.Hour), now.Add(-time.Hour))
	elsewhere, elsewhereKey := testCert(t, []string{"other.example.com"}, now.Add(-time.Hour), now.Add(time.Hour))
	for name, pair := range map[string][2]string{"wrong key": {cert, otherKey}, "expired": {expired, expiredKey}, "other domain": {elsewhere, elsewhereKey}, "garbage": {"x", "y"}} {
		if st := admin.do("PUT", base+"/certificate", map[string]string{"cert_pem": pair[0], "key_pem": pair[1]}, &e); st != http.StatusBadRequest || e.Code != "invalid_certificate" {
			t.Errorf("%s: %d %s", name, st, e.Code)
		}
	}
	if st := admin.do("PATCH", base, map[string]any{"kind": "php", "php_version": "8.4", "tls": "custom", "aliases": []string{"www.shop.example.com"}}, &e); e.Code != "certificate_required" {
		t.Errorf("custom without a certificate: %d %s", st, e.Code)
	}
	var up struct {
		Site      site     `json:"site"`
		Uncovered []string `json:"uncovered"`
	}
	if st := admin.do("PUT", base+"/certificate", map[string]string{"cert_pem": cert, "key_pem": key}, &up); st != http.StatusOK {
		t.Fatalf("upload: %d", st)
	}
	if up.Site.TLS != "custom" || up.Site.TLSExpiresAt == nil || up.Site.TLSNames[0] != "shop.example.com" || len(up.Uncovered) != 1 || up.Uncovered[0] != "www.shop.example.com" {
		t.Fatalf("uploaded: %+v", up)
	}
	// Saving other settings keeps it; choosing a free certificate drops it.
	if st := admin.do("PATCH", base, map[string]any{"kind": "php", "php_version": "8.4", "tls": "custom", "aliases": []string{"www.shop.example.com"}}, &s); st != http.StatusOK || s.TLS != "custom" {
		t.Fatalf("keep: %d %s", st, s.TLS)
	}
	if st := admin.do("DELETE", base+"/certificate", nil, &s); st != http.StatusOK || s.TLS != "auto" || s.TLSExpiresAt != nil {
		t.Fatalf("remove: %d %+v", st, s)
	}

	// Another customer cannot touch the site; PHP extensions are for administrators.
	var pkg struct {
		ID string `json:"id"`
	}
	admin.do("POST", "/api/v1/packages", map[string]any{"name": "P", "max_sites": 1}, &pkg)
	var c struct {
		Password string `json:"password"`
	}
	admin.do("POST", "/api/v1/customers", map[string]string{"email": "c@example.com", "name": "C", "package_id": pkg.ID}, &c)
	cust := login(t, p.web.URL, "c@example.com", c.Password)
	if st := cust.do("PUT", base+"/web", web, &e); st != http.StatusNotFound {
		t.Errorf("customer on another's site: %d", st)
	}
	if st := cust.do("POST", "/api/v1/servers/"+serverID+"/php/extensions", map[string]any{"version": "8.4", "name": "redis", "installed": true}, &e); st != http.StatusForbidden {
		t.Errorf("customer changing PHP extensions: %d", st)
	}
	for _, bad := range []map[string]any{{"version": "8.4", "name": "fpm"}, {"version": "5.6", "name": "redis"}, {"version": "8.4", "name": "evil; rm -rf /"}} {
		if st := admin.do("POST", "/api/v1/servers/"+serverID+"/php/extensions", bad, &e); st != http.StatusBadRequest {
			t.Errorf("extension %v: %d", bad, st)
		}
	}
	var task struct {
		Kind string `json:"kind"`
	}
	if st := admin.do("POST", "/api/v1/servers/"+serverID+"/php/extensions", map[string]any{"version": "8.4", "name": "redis", "installed": true}, &task); st != http.StatusAccepted || task.Kind != "php.extension" {
		t.Errorf("extension task: %d %+v", st, task)
	}
}
