package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pquerna/otp/totp"

	"hostpanel/internal/agent"
	"hostpanel/internal/agentproto"
	"hostpanel/internal/api"
	"hostpanel/internal/db"
	"hostpanel/internal/hub"
	"hostpanel/internal/store"
)

var databaseURL string

// TestMain runs one embedded PostgreSQL for the package. The first run downloads it.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hostpanel-api-test")
	if err != nil {
		panic(err)
	}
	cache, _ := os.UserCacheDir()
	dev, err := db.StartDevDB(dir, filepath.Join(cache, "hostpanel", "embedded-postgres"), io.Discard)
	if err != nil {
		panic(err)
	}
	databaseURL = dev.URL
	code := m.Run()
	dev.Stop()
	os.RemoveAll(dir)
	os.Exit(code)
}

const setupToken = "test-setup-token"

func newPanel(t *testing.T) *httptest.Server {
	t.Helper()
	ctx := context.Background()
	pool, err := db.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// Each test starts from an empty database. The audit log is append-only, so drop its
	// trigger for the cleanup only.
	_, err = pool.Exec(ctx, `
		ALTER TABLE audit_log DISABLE TRIGGER audit_log_no_update;
		TRUNCATE users, sessions, servers, audit_log CASCADE;
		ALTER TABLE audit_log ENABLE TRIGGER audit_log_no_update;`)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(api.New(store.New(pool), hub.New(), log, api.Options{SetupToken: setupToken}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

type client struct {
	t    *testing.T
	base string
	http *http.Client
}

func newClient(t *testing.T, base string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, base: base, http: &http.Client{Jar: jar}}
}

// do sends a JSON request and decodes a JSON response into out (if non-nil).
func (c *client) do(method, path string, body any, out any, headers ...string) int {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

type errBody struct {
	Code string `json:"code"`
}

func setUp(t *testing.T, c *client) {
	t.Helper()
	status := c.do("POST", "/api/v1/setup", map[string]string{
		"token": setupToken, "email": "admin@example.com", "name": "Admin", "password": "a-long-password", "locale": "bn",
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("setup: status %d", status)
	}
}

func TestSetupAndLogin(t *testing.T) {
	srv := newPanel(t)
	c := newClient(t, srv.URL)

	var st struct {
		NeedsSetup bool `json:"needs_setup"`
	}
	c.do("GET", "/api/v1/setup/status", nil, &st)
	if !st.NeedsSetup {
		t.Fatal("fresh panel should need setup")
	}

	var e errBody
	if s := c.do("POST", "/api/v1/setup", map[string]string{"token": "wrong", "email": "a@b.co", "name": "A", "password": "a-long-password"}, &e); s != http.StatusForbidden || e.Code != "invalid_setup_token" {
		t.Fatalf("wrong token: %d %s", s, e.Code)
	}
	if s := c.do("POST", "/api/v1/setup", map[string]string{"token": setupToken, "email": "a@b.co", "name": "A", "password": "short"}, &e); s != http.StatusBadRequest || e.Code != "weak_password" {
		t.Fatalf("weak password: %d %s", s, e.Code)
	}
	setUp(t, c)
	if s := c.do("POST", "/api/v1/setup", map[string]string{"token": setupToken, "email": "x@y.co", "name": "X", "password": "a-long-password"}, &e); s != http.StatusConflict {
		t.Fatalf("second setup: %d", s)
	}

	// Setup signs the admin in.
	var me store.User
	if s := c.do("GET", "/api/v1/auth/me", nil, &me); s != http.StatusOK || me.Role != "admin" || me.Locale != "bn" {
		t.Fatalf("me after setup: %d %+v", s, me)
	}

	if s := c.do("POST", "/api/v1/auth/logout", nil, nil); s != http.StatusNoContent {
		t.Fatalf("logout: %d", s)
	}
	if s := c.do("GET", "/api/v1/auth/me", nil, nil); s != http.StatusUnauthorized {
		t.Fatalf("me after logout: %d", s)
	}

	if s := c.do("POST", "/api/v1/auth/login", map[string]string{"email": "admin@example.com", "password": "wrong-password"}, &e); s != http.StatusUnauthorized || e.Code != "invalid_credentials" {
		t.Fatalf("bad login: %d %s", s, e.Code)
	}
	if s := c.do("POST", "/api/v1/auth/login", map[string]string{"email": "nobody@example.com", "password": "whatever-long"}, &e); s != http.StatusUnauthorized || e.Code != "invalid_credentials" {
		t.Fatalf("unknown user must look like a bad password: %d %s", s, e.Code)
	}
	if s := c.do("POST", "/api/v1/auth/login", map[string]string{"email": "ADMIN@example.com", "password": "a-long-password"}, nil); s != http.StatusOK {
		t.Fatalf("login (email is case-insensitive): %d", s)
	}
}

func TestLoginRateLimit(t *testing.T) {
	srv := newPanel(t)
	c := newClient(t, srv.URL)
	setUp(t, c)
	var e errBody
	for i := 0; i < 10; i++ {
		c.do("POST", "/api/v1/auth/login", map[string]string{"email": "admin@example.com", "password": "wrong-password"}, nil)
	}
	if s := c.do("POST", "/api/v1/auth/login", map[string]string{"email": "admin@example.com", "password": "a-long-password"}, &e); s != http.StatusTooManyRequests {
		t.Fatalf("after 10 failures even the right password must wait: %d %s", s, e.Code)
	}
}

func TestTwoFactor(t *testing.T) {
	srv := newPanel(t)
	c := newClient(t, srv.URL)
	setUp(t, c)

	var setup struct {
		Secret string `json:"secret"`
		QR     string `json:"qr_png"`
	}
	if s := c.do("POST", "/api/v1/auth/2fa/setup", nil, &setup); s != http.StatusOK || setup.Secret == "" || !strings.HasPrefix(setup.QR, "data:image/png;base64,") {
		t.Fatalf("2fa setup: %d", s)
	}
	var e errBody
	if s := c.do("POST", "/api/v1/auth/2fa/enable", map[string]string{"code": "000000"}, &e); s != http.StatusBadRequest {
		t.Fatalf("enable with wrong code: %d", s)
	}
	code, _ := totp.GenerateCode(setup.Secret, time.Now())
	if s := c.do("POST", "/api/v1/auth/2fa/enable", map[string]string{"code": code}, nil); s != http.StatusOK {
		t.Fatalf("enable: %d", s)
	}

	c.do("POST", "/api/v1/auth/logout", nil, nil)
	if s := c.do("POST", "/api/v1/auth/login", map[string]string{"email": "admin@example.com", "password": "a-long-password"}, &e); s != http.StatusUnauthorized || e.Code != "totp_required" {
		t.Fatalf("login without code: %d %s", s, e.Code)
	}
	if s := c.do("GET", "/api/v1/auth/me", nil, nil); s != http.StatusUnauthorized {
		t.Fatal("password alone must not create a session when 2FA is on")
	}
	code, _ = totp.GenerateCode(setup.Secret, time.Now())
	if s := c.do("POST", "/api/v1/auth/login", map[string]string{"email": "admin@example.com", "password": "a-long-password", "totp_code": code}, nil); s != http.StatusOK {
		t.Fatalf("login with code: %d", s)
	}
}

func TestCrossSiteRequestsBlocked(t *testing.T) {
	srv := newPanel(t)
	c := newClient(t, srv.URL)
	setUp(t, c)
	var e errBody
	if s := c.do("POST", "/api/v1/servers", map[string]string{"name": "x"}, &e, "Sec-Fetch-Site", "cross-site"); s != http.StatusForbidden || e.Code != "cross_site" {
		t.Fatalf("cross-site: %d %s", s, e.Code)
	}
	if s := c.do("POST", "/api/v1/servers", map[string]string{"name": "x"}, &e, "Origin", "https://evil.example"); s != http.StatusForbidden {
		t.Fatalf("foreign origin: %d", s)
	}
}

func TestServerEnrolmentAndLiveMetrics(t *testing.T) {
	srv := newPanel(t)
	c := newClient(t, srv.URL)
	setUp(t, c)

	var created struct {
		Server struct {
			ID string `json:"id"`
		} `json:"server"`
		EnrollToken string `json:"enroll_token"`
	}
	if s := c.do("POST", "/api/v1/servers", map[string]string{"name": "web-1"}, &created); s != http.StatusCreated || created.EnrollToken == "" {
		t.Fatalf("create server: %d", s)
	}

	ctx := context.Background()
	cfgPath := filepath.Join(t.TempDir(), "agent.json")
	cfg, err := agent.Enroll(ctx, srv.URL, created.EnrollToken, cfgPath)
	if err != nil || cfg.ServerID != created.Server.ID {
		t.Fatalf("enroll: %v %+v", err, cfg)
	}
	if _, err := agent.Enroll(ctx, srv.URL, created.EnrollToken, cfgPath+".2"); err == nil {
		t.Fatal("an enrolment token must work only once")
	}

	// Wrong secret is refused.
	wsURL := strings.Replace(srv.URL, "http", "ws", 1) + agentproto.ConnectPath
	_, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + cfg.ServerID + ".wrong"}},
	})
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong agent secret accepted: %v", err)
	}

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + cfg.ServerID + "." + cfg.Secret}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	send := func(env agentproto.Envelope) {
		b, _ := json.Marshal(env)
		if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	send(agentproto.Envelope{Type: agentproto.TypeHello, Hello: &agentproto.Hello{Hostname: "web-1.local", OS: "Ubuntu 24.04 LTS", CPUCount: 4}})
	send(agentproto.Envelope{Type: agentproto.TypeMetrics, Metrics: &agentproto.Metrics{CPUPercent: 37.5, MemTotal: 8 << 30}})

	type view struct {
		ID       string              `json:"id"`
		Hostname string              `json:"hostname"`
		Online   bool                `json:"online"`
		Latest   *agentproto.Metrics `json:"latest"`
	}
	var list []view
	deadline := time.Now().Add(3 * time.Second)
	for {
		c.do("GET", "/api/v1/servers", nil, &list)
		if len(list) == 1 && list[0].Online && list[0].Latest != nil && list[0].Hostname == "web-1.local" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never showed live data: %+v", list)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if list[0].Latest.CPUPercent != 37.5 {
		t.Fatalf("latest cpu = %v", list[0].Latest.CPUPercent)
	}

	var e errBody
	if s := c.do("GET", "/api/v1/servers/not-a-uuid", nil, &e); s != http.StatusNotFound || e.Code != "server_not_found" {
		t.Fatalf("bad id: %d %s", s, e.Code)
	}

	var audit []store.AuditEntry
	c.do("GET", "/api/v1/audit", nil, &audit)
	actions := map[string]bool{}
	for _, a := range audit {
		actions[a.Action] = true
	}
	for _, want := range []string{"setup.admin_created", "server.created", "server.enrolled"} {
		if !actions[want] {
			t.Errorf("audit log missing %s; have %v", want, actions)
		}
	}
}

func TestAuditLogIsAppendOnly(t *testing.T) {
	srv := newPanel(t)
	setUp(t, newClient(t, srv.URL))
	pool, err := db.Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(context.Background(), `UPDATE audit_log SET action = 'tampered'`); err == nil {
		t.Fatal("audit rows must not be editable")
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM audit_log`); err == nil {
		t.Fatal("audit rows must not be deletable")
	}
}

func TestNonAdminEndpointsNeedLogin(t *testing.T) {
	srv := newPanel(t)
	c := newClient(t, srv.URL)
	if s := c.do("GET", "/api/v1/servers", nil, nil); s != http.StatusUnauthorized {
		t.Fatalf("servers without login: %d", s)
	}
	if s := c.do("GET", "/api/v1/nope", nil, nil); s != http.StatusNotFound {
		t.Fatalf("unknown endpoint: %d", s)
	}
}
