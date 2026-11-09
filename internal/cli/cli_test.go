package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const goodToken = "ekp_goodtoken123"

// ---- A fake panel ----

type recorded struct {
	Method, Path, Query, Auth, UA, ContentType, Body string
}

type fakePanel struct {
	mu     sync.Mutex
	routes map[string]http.HandlerFunc // "GET /api/v1/sites"
	reqs   []recorded
}

func newFakePanel() *fakePanel {
	f := &fakePanel{routes: map[string]http.HandlerFunc{}}
	f.handle("GET /api/v1/health", reply(200, `{"status":"ok","version":"v0.2.0"}`))
	f.handle("GET /api/v1/auth/me", reply(200, `{"id":"u1","email":"rakib@example.com","name":"Rakib","role":"admin"}`))
	return f
}

func (f *fakePanel) handle(route string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[route] = h
}

func (f *fakePanel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"),
		r.Header.Get("User-Agent"), r.Header.Get("Content-Type"), string(body)})
	h := f.routes[r.Method+" "+r.URL.Path]
	f.mu.Unlock()
	if r.URL.Path != "/api/v1/health" && r.Header.Get("Authorization") != "Bearer "+goodToken {
		reply(401, `{"code":"invalid_token","message":"this API token does not exist, was revoked or has expired"}`)(w, r)
		return
	}
	if h == nil {
		reply(404, `{"code":"not_found","message":"no such route"}`)(w, r)
		return
	}
	h(w, r)
}

func (f *fakePanel) requests(method, path string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, r := range f.reqs {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

// ---- Running ekctl ----

type testEnv struct {
	*Env
	stdout, stderr *bytes.Buffer
	vars           map[string]string
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	te := &testEnv{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, vars: map[string]string{}}
	te.Env = &Env{
		Stdin: strings.NewReader(""), Stdout: te.stdout, Stderr: te.stderr,
		Getenv:    func(k string) string { return te.vars[k] },
		ConfigDir: t.TempDir(), Version: "1.2.3", PollInterval: 2 * time.Millisecond,
	}
	return te
}

func (te *testEnv) run(args ...string) int {
	te.stdout.Reset()
	te.stderr.Reset()
	return Run(context.Background(), te.Env, args)
}

func (te *testEnv) configFile() string { return filepath.Join(te.ConfigDir, "ekctl", "config.json") }

func (te *testEnv) writeConfig(t *testing.T, cfg Config) {
	t.Helper()
	os.MkdirAll(filepath.Dir(te.configFile()), 0o700)
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(te.configFile(), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (te *testEnv) readConfig(t *testing.T) Config {
	t.Helper()
	data, err := os.ReadFile(te.configFile())
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// loggedIn starts a fake panel and saves a login to it.
func loggedIn(t *testing.T) (*testEnv, *fakePanel) {
	t.Helper()
	f := newFakePanel()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	te := newEnv(t)
	te.writeConfig(t, Config{URL: srv.URL, Token: goodToken})
	return te, f
}

func expect(t *testing.T, te *testEnv, code, want int) {
	t.Helper()
	if code != want {
		t.Fatalf("exit code %d, want %d\nstdout:\n%s\nstderr:\n%s", code, want, te.stdout, te.stderr)
	}
}

func contains(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%s does not contain %q:\n%s", what, w, got)
		}
	}
}

// ---- Login and config ----

func TestLoginSavesConfigPrivately(t *testing.T) {
	f := newFakePanel()
	srv := httptest.NewServer(f)
	defer srv.Close()
	te := newEnv(t)

	expect(t, te, te.run("login", "--url", srv.URL+"/api/v1/", "--token", goodToken), 0)
	contains(t, "stdout", te.stdout.String(), "Logged in to "+srv.URL+" as Rakib <rakib@example.com> (admin).")
	contains(t, "stderr", te.stderr.String(), "does not use HTTPS")

	cfg := te.readConfig(t)
	if cfg.URL != srv.URL || cfg.Token != goodToken {
		t.Errorf("saved %+v", cfg)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(te.configFile())
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("config file mode %v, want 0600", fi.Mode().Perm())
		}
		di, _ := os.Stat(filepath.Dir(te.configFile()))
		if di.Mode().Perm() != 0o700 {
			t.Errorf("config dir mode %v, want 0700", di.Mode().Perm())
		}
	}
	if r := f.requests("GET", "/api/v1/auth/me"); len(r) != 1 || r[0].UA != "ekctl/1.2.3" {
		t.Errorf("auth/me requests %+v", r)
	}

	expect(t, te, te.run("logout"), 0)
	if cfg := te.readConfig(t); cfg.Token != "" || cfg.URL != srv.URL {
		t.Errorf("after logout %+v", cfg)
	}
	expect(t, te, te.run("sites", "list"), 1)
	contains(t, "stderr", te.stderr.String(), "Not logged in to "+srv.URL)
}

func TestLoginTokenFromStdin(t *testing.T) {
	f := newFakePanel()
	srv := httptest.NewServer(f)
	defer srv.Close()

	te := newEnv(t)
	te.Stdin = strings.NewReader(goodToken + "\n")
	expect(t, te, te.run("login", "--url", srv.URL), 0)
	if te.readConfig(t).Token != goodToken {
		t.Error("token from stdin not saved")
	}

	// On a terminal the token is read with echo off, and echo is turned back on.
	te = newEnv(t)
	te.Stdin = strings.NewReader(goodToken + "\n")
	te.StdinIsTerminal = true
	var off, on bool
	te.NoEcho = func() (func(), error) { off = true; return func() { on = true }, nil }
	expect(t, te, te.run("login", "--url", srv.URL), 0)
	if !off || !on {
		t.Errorf("echo off %v, back on %v", off, on)
	}
	contains(t, "stderr", te.stderr.String(), "API token")

	// A wrong token is refused and nothing is saved.
	te = newEnv(t)
	expect(t, te, te.run("login", "--url", srv.URL, "--token", "ekp_wrongtoken"), 1)
	contains(t, "stderr", te.stderr.String(), "did not accept this token")
	if _, err := os.Stat(te.configFile()); err == nil {
		t.Error("config saved after a refused token")
	}

	// Not a token at all: a usage error.
	expect(t, te, te.run("login", "--url", srv.URL, "--token", "hunter2"), 2)
}

func TestLoginTokenWithoutReadScope(t *testing.T) {
	f := newFakePanel()
	f.handle("GET /api/v1/auth/me", reply(403, `{"code":"token_scope","message":"this API token does not have the scope for this: read"}`))
	srv := httptest.NewServer(f)
	defer srv.Close()
	te := newEnv(t)
	expect(t, te, te.run("login", "--url", srv.URL, "--token", goodToken), 0)
	contains(t, "stderr", te.stderr.String(), "did not say whose token this is")
	if te.readConfig(t).Token != goodToken {
		t.Error("token not saved")
	}
}

func TestPrecedenceFlagEnvConfig(t *testing.T) {
	var panels [3]*fakePanel
	var urls [3]string
	for i := range panels {
		panels[i] = newFakePanel()
		panels[i].handle("GET /api/v1/sites", reply(200, `[]`))
		srv := httptest.NewServer(panels[i])
		defer srv.Close()
		urls[i] = srv.URL
	}
	cfgPanel, envPanel, flagPanel := panels[0], panels[1], panels[2]
	te := newEnv(t)
	te.writeConfig(t, Config{URL: urls[0], Token: goodToken})

	expect(t, te, te.run("sites", "list"), 0)
	if len(cfgPanel.requests("GET", "/api/v1/sites")) != 1 {
		t.Fatal("config URL not used")
	}

	te.vars["EKCTL_URL"] = urls[1]
	te.vars["EKCTL_TOKEN"] = goodToken
	expect(t, te, te.run("sites", "list"), 0)
	if len(envPanel.requests("GET", "/api/v1/sites")) != 1 {
		t.Fatal("EKCTL_URL does not override the config")
	}

	expect(t, te, te.run("sites", "list", "--url", urls[2]), 0)
	if len(flagPanel.requests("GET", "/api/v1/sites")) != 1 {
		t.Fatal("--url does not override EKCTL_URL")
	}

	// --token overrides EKCTL_TOKEN.
	expect(t, te, te.run("--token", "ekp_otherone", "sites", "list"), 1)
	if r := envPanel.requests("GET", "/api/v1/sites"); r[len(r)-1].Auth != "Bearer ekp_otherone" {
		t.Errorf("--token not used: %q", r[len(r)-1].Auth)
	}
	contains(t, "stderr", te.stderr.String(), "(invalid_token)")

	// The saved token is never sent to another panel.
	delete(te.vars, "EKCTL_TOKEN")
	expect(t, te, te.run("sites", "list"), 1)
	contains(t, "stderr", te.stderr.String(), "Not logged in to "+urls[1])
	for _, r := range envPanel.requests("GET", "/api/v1/sites") {
		if r.Auth == "" {
			continue
		}
		if r.Auth != "Bearer "+goodToken && r.Auth != "Bearer ekp_otherone" {
			t.Errorf("unexpected token %q", r.Auth)
		}
	}
	if n := len(envPanel.requests("GET", "/api/v1/sites")); n != 2 {
		t.Errorf("env panel got %d requests, want 2", n)
	}

	// --config chooses another file.
	other := filepath.Join(t.TempDir(), "other.json")
	os.WriteFile(other, []byte(`{"url":"`+urls[2]+`","token":"`+goodToken+`"}`), 0o600)
	delete(te.vars, "EKCTL_URL")
	expect(t, te, te.run("sites", "list", "--config", other), 0)
	if len(flagPanel.requests("GET", "/api/v1/sites")) != 2 {
		t.Error("--config not used")
	}
}

// ---- Certificate pinning ----

func newCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "panel.test"},
		NotBefore:    time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func newTLSPanel(t *testing.T, f *fakePanel, cert tls.Certificate) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(f)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // refused handshakes are expected
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestCertificatePinning(t *testing.T) {
	f := newFakePanel()
	f.handle("GET /api/v1/sites", reply(200, `[]`))
	cert := newCert(t)
	srv := newTLSPanel(t, f, cert)
	fp := fingerprint(cert.Certificate[0])

	// Without a terminal or --fingerprint, an untrusted certificate is refused.
	te := newEnv(t)
	expect(t, te, te.run("login", "--url", srv.URL, "--token", goodToken), 1)
	contains(t, "stderr", te.stderr.String(), "not trusted", fp, "--fingerprint "+fp)
	if _, err := os.Stat(te.configFile()); err == nil {
		t.Fatal("config saved for an untrusted certificate")
	}
	if len(f.requests("GET", "/api/v1/auth/me")) != 0 {
		t.Fatal("the token was sent over an untrusted connection")
	}

	// A wrong --fingerprint is refused.
	wrong := strings.Repeat("00:", 31) + "00"
	expect(t, te, te.run("login", "--url", srv.URL, "--token", goodToken, "--fingerprint", wrong), 1)
	contains(t, "stderr", te.stderr.String(), "not the "+wrong)

	// On a terminal, saying no refuses it.
	te.StdinIsTerminal = true
	te.Stdin = strings.NewReader("n\n")
	expect(t, te, te.run("login", "--url", srv.URL, "--token", goodToken), 1)
	contains(t, "stderr", te.stderr.String(), "Trust this certificate? [y/N]")

	// Saying yes trusts it and saves the fingerprint.
	te.Stdin = strings.NewReader("y\n")
	expect(t, te, te.run("login", "--url", srv.URL, "--token", goodToken), 0)
	if got := te.readConfig(t).TrustedCerts[hostPort(srv.URL)]; got != fp {
		t.Fatalf("saved fingerprint %q, want %q", got, fp)
	}

	// Later commands accept that certificate.
	te.StdinIsTerminal = false
	expect(t, te, te.run("sites", "list"), 0)

	// --fingerprint works without a terminal, in any spelling.
	te2 := newEnv(t)
	expect(t, te2, te2.run("login", "--url", srv.URL, "--token", goodToken, "--fingerprint", strings.ToLower(strings.ReplaceAll(fp, ":", ""))), 0)
	if te2.readConfig(t).TrustedCerts[hostPort(srv.URL)] != fp {
		t.Fatal("--fingerprint not saved")
	}

	// A panel with another certificate behind a pinned address is refused.
	other := newTLSPanel(t, f, newCert(t))
	te3 := newEnv(t)
	te3.writeConfig(t, Config{URL: other.URL, Token: goodToken, TrustedCerts: map[string]string{hostPort(other.URL): fp}})
	before := len(f.requests("GET", "/api/v1/sites"))
	expect(t, te3, te3.run("sites", "list"), 1)
	contains(t, "stderr", te3.stderr.String(), "is not the one you trusted", "ekctl login --url "+other.URL)
	if len(f.requests("GET", "/api/v1/sites")) != before {
		t.Fatal("a request went to a panel with the wrong certificate")
	}

	// Without a saved fingerprint, ordinary commands refuse it too.
	te4 := newEnv(t)
	te4.writeConfig(t, Config{URL: srv.URL, Token: goodToken})
	expect(t, te4, te4.run("sites", "list"), 1)
	contains(t, "stderr", te4.stderr.String(), "is not trusted")
}

func TestPinnedTLSAcceptsOnlyThePin(t *testing.T) {
	a, b := newCert(t), newCert(t)
	cfg := pinnedTLS(fingerprint(a.Certificate[0]))
	leafA, _ := x509.ParseCertificate(a.Certificate[0])
	leafB, _ := x509.ParseCertificate(b.Certificate[0])
	if err := cfg.VerifyConnection(tls.ConnectionState{ServerName: "127.0.0.1", PeerCertificates: []*x509.Certificate{leafA}}); err != nil {
		t.Errorf("pinned certificate refused: %v", err)
	}
	if err := cfg.VerifyConnection(tls.ConnectionState{ServerName: "127.0.0.1", PeerCertificates: []*x509.Certificate{leafB}}); err == nil {
		t.Error("another certificate accepted")
	}
	if err := cfg.VerifyConnection(tls.ConnectionState{}); err == nil {
		t.Error("no certificate accepted")
	}
}

// ---- Output ----

const sitesJSON = `[
 {"id":"11111111-1111-1111-1111-111111111111","server_id":"s1","server_name":"web1","domain":"example.com","domain_display":"example.com","kind":"php","php_version":"8.4","state":"active","generation":1,"applied_generation":1},
 {"id":"22222222-2222-2222-2222-222222222222","server_id":"s1","server_name":"web1","domain":"xn--bcher-kva.example","domain_display":"bücher.example","kind":"static","php_version":null,"state":"pending","generation":2,"applied_generation":1}
]`

func TestSitesListTableAndJSON(t *testing.T) {
	te, f := loggedIn(t)
	f.handle("GET /api/v1/sites", reply(200, sitesJSON))

	expect(t, te, te.run("sites", "list"), 0)
	lines := strings.Split(strings.TrimRight(te.stdout.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("table:\n%s", te.stdout)
	}
	contains(t, "header", lines[0], "ID", "DOMAIN", "KIND", "PHP", "SERVER", "STATE")
	contains(t, "row", lines[1], "11111111-1111-1111-1111-111111111111", "example.com", "php", "8.4", "web1", "active")
	contains(t, "row", lines[2], "bücher.example", "static", " - ", "pending")
	// Columns are aligned.
	if strings.Index(lines[0], "DOMAIN") != strings.Index(lines[1], "example.com") {
		t.Errorf("columns not aligned:\n%s", te.stdout)
	}

	expect(t, te, te.run("sites", "list", "--json"), 0)
	var got []map[string]any
	if err := json.Unmarshal(te.stdout.Bytes(), &got); err != nil || len(got) != 2 {
		t.Fatalf("--json output %v:\n%s", err, te.stdout)
	}
	if got[1]["domain"] != "xn--bcher-kva.example" {
		t.Errorf("--json changed the data: %v", got[1])
	}
	if !strings.Contains(te.stdout.String(), "\n  {") {
		t.Errorf("--json is not pretty:\n%s", te.stdout)
	}

	// An empty list prints nothing on stdout.
	f.handle("GET /api/v1/sites", reply(200, `[]`))
	expect(t, te, te.run("sites", "ls"), 0)
	if te.stdout.Len() != 0 || !strings.Contains(te.stderr.String(), "No sites.") {
		t.Errorf("empty list: stdout %q stderr %q", te.stdout, te.stderr)
	}
}

func TestSitesGetByDomainWithInterspersedFlags(t *testing.T) {
	te, f := loggedIn(t)
	f.handle("GET /api/v1/sites", reply(200, sitesJSON))
	f.handle("GET /api/v1/sites/11111111-1111-1111-1111-111111111111", reply(200, `{"id":"11111111-1111-1111-1111-111111111111","domain":"example.com","kind":"php","php_version":"8.4","state":"active","server_name":"web1","linux_user":"example_ab12"}`))

	expect(t, te, te.run("sites", "get", "example.com"), 0)
	contains(t, "stdout", te.stdout.String(), "Linux user:", "example_ab12", "PHP:", "8.4")

	expect(t, te, te.run("sites", "get", "--json", "EXAMPLE.COM"), 0)
	contains(t, "stdout", te.stdout.String(), `"linux_user": "example_ab12"`)

	expect(t, te, te.run("sites", "get", "nope.example"), 1)
	contains(t, "stderr", te.stderr.String(), `No site is called "nope.example"`)
}

const tasksJSON = `[
 {"id":"aaaaaaaa-0000-0000-0000-000000000001","kind":"backup","server_name":"web1","status":"failed","error":"repository locked","created_at":"2026-11-09T10:00:00Z"},
 {"id":"aaaaaaaa-0000-0000-0000-000000000002","kind":"reconcile","server_name":null,"status":"succeeded","error":null,"created_at":"2026-11-09T09:00:00Z"}
]`

func TestTasksListTableAndJSON(t *testing.T) {
	te, f := loggedIn(t)
	f.handle("GET /api/v1/tasks", reply(200, tasksJSON))

	expect(t, te, te.run("tasks", "list", "--active", "--limit", "5"), 0)
	out := te.stdout.String()
	contains(t, "table", out, "ID", "KIND", "STATUS", "SERVER", "ERROR", "backup", "failed", "repository locked", "reconcile", "succeeded")
	r := f.requests("GET", "/api/v1/tasks")
	if q := r[len(r)-1].Query; q != "active=1&limit=5" {
		t.Errorf("query %q", q)
	}

	expect(t, te, te.run("--json", "tasks", "list"), 0)
	var got []map[string]any
	if err := json.Unmarshal(te.stdout.Bytes(), &got); err != nil || len(got) != 2 || got[0]["error"] != "repository locked" {
		t.Fatalf("--json: %v\n%s", err, te.stdout)
	}

	expect(t, te, te.run("tasks", "list", "--limit", "500"), 2)
}

// ---- Waiting ----

func TestSitesCreateWait(t *testing.T) {
	te, f := loggedIn(t)
	const id = "33333333-3333-3333-3333-333333333333"
	f.handle("GET /api/v1/servers", reply(200, `[{"id":"44444444-4444-4444-4444-444444444444","name":"web1"}]`))
	f.handle("POST /api/v1/sites", reply(201, `{"id":"`+id+`","domain":"new.example","server_name":"web1","state":"pending","generation":1,"applied_generation":0}`))
	var mu sync.Mutex
	polls := 0
	f.handle("GET /api/v1/sites/"+id, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		polls++
		n := polls
		mu.Unlock()
		if n < 3 {
			reply(200, `{"id":"`+id+`","domain":"new.example","state":"pending","generation":1,"applied_generation":0}`)(w, r)
			return
		}
		reply(200, `{"id":"`+id+`","domain":"new.example","state":"active","generation":1,"applied_generation":1}`)(w, r)
	})

	expect(t, te, te.run("sites", "create", "new.example", "--server", "web1", "--php", "8.4", "--wait"), 0)
	contains(t, "stdout", te.stdout.String(), "Site new.example is ready")
	if polls != 3 {
		t.Errorf("polled %d times, want 3", polls)
	}
	req := f.requests("POST", "/api/v1/sites")[0]
	var body map[string]any
	json.Unmarshal([]byte(req.Body), &body)
	if body["server_id"] != "44444444-4444-4444-4444-444444444444" || body["kind"] != "php" || body["php_version"] != "8.4" || body["domain"] != "new.example" {
		t.Errorf("body %v", body)
	}
	if req.ContentType != "application/json" {
		t.Errorf("content type %q", req.ContentType)
	}

	// A failure while setting up is an error.
	f.handle("GET /api/v1/sites/"+id, reply(200, `{"id":"`+id+`","state":"error","message":"nginx -t failed","generation":1,"applied_generation":0}`))
	expect(t, te, te.run("sites", "create", "new.example", "--server", "44444444-4444-4444-4444-444444444444", "--wait"), 1)
	contains(t, "stderr", te.stderr.String(), "nginx -t failed")

	// --timeout gives up.
	f.handle("GET /api/v1/sites/"+id, reply(200, `{"id":"`+id+`","state":"pending","generation":1,"applied_generation":0}`))
	expect(t, te, te.run("sites", "create", "new.example", "--server", "web1", "--wait", "--timeout", "20ms"), 1)
	contains(t, "stderr", te.stderr.String(), "Stopped waiting after 20ms")

	// Usage errors.
	expect(t, te, te.run("sites", "create", "new.example"), 2)
	contains(t, "stderr", te.stderr.String(), "Missing --server")
	expect(t, te, te.run("sites", "create", "new.example", "--server", "web1", "--kind", "static", "--php", "8.4"), 2)
}

func TestBackupsRunWaitAndTasksWait(t *testing.T) {
	te, f := loggedIn(t)
	const repo = "55555555-5555-5555-5555-555555555555"
	const task = "66666666-6666-6666-6666-666666666666"
	f.handle("POST /api/v1/backups/"+repo+"/run", reply(202, `{"id":"`+task+`","kind":"backup","status":"queued"}`))
	var mu sync.Mutex
	polls := 0
	f.handle("GET /api/v1/tasks/"+task, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		polls++
		n := polls
		mu.Unlock()
		status := map[bool]string{true: "running", false: "succeeded"}[n < 3]
		reply(200, `{"id":"`+task+`","kind":"backup","status":"`+status+`"}`)(w, r)
	})

	expect(t, te, te.run("backups", "run", repo, "--wait"), 0)
	contains(t, "stdout", te.stdout.String(), "Task "+task+" (backup) succeeded.")
	contains(t, "stderr", te.stderr.String(), "running", "succeeded")

	// A failed task: exit 1, and --json still prints it.
	f.handle("GET /api/v1/tasks/"+task, reply(200, `{"id":"`+task+`","kind":"backup","status":"failed","error":"disk full"}`))
	expect(t, te, te.run("tasks", "wait", task, "--json"), 1)
	contains(t, "stderr", te.stderr.String(), "failed: disk full")
	contains(t, "stdout", te.stdout.String(), `"error": "disk full"`)

	// An unknown task.
	f.handle("GET /api/v1/tasks/"+task, reply(404, `{"code":"task_not_found","message":"no such task"}`))
	expect(t, te, te.run("tasks", "wait", task), 1)
	contains(t, "stderr", te.stderr.String(), "No task "+task)
	expect(t, te, te.run("tasks", "wait", "abc"), 2)
}

func TestAppsDeployWait(t *testing.T) {
	te, f := loggedIn(t)
	const app = "77777777-7777-7777-7777-777777777777"
	f.handle("GET /api/v1/apps", reply(200, `[{"id":"`+app+`","name":"shop-api"}]`))
	f.handle("POST /api/v1/apps/"+app+"/deploy", reply(202, `{"id":"d1","number":4,"status":"queued"}`))
	f.handle("GET /api/v1/apps/"+app+"/deployments/d1", reply(200, `{"id":"d1","number":4,"status":"failed","error":"npm ci failed"}`))
	expect(t, te, te.run("apps", "deploy", "shop-api", "--wait"), 1)
	contains(t, "stderr", te.stderr.String(), "Deployment #4 failed: npm ci failed", "/deployments/d1/log")

	f.handle("GET /api/v1/apps/"+app+"/deployments/d1", reply(200, `{"id":"d1","number":4,"status":"live"}`))
	expect(t, te, te.run("apps", "deploy", "shop-api", "--wait"), 0)
	contains(t, "stdout", te.stdout.String(), "Deployment #4 of shop-api is live.")
}

// ---- Deleting ----

func TestDeleteConfirmation(t *testing.T) {
	te, f := loggedIn(t)
	const id = "11111111-1111-1111-1111-111111111111"
	f.handle("GET /api/v1/sites/"+id, reply(200, `{"id":"`+id+`","domain":"example.com","server_name":"web1"}`))
	f.handle("DELETE /api/v1/sites/"+id, reply(202, `{"id":"`+id+`","state":"deleting"}`))

	// No terminal and no --yes: refused before anything is deleted.
	expect(t, te, te.run("sites", "delete", id), 2)
	contains(t, "stderr", te.stderr.String(), "--yes")

	// A wrong name: refused.
	te.StdinIsTerminal = true
	te.Stdin = strings.NewReader("example.org\n")
	expect(t, te, te.run("sites", "delete", id), 1)
	contains(t, "stderr", te.stderr.String(), "Type example.com to confirm", "Nothing was deleted")
	if len(f.requests("DELETE", "/api/v1/sites/"+id)) != 0 {
		t.Fatal("deleted without confirmation")
	}

	te.Stdin = strings.NewReader("example.com\n")
	expect(t, te, te.run("sites", "delete", id), 0)
	te.StdinIsTerminal = false
	expect(t, te, te.run("sites", "rm", id, "--yes"), 0)
	if n := len(f.requests("DELETE", "/api/v1/sites/"+id)); n != 2 {
		t.Errorf("%d deletes, want 2", n)
	}
}

func TestDatabasesCreateShowsPassword(t *testing.T) {
	te, f := loggedIn(t)
	f.handle("GET /api/v1/servers", reply(200, `[{"id":"44444444-4444-4444-4444-444444444444","name":"web1"}]`))
	f.handle("POST /api/v1/databases", reply(201, `{"database":{"id":"d1","name":"shop","engine":"mariadb","username":"shopuser","server_name":"web1"},"password":"s3cret"}`))
	expect(t, te, te.run("databases", "create", "shop", "--server", "web1", "--engine", "mariadb", "--user", "shopuser"), 0)
	contains(t, "stdout", te.stdout.String(), "s3cret", "shopuser")
	var body map[string]any
	json.Unmarshal([]byte(f.requests("POST", "/api/v1/databases")[0].Body), &body)
	if body["username"] != "shopuser" || body["engine"] != "mariadb" || body["name"] != "shop" {
		t.Errorf("body %v", body)
	}
	expect(t, te, te.run("db", "create", "shop", "--server", "web1", "--engine", "mysql"), 2)
}

func TestDNSRecordsAdd(t *testing.T) {
	te, f := loggedIn(t)
	const zone = "88888888-8888-8888-8888-888888888888"
	f.handle("GET /api/v1/dns/zones", reply(200, `[{"id":"`+zone+`","name":"example.com"}]`))
	f.handle("POST /api/v1/dns/zones/"+zone+"/records", reply(201, `{"id":"r1","type":"A","name":"www.example.com","content":"203.0.113.7","ttl":1,"proxied":true}`))
	expect(t, te, te.run("dns", "records", "add", "example.com", "--type", "a", "--name", "www", "--content", "203.0.113.7", "--proxied"), 0)
	contains(t, "stdout", te.stdout.String(), "Added A www.example.com → 203.0.113.7 (r1)")
	var body map[string]any
	json.Unmarshal([]byte(f.requests("POST", "/api/v1/dns/zones/"+zone+"/records")[0].Body), &body)
	if body["type"] != "A" || body["proxied"] != true || body["ttl"] != float64(1) {
		t.Errorf("body %v", body)
	}
	expect(t, te, te.run("dns", "records", "add", "example.com", "--type", "MX", "--name", "@", "--content", "mx.example.com"), 2)
	contains(t, "stderr", te.stderr.String(), "--priority")
}

// ---- The api command ----

func TestAPICommand(t *testing.T) {
	te, f := loggedIn(t)
	f.handle("GET /api/v1/sites", reply(200, `[{"id":"x"}]`))
	f.handle("POST /api/v1/cron", reply(201, `{"id":"c1"}`))
	f.handle("DELETE /api/v1/cron/c1", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	f.handle("GET /api/v1/sites/missing", reply(404, `{"code":"site_not_found","message":"no site with that ID"}`))

	expect(t, te, te.run("api", "get", "sites"), 0)
	contains(t, "stdout", te.stdout.String(), `"id": "x"`)
	expect(t, te, te.run("api", "GET", "/api/v1/sites"), 0)
	expect(t, te, te.run("api", "GET", "/sites"), 0)
	if n := len(f.requests("GET", "/api/v1/sites")); n != 3 {
		t.Errorf("%d requests, want 3", n)
	}

	expect(t, te, te.run("api", "POST", "cron", "--data", `{"schedule":"@daily"}`), 0)
	file := filepath.Join(t.TempDir(), "job.json")
	os.WriteFile(file, []byte(`{"from":"file"}`), 0o600)
	expect(t, te, te.run("api", "POST", "cron", "--data", "@"+file), 0)
	te.Stdin = strings.NewReader(`{"from":"stdin"}`)
	expect(t, te, te.run("api", "--data", "-", "POST", "cron"), 0)
	posts := f.requests("POST", "/api/v1/cron")
	if len(posts) != 3 || posts[0].Body != `{"schedule":"@daily"}` || posts[1].Body != `{"from":"file"}` || posts[2].Body != `{"from":"stdin"}` {
		t.Errorf("bodies %+v", posts)
	}

	expect(t, te, te.run("api", "DELETE", "cron/c1"), 0)
	if te.stdout.Len() != 0 {
		t.Errorf("204 printed %q", te.stdout)
	}

	expect(t, te, te.run("api", "GET", "sites/missing"), 1)
	contains(t, "stdout", te.stdout.String(), `"code": "site_not_found"`)
	contains(t, "stderr", te.stderr.String(), "Error: no site with that ID (site_not_found)")

	expect(t, te, te.run("api", "FETCH", "sites"), 2)
	expect(t, te, te.run("api", "POST", "cron", "--data", "{not json"), 2)
	expect(t, te, te.run("api", "GET", "https://evil.example/api/v1/sites"), 2)
	expect(t, te, te.run("api", "GET"), 2)
}

// ---- Errors ----

func TestAPIErrors(t *testing.T) {
	te, f := loggedIn(t)
	te.vars["EKCTL_TOKEN"] = "ekp_revoked"
	expect(t, te, te.run("sites", "list"), 1)
	contains(t, "stderr", te.stderr.String(),
		"Error: this API token does not exist, was revoked or has expired (invalid_token)",
		"ekctl login --url http://127.0.0.1")
	delete(te.vars, "EKCTL_TOKEN")

	f.handle("GET /api/v1/databases", reply(403, `{"code":"token_scope","message":"this API token does not have the scope for this: read or databases"}`))
	expect(t, te, te.run("databases", "list"), 1)
	contains(t, "stderr", te.stderr.String(),
		"Error: this API token does not have the scope for this: read or databases (token_scope)",
		"Create a token with that scope")

	f.handle("GET /api/v1/servers", reply(502, `<html>Bad Gateway</html>`))
	expect(t, te, te.run("servers", "list"), 1)
	contains(t, "stderr", te.stderr.String(), "Error: Bad Gateway (http_502)", "Check the panel URL")

	// A panel that is not there.
	te.writeConfig(t, Config{URL: "http://127.0.0.1:1", Token: goodToken})
	expect(t, te, te.run("servers", "list"), 1)
	contains(t, "stderr", te.stderr.String(), "Cannot reach http://127.0.0.1:1")
}

func TestUsageAndHelp(t *testing.T) {
	te := newEnv(t)
	expect(t, te, te.run(), 0)
	contains(t, "help", te.stdout.String(), "Commands:", "sites", "ekctl login --url")

	expect(t, te, te.run("help", "sites", "create"), 0)
	contains(t, "help", te.stdout.String(), "ekctl sites create DOMAIN --server SERVER", "--php VERSION", "Examples:")
	help := te.stdout.String()
	expect(t, te, te.run("sites", "create", "--help"), 0)
	if te.stdout.String() != help {
		t.Error("'help sites create' and 'sites create --help' differ")
	}
	expect(t, te, te.run("dns", "records"), 0)
	contains(t, "help", te.stdout.String(), "add", "delete", "list")

	expect(t, te, te.run("sitse"), 2)
	contains(t, "stderr", te.stderr.String(), `Did you mean "sites"?`)
	expect(t, te, te.run("sites", "list", "--bogus"), 2)
	contains(t, "stderr", te.stderr.String(), "Unknown flag -bogus", "ekctl sites list --help")
	expect(t, te, te.run("sites", "get"), 2)
	contains(t, "stderr", te.stderr.String(), "Missing SITE")
	expect(t, te, te.run("sites", "get", "a", "b"), 2)
	expect(t, te, te.run("help", "nope"), 2)

	expect(t, te, te.run("version"), 0)
	contains(t, "version", te.stdout.String(), "ekctl 1.2.3")
	expect(t, te, te.run("--version"), 0)
	expect(t, te, te.run("version", "--json"), 0)
	contains(t, "version", te.stdout.String(), `"version": "1.2.3"`)
}

func TestWhoami(t *testing.T) {
	te, _ := loggedIn(t)
	expect(t, te, te.run("whoami"), 0)
	contains(t, "stdout", te.stdout.String(), "Rakib", "rakib@example.com", "admin", "v0.2.0", "http://127.0.0.1")
	expect(t, te, te.run("whoami", "--json"), 0)
	var v struct {
		User  map[string]any    `json:"user"`
		Panel map[string]string `json:"panel"`
	}
	if err := json.Unmarshal(te.stdout.Bytes(), &v); err != nil || v.User["email"] != "rakib@example.com" || v.Panel["version"] != "v0.2.0" {
		t.Errorf("whoami --json: %v %s", err, te.stdout)
	}
}

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"panel.example.com":                     "https://panel.example.com",
		"https://Panel.Example.com/":            "https://panel.example.com",
		"https://panel.example.com:8443/api/v1": "https://panel.example.com:8443",
		"http://127.0.0.1:8080":                 "http://127.0.0.1:8080",
	} {
		if got, err := normalizeURL(in); err != nil || got != want {
			t.Errorf("normalizeURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://x", "https://user:pw@panel", "https://panel?x=1"} {
		if _, err := normalizeURL(bad); err == nil {
			t.Errorf("normalizeURL(%q) accepted", bad)
		}
	}
	if hostPort("https://panel.example.com") != "panel.example.com:443" || hostPort("http://x:81") != "x:81" {
		t.Error("hostPort")
	}
}

func TestParseInterspersed(t *testing.T) {
	te := newEnv(t)
	a := &app{env: te.Env}
	fs := a.flagSet("x")
	yes := fs.Bool("yes", false, "")
	n := fs.String("n", "", "")
	pos, err := parseInterspersed(fs, []string{"a", "--yes", "b", "-n", "v", "--", "-c", "--yes"})
	if err != nil || !*yes || *n != "v" || strings.Join(pos, ",") != "a,b,-c,--yes" {
		t.Errorf("pos %v yes %v n %q err %v", pos, *yes, *n, err)
	}
}
