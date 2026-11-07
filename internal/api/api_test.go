package api_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pquerna/otp/totp"

	"ekpanel/internal/agent"
	"ekpanel/internal/agentproto"
	"ekpanel/internal/api"
	"ekpanel/internal/auth"
	"ekpanel/internal/db"
	"ekpanel/internal/hub"
	"ekpanel/internal/pki"
	"ekpanel/internal/store"
	"ekpanel/internal/tasks"
)

var databaseURL string

// TestMain runs one embedded PostgreSQL for the package. The first run downloads it.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ekpanel-api-test")
	if err != nil {
		panic(err)
	}
	cache, _ := os.UserCacheDir()
	dev, err := db.StartDevDB(dir, filepath.Join(cache, "ekpanel", "embedded-postgres"), io.Discard)
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

type testPanel struct {
	web       *httptest.Server
	agentAddr string
}

func newPanel(t *testing.T) *httptest.Server { return newTestPanel(t).web }

// newTestPanel starts the web API and the mutual-TLS agent listener against a clean
// database, with task workers running.
func newTestPanel(t *testing.T) *testPanel {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pool, err := db.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// Each test starts from an empty database. The audit log is append-only, so drop its
	// trigger for the cleanup only.
	_, err = pool.Exec(ctx, `
		ALTER TABLE audit_log DISABLE TRIGGER audit_log_no_update;
		TRUNCATE users, sessions, servers, audit_log, tasks, server_metrics, packages CASCADE;
		ALTER TABLE audit_log ENABLE TRIGGER audit_log_no_update;`)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := pki.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, h := store.New(pool), hub.New()
	runner := tasks.New(st, h, log)
	done := make(chan struct{})
	go func() { runner.Run(ctx, 2); close(done) }()

	s := api.New(st, h, log, api.Options{SetupToken: setupToken, CA: ca, AgentPort: 7443, Tasks: runner})
	web := httptest.NewServer(s.Handler())
	agents := httptest.NewUnstartedServer(s.AgentHandler())
	agents.TLS, err = ca.ServerTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	agents.StartTLS()
	t.Cleanup(func() {
		web.Close()
		agents.Close()
		cancel()
		<-done
		pool.Close()
	})
	return &testPanel{web: web, agentAddr: agents.Listener.Addr().String()}
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

// enrolled creates a server through the API and enrols an agent for it over mutual TLS.
func enrolled(t *testing.T, p *testPanel, c *client, name string) (id string, cfg agent.Config, token, pin string) {
	t.Helper()
	var created struct {
		Server struct {
			ID string `json:"id"`
		} `json:"server"`
		EnrollToken string `json:"enroll_token"`
		CAPin       string `json:"ca_pin"`
		AgentPort   int    `json:"agent_port"`
	}
	if s := c.do("POST", "/api/v1/servers", map[string]string{"name": name}, &created); s != http.StatusCreated || created.EnrollToken == "" || !strings.HasPrefix(created.CAPin, "sha256:") {
		t.Fatalf("create server: %d %+v", s, created)
	}
	cfg, err := agent.Enroll(context.Background(), p.agentAddr, created.EnrollToken, created.CAPin, filepath.Join(t.TempDir(), "agent.json"))
	if err != nil || cfg.ServerID != created.Server.ID {
		t.Fatalf("enroll: %v %+v", err, cfg)
	}
	return created.Server.ID, cfg, created.EnrollToken, created.CAPin
}

// dialAgent opens the agent WebSocket with tlsCfg and returns the HTTP status on failure.
func dialAgent(t *testing.T, p *testPanel, tlsCfg *tls.Config) (*websocket.Conn, int, error) {
	t.Helper()
	conn, resp, err := websocket.Dial(context.Background(), "wss://"+p.agentAddr+agentproto.ConnectPath, &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}},
	})
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	return conn, status, err
}

func TestAgentEnrolmentAndMutualTLS(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	ctx := context.Background()

	id, cfg, token, pin := enrolled(t, p, c, "web-1")
	if _, err := agent.Enroll(ctx, p.agentAddr, token, pin, filepath.Join(t.TempDir(), "again.json")); err == nil {
		t.Fatal("an enrolment token must work only once")
	}

	// A pin from another panel (or an attacker in the middle) is refused before the token is sent.
	other, _ := pki.LoadOrCreateCA(t.TempDir())
	var created struct {
		EnrollToken string `json:"enroll_token"`
	}
	c.do("POST", "/api/v1/servers", map[string]string{"name": "web-2"}, &created)
	if _, err := agent.Enroll(ctx, p.agentAddr, created.EnrollToken, other.Pin(), filepath.Join(t.TempDir(), "x.json")); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("wrong pin: %v", err)
	}

	// Without a client certificate the connection is refused.
	var caPEM []byte
	if _, status, err := dialAgent(t, p, pki.PinnedTLSConfig(pin, &caPEM)); err == nil || status != http.StatusUnauthorized {
		t.Fatalf("no client cert: status %d err %v", status, err)
	}

	tlsCfg, _ := cfg.TLSConfig()
	conn, _, err := dialAgent(t, p, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
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
	eventually(t, "server shows live data", func() bool {
		var v view
		c.do("GET", "/api/v1/servers/"+id, nil, &v)
		return v.Online && v.Latest != nil && v.Latest.CPUPercent == 37.5 && v.Hostname == "web-1.local"
	})

	// Closing the connection writes the partial minute to history.
	conn.Close(websocket.StatusNormalClosure, "")
	eventually(t, "metric history has the sample", func() bool {
		var h struct {
			Points []store.MetricPoint `json:"points"`
		}
		c.do("GET", "/api/v1/servers/"+id+"/metrics?range=1h", nil, &h)
		return len(h.Points) == 1 && h.Points[0].CPUPercent == 37.5
	})
	var e errBody
	if s := c.do("GET", "/api/v1/servers/"+id+"/metrics?range=1y", nil, &e); s != http.StatusBadRequest || e.Code != "invalid_range" {
		t.Fatalf("bad range: %d %s", s, e.Code)
	}

	// Asking for a new connection command revokes the current certificate at once.
	c.do("POST", "/api/v1/servers/"+id+"/enroll-token", nil, nil)
	if _, status, err := dialAgent(t, p, tlsCfg); err == nil || status != http.StatusUnauthorized {
		t.Fatalf("revoked certificate: status %d err %v", status, err)
	}

	var audit []store.AuditEntry
	c.do("GET", "/api/v1/audit", nil, &audit)
	actions := map[string]bool{}
	for _, a := range audit {
		actions[a.Action] = true
	}
	for _, want := range []string{"setup.admin_created", "server.created", "server.enrolled", "server.enroll_token_renewed"} {
		if !actions[want] {
			t.Errorf("audit log missing %s; have %v", want, actions)
		}
	}
	if s := c.do("GET", "/api/v1/servers/not-a-uuid", nil, &e); s != http.StatusNotFound || e.Code != "server_not_found" {
		t.Fatalf("bad id: %d %s", s, e.Code)
	}
}

// fakeAgent answers the panel's requests like a real agent would, using handle.
func fakeAgent(t *testing.T, conn *websocket.Conn, handle func(op string, params json.RawMessage) (any, *agentproto.OpError)) {
	go func() {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var req agentproto.Envelope
			json.Unmarshal(data, &req)
			if req.Type != agentproto.TypeRequest {
				continue
			}
			resp := agentproto.Envelope{Type: agentproto.TypeResponse, ID: req.ID}
			result, opErr := handle(req.Op, req.Params)
			resp.Error = opErr
			if opErr == nil {
				resp.Result, _ = json.Marshal(result)
			}
			b, _ := json.Marshal(resp)
			conn.Write(context.Background(), websocket.MessageText, b)
		}
	}()
}

func TestAgentOperationsAndTasks(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	id, cfg, _, _ := enrolled(t, p, c, "web-1")

	var e errBody
	if s := c.do("GET", "/api/v1/servers/"+id+"/services", nil, &e); s != http.StatusConflict || e.Code != "agent_offline" {
		t.Fatalf("services while offline: %d %s", s, e.Code)
	}

	tlsCfg, _ := cfg.TLSConfig()
	conn, _, err := dialAgent(t, p, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	restarted := make(chan string, 4)
	fakeAgent(t, conn, func(op string, params json.RawMessage) (any, *agentproto.OpError) {
		switch op {
		case agentproto.OpServicesStatus:
			return agentproto.ServicesStatusResult{Services: []agentproto.Service{{Name: "caddy", Installed: true, ActiveState: "active", Restartable: true}}}, nil
		case agentproto.OpServicesRestart:
			var p agentproto.ServicesRestartParams
			json.Unmarshal(params, &p)
			if p.Name != "caddy" {
				return nil, &agentproto.OpError{Code: agentproto.ErrNotAllowed, Message: "no"}
			}
			restarted <- p.Name
			return map[string]string{"restarted": p.Name}, nil
		}
		return nil, &agentproto.OpError{Code: agentproto.ErrUnknownOp, Message: op}
	})
	eventually(t, "agent online", func() bool {
		var v struct{ Online bool }
		c.do("GET", "/api/v1/servers/"+id, nil, &v)
		return v.Online
	})

	var svc agentproto.ServicesStatusResult
	if s := c.do("GET", "/api/v1/servers/"+id+"/services", nil, &svc); s != http.StatusOK || len(svc.Services) != 1 || svc.Services[0].ActiveState != "active" {
		t.Fatalf("services: %d %+v", s, svc)
	}

	var task store.Task
	if s := c.do("POST", "/api/v1/servers/"+id+"/services/caddy/restart", nil, &task); s != http.StatusAccepted || task.Status != "queued" {
		t.Fatalf("restart: %d %+v", s, task)
	}
	select {
	case name := <-restarted:
		if name != "caddy" {
			t.Fatalf("restarted %s", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the task never reached the agent")
	}
	eventually(t, "restart task succeeded", func() bool {
		var list []store.Task
		c.do("GET", "/api/v1/tasks?server_id="+id, nil, &list)
		return len(list) == 1 && list[0].Status == "succeeded" && list[0].ServerName != nil && *list[0].ServerName == "web-1"
	})

	// The agent refuses ssh; the task fails with the agent's reason.
	c.do("POST", "/api/v1/servers/"+id+"/services/ssh/restart", nil, &task)
	eventually(t, "refused restart fails", func() bool {
		var list []store.Task
		c.do("GET", "/api/v1/tasks?active=0&server_id="+id, nil, &list)
		for _, x := range list {
			if x.ID == task.ID {
				return x.Status == "failed" && x.Error != nil && strings.Contains(*x.Error, agentproto.ErrNotAllowed)
			}
		}
		return false
	})
	if s := c.do("POST", "/api/v1/servers/"+id+"/services/rm%20-rf/restart", nil, &e); s != http.StatusBadRequest {
		t.Fatalf("bad service name: %d", s)
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(50 * time.Millisecond)
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

func TestMetricCompactionAndTaskRecovery(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	id, _, _, _ := enrolled(t, p, c, "web-1")

	ctx := context.Background()
	pool, err := db.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	st := store.New(pool)

	// Two minutes in the same hour: 10% over 30 samples and 40% over 10 samples.
	hour := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	st.SaveMinute(ctx, id, store.MetricPoint{At: hour.Add(time.Minute), CPUPercent: 10, MemUsed: 1, MemTotal: 8}, 30)
	st.SaveMinute(ctx, id, store.MetricPoint{At: hour.Add(2 * time.Minute), CPUPercent: 40, MemUsed: 3, MemTotal: 8}, 10)
	// The same minute again (an agent that reconnected) merges by sample count.
	st.SaveMinute(ctx, id, store.MetricPoint{At: hour.Add(2 * time.Minute), CPUPercent: 40, MemUsed: 3, MemTotal: 8}, 10)
	if err := st.CompactMetrics(ctx); err != nil {
		t.Fatal(err)
	}
	points, err := st.MetricRange(ctx, id, 7*24*time.Hour, time.Hour, 3600)
	if err != nil {
		t.Fatal(err)
	}
	// (10*30 + 40*20) / 50 = 22
	if len(points) != 1 || int(points[0].CPUPercent+0.5) != 22 || points[0].MemUsed != 3 {
		t.Fatalf("hourly rollup = %+v", points)
	}

	// A task whose worker vanished is retried while attempts remain, then failed.
	task, err := st.CreateTask(ctx, store.NewTask{Kind: "test.noop", MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	// run_after in the future keeps the test's own workers from claiming it once requeued.
	pool.Exec(ctx, `UPDATE tasks SET status = 'running', attempts = 1, run_after = now() + interval '1 hour',
		locked_until = now() - interval '1 second' WHERE id = $1`, task.ID)
	st.RecoverTasks(ctx)
	if got, _ := st.GetTask(ctx, task.ID); got.Status != "queued" {
		t.Fatalf("after first crash: %s", got.Status)
	}
	pool.Exec(ctx, `UPDATE tasks SET status = 'running', attempts = 2, locked_until = now() - interval '1 second' WHERE id = $1`, task.ID)
	st.RecoverTasks(ctx)
	if got, _ := st.GetTask(ctx, task.ID); got.Status != "failed" || got.Error == nil {
		t.Fatalf("after last attempt: %+v", got)
	}
}

func TestSitesLifecycle(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	serverID, cfg, _, _ := enrolled(t, p, c, "web-1")

	// A fake agent that applies everything, except PHP 7.4 which it reports as broken.
	tlsCfg, _ := cfg.TLSConfig()
	conn, _, err := dialAgent(t, p, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	applied := make(chan agentproto.StateApplyParams, 10)
	fakeAgent(t, conn, func(op string, params json.RawMessage) (any, *agentproto.OpError) {
		var in agentproto.StateApplyParams
		json.Unmarshal(params, &in)
		applied <- in
		var out agentproto.StateApplyResult
		for _, s := range in.Sites {
			r := agentproto.SiteResult{ID: s.ID, Generation: s.Generation, OK: true}
			if s.PHPVersion == "7.4" {
				r.OK, r.Error = false, "install PHP 7.4: no candidate"
			}
			out.Sites = append(out.Sites, r)
		}
		return out, nil
	})

	type site struct {
		ID         string   `json:"id"`
		Domain     string   `json:"domain"`
		Aliases    []string `json:"aliases"`
		State      string   `json:"state"`
		Message    *string  `json:"message"`
		LinuxUser  string   `json:"linux_user"`
		Generation int64    `json:"generation"`
		PHPVersion *string  `json:"php_version"`
	}
	getSite := func(id string) (site, int) {
		var s site
		status := c.do("GET", "/api/v1/sites/"+id, nil, &s)
		return s, status
	}

	var s site
	var e errBody
	if st := c.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "Shop.Example.com",
		"aliases": []string{"www.shop.example.com", "shop.example.com"}, "kind": "php", "php_version": "8.4", "tls": "internal"}, &s); st != http.StatusCreated {
		t.Fatalf("create: %d", st)
	}
	if s.Domain != "shop.example.com" || len(s.Aliases) != 1 || !strings.HasPrefix(s.LinuxUser, "shop_") || s.State != "pending" {
		t.Fatalf("created site = %+v", s)
	}
	eventually(t, "site becomes active", func() bool { got, _ := getSite(s.ID); return got.State == "active" })

	// Every host name is unique across the panel.
	if st := c.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "www.shop.example.com", "kind": "static"}, &e); st != http.StatusConflict || e.Code != "hostname_taken" {
		t.Fatalf("duplicate host: %d %s", st, e.Code)
	}
	for _, bad := range []map[string]any{
		{"server_id": serverID, "domain": "not a domain", "kind": "static"},
		{"server_id": serverID, "domain": "ok.example.com", "kind": "php", "php_version": "5.6"},
		{"server_id": serverID, "domain": "ok.example.com", "kind": "python"},
		{"server_id": serverID, "domain": "ok.example.com", "kind": "static", "aliases": []string{"evil.com {"}},
	} {
		if st := c.do("POST", "/api/v1/sites", bad, &e); st != http.StatusBadRequest {
			t.Errorf("%v: status %d", bad, st)
		}
	}

	// A change the agent cannot apply shows the reason on the site.
	if st := c.do("PATCH", "/api/v1/sites/"+s.ID, map[string]any{"kind": "php", "php_version": "7.4", "tls": "internal"}, &s); st != http.StatusOK || s.Generation != 2 {
		t.Fatalf("update: %d %+v", st, s)
	}
	eventually(t, "site shows the error", func() bool {
		got, _ := getSite(s.ID)
		return got.State == "error" && got.Message != nil && strings.Contains(*got.Message, "7.4")
	})
	c.do("PATCH", "/api/v1/sites/"+s.ID, map[string]any{"kind": "static", "tls": "auto"}, &s)
	eventually(t, "fixed site is active again", func() bool { got, _ := getSite(s.ID); return got.State == "active" && got.PHPVersion == nil })

	// A domain in Bangla script is stored in its DNS (punycode) form.
	var bn site
	if st := c.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "উদাহরণ.বাংলা", "kind": "static"}, &bn); st != http.StatusCreated || bn.Domain != "xn--d5b6ci4b4b3a.xn--54b7fta0cc" {
		t.Fatalf("IDN site: %d %+v", st, bn)
	}

	// Deleting waits for the agent, then the site is gone and its host names are free again.
	if st := c.do("DELETE", "/api/v1/sites/"+s.ID, nil, &s); st != http.StatusAccepted || s.State != "deleting" {
		t.Fatalf("delete: %d %+v", st, s)
	}
	eventually(t, "site removed", func() bool { _, st := getSite(s.ID); return st == http.StatusNotFound })
	var last agentproto.StateApplyParams
	for len(applied) > 0 {
		last = <-applied
	}
	_ = last
	if st := c.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "shop.example.com", "kind": "static"}, &e); st != http.StatusCreated {
		t.Fatalf("host name not released after delete: %d %s", st, e.Code)
	}

	var audit []store.AuditEntry
	c.do("GET", "/api/v1/audit", nil, &audit)
	seen := map[string]bool{}
	for _, a := range audit {
		seen[a.Action] = true
	}
	for _, want := range []string{"site.created", "site.updated", "site.deleted"} {
		if !seen[want] {
			t.Errorf("audit missing %s", want)
		}
	}
}

func TestApplyFailureIsShownOnSites(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	serverID, cfg, _, _ := enrolled(t, p, c, "web-1")
	tlsCfg, _ := cfg.TLSConfig()
	conn, _, err := dialAgent(t, p, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	fakeAgent(t, conn, func(op string, _ json.RawMessage) (any, *agentproto.OpError) {
		return nil, &agentproto.OpError{Code: agentproto.ErrFailed, Message: "mkdir /etc/ekpanel: permission denied"}
	})
	var s struct {
		ID      string  `json:"id"`
		State   string  `json:"state"`
		Message *string `json:"message"`
	}
	c.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "a.example.com", "kind": "static"}, &s)
	eventually(t, "site shows the agent's error", func() bool {
		c.do("GET", "/api/v1/sites/"+s.ID, nil, &s)
		return s.State == "error" && s.Message != nil && strings.Contains(*s.Message, "permission denied")
	})
}

func TestDatabasesLifecycle(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	serverID, cfg, _, _ := enrolled(t, p, c, "db-1")
	tlsCfg, _ := cfg.TLSConfig()
	conn, _, err := dialAgent(t, p, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	hashes := make(chan string, 20)
	fakeAgent(t, conn, func(op string, params json.RawMessage) (any, *agentproto.OpError) {
		var in agentproto.StateApplyParams
		json.Unmarshal(params, &in)
		var out agentproto.StateApplyResult
		for _, d := range in.Databases {
			size := int64(4096)
			hashes <- d.PasswordHash
			out.Databases = append(out.Databases, agentproto.DatabaseResult{ID: d.ID, Generation: d.Generation, OK: true, SizeBytes: &size})
		}
		return out, nil
	})

	type db struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Username  string `json:"username"`
		Engine    string `json:"engine"`
		State     string `json:"state"`
		SizeBytes *int64 `json:"size_bytes"`
	}
	var created struct {
		Database db     `json:"database"`
		Password string `json:"password"`
	}
	if st := c.do("POST", "/api/v1/databases", map[string]string{"server_id": serverID, "engine": "mariadb", "name": "Shop"}, &created); st != http.StatusCreated {
		t.Fatalf("create: %d", st)
	}
	if created.Database.Name != "shop" || created.Database.Username != "shop" || len(created.Password) != 24 {
		t.Fatalf("created = %+v", created)
	}
	id := created.Database.ID
	eventually(t, "database active with size", func() bool {
		var d db
		c.do("GET", "/api/v1/databases/"+id, nil, &d)
		return d.State == "active" && d.SizeBytes != nil && *d.SizeBytes == 4096
	})

	// The agent receives only the MariaDB hash of the password, never the password.
	var sent string
	for len(hashes) > 0 {
		sent = <-hashes
	}
	if sent != auth.MariaDBNativeHash(created.Password) || strings.Contains(sent, created.Password) {
		t.Fatalf("agent got %q", sent)
	}
	// Nor does the API ever return the hash.
	var raw map[string]any
	c.do("GET", "/api/v1/databases/"+id, nil, &raw)
	if _, ok := raw["password_hash"]; ok {
		t.Fatal("password hash exposed by the API")
	}

	var e errBody
	for _, bad := range []map[string]string{
		{"server_id": serverID, "engine": "mariadb", "name": "shop"},                      // taken
		{"server_id": serverID, "engine": "mariadb", "name": "other", "username": "shop"}, // user taken
		{"server_id": serverID, "engine": "mariadb", "name": "mysql"},                     // reserved
		{"server_id": serverID, "engine": "mariadb", "name": "x`; DROP DATABASE y"},       // invalid
		{"server_id": serverID, "engine": "mongodb", "name": "m"},                         // engine
	} {
		if st := c.do("POST", "/api/v1/databases", bad, &e); st != http.StatusConflict && st != http.StatusBadRequest {
			t.Errorf("%v: %d %s", bad, st, e.Code)
		}
	}
	// The same name is fine on the other engine.
	var pg struct {
		Database db     `json:"database"`
		Password string `json:"password"`
	}
	if st := c.do("POST", "/api/v1/databases", map[string]string{"server_id": serverID, "engine": "postgresql", "name": "shop"}, &pg); st != http.StatusCreated {
		t.Fatalf("postgres with same name: %d", st)
	}

	var reset struct {
		Database db     `json:"database"`
		Password string `json:"password"`
	}
	if st := c.do("POST", "/api/v1/databases/"+id+"/password", nil, &reset); st != http.StatusOK || reset.Password == created.Password || reset.Database.State != "pending" {
		t.Fatalf("reset: %d %+v", st, reset)
	}
	eventually(t, "new password applied", func() bool {
		var d db
		c.do("GET", "/api/v1/databases/"+id, nil, &d)
		return d.State == "active"
	})

	if st := c.do("DELETE", "/api/v1/databases/"+id, nil, nil); st != http.StatusAccepted {
		t.Fatalf("delete: %d", st)
	}
	eventually(t, "database removed", func() bool {
		return c.do("GET", "/api/v1/databases/"+id, nil, nil) == http.StatusNotFound
	})
}

func TestSFTPAndFiles(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	serverID, cfg, _, _ := enrolled(t, p, c, "web-1")
	tlsCfg, _ := cfg.TLSConfig()
	conn, _, err := dialAgent(t, p, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	specs := make(chan agentproto.SiteSpec, 50)
	fileCalls := make(chan agentproto.FilesParams, 50)
	fakeAgent(t, conn, func(op string, params json.RawMessage) (any, *agentproto.OpError) {
		switch op {
		case agentproto.OpStateApply:
			var in agentproto.StateApplyParams
			json.Unmarshal(params, &in)
			var out agentproto.StateApplyResult
			for _, s := range in.Sites {
				specs <- s
				out.Sites = append(out.Sites, agentproto.SiteResult{ID: s.ID, Generation: s.Generation, OK: true})
			}
			return out, nil
		case agentproto.OpFiles:
			var f agentproto.FilesParams
			json.Unmarshal(params, &f)
			fileCalls <- f
			switch f.Action {
			case "list":
				return agentproto.FilesResult{Entries: []agentproto.FileEntry{{Name: "index.html", Type: "file", Size: 5}}}, nil
			case "read_chunk": // a 6-byte file served in two pieces
				if f.Offset == 0 {
					return agentproto.FilesResult{Data: []byte("hel"), Size: 6}, nil
				}
				return agentproto.FilesResult{Data: []byte("lo!"), Size: 6, EOF: true}, nil
			case "write_chunk":
				return agentproto.FilesResult{Size: f.Offset + int64(len(f.Data))}, nil
			}
		}
		return nil, &agentproto.OpError{Code: agentproto.ErrUnknownOp}
	})

	var site struct {
		ID string `json:"id"`
	}
	c.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "files.example.com", "kind": "static"}, &site)
	lastSpec := func() agentproto.SiteSpec {
		var last agentproto.SiteSpec
		deadline := time.After(5 * time.Second)
		for {
			select {
			case s := <-specs:
				last = s
				if len(specs) == 0 {
					return last
				}
			case <-deadline:
				t.Fatal("no state.apply")
			}
		}
	}

	// SFTP on: a password is returned once; the agent receives only its crypt hash.
	var on struct {
		Password string `json:"password"`
		Site     struct {
			SFTPEnabled bool `json:"sftp_enabled"`
		} `json:"site"`
	}
	if st := c.do("POST", "/api/v1/sites/"+site.ID+"/sftp", map[string]bool{"enabled": true}, &on); st != http.StatusOK || len(on.Password) != 24 || !on.Site.SFTPEnabled {
		t.Fatalf("enable sftp: %d %+v", st, on)
	}
	spec := lastSpec()
	for spec.SFTPPasswordHash == "" {
		spec = lastSpec()
	}
	if !spec.SFTPEnabled || !auth.CheckLinuxPassword(spec.SFTPPasswordHash, on.Password) || strings.Contains(spec.SFTPPasswordHash, on.Password) {
		t.Fatalf("agent got %+v", spec)
	}
	var raw map[string]any
	c.do("GET", "/api/v1/sites/"+site.ID, nil, &raw)
	if _, leaked := raw["sftp_password_hash"]; leaked {
		t.Fatal("password hash exposed by the API")
	}

	var e errBody
	if st := c.do("PUT", "/api/v1/sites/"+site.ID+"/ssh-keys", map[string]any{"keys": []string{"ssh-dss AAAA bad"}}, &e); st != http.StatusBadRequest || e.Code != "invalid_ssh_key" {
		t.Fatalf("bad key: %d %s", st, e.Code)
	}
	key := `no-pty ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFJiNUh2lSu0HKrMBqDhBTLTtc3P6gVjsyhP6UkCQBkW me@laptop`
	var withKeys struct {
		SSHKeys []string `json:"ssh_keys"`
	}
	if st := c.do("PUT", "/api/v1/sites/"+site.ID+"/ssh-keys", map[string]any{"keys": []string{key}}, &withKeys); st != http.StatusOK || len(withKeys.SSHKeys) != 1 || strings.Contains(withKeys.SSHKeys[0], "no-pty") {
		t.Fatalf("keys: %d %+v", st, withKeys)
	}

	// SFTP off forgets the password.
	c.do("POST", "/api/v1/sites/"+site.ID+"/sftp", map[string]bool{"enabled": false}, nil)
	spec = lastSpec()
	for spec.SFTPEnabled {
		spec = lastSpec()
	}
	if spec.SFTPPasswordHash != "" {
		t.Fatal("disabling SFTP must clear the password")
	}

	// Files: listing, the site's own account, and actions the JSON endpoint refuses.
	var list agentproto.FilesResult
	if st := c.do("POST", "/api/v1/sites/"+site.ID+"/files", map[string]any{"action": "list", "path": "/public_html"}, &list); st != http.StatusOK || len(list.Entries) != 1 {
		t.Fatalf("list: %d %+v", st, list)
	}
	if call := <-fileCalls; !strings.HasPrefix(call.User, "files_") {
		t.Fatalf("file action must run as the site's account, got %q", call.User)
	}
	if st := c.do("POST", "/api/v1/sites/"+site.ID+"/files", map[string]any{"action": "write_chunk", "path": "/x"}, &e); st != http.StatusBadRequest {
		t.Fatalf("chunk action through JSON endpoint: %d", st)
	}

	// Upload a piece as raw bytes; a text/plain body (what a cross-site form can send) is refused.
	req, _ := http.NewRequest("POST", p.web.URL+"/api/v1/sites/"+site.ID+"/files/upload?path=/public_html/a.bin&offset=0&final=1", strings.NewReader("abc"))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("upload: %v %d", err, resp.StatusCode)
	}
	resp.Body.Close()
	if call := <-fileCalls; call.Action != "write_chunk" || string(call.Data) != "abc" || !call.Final {
		t.Fatalf("upload reached agent as %+v", call)
	}
	req, _ = http.NewRequest("POST", p.web.URL+"/api/v1/sites/"+site.ID+"/files/upload?path=/x&offset=0", strings.NewReader("abc"))
	req.Header.Set("Content-Type", "text/plain")
	resp, _ = c.http.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain upload: %d", resp.StatusCode)
	}

	// Download is streamed from the agent in pieces.
	resp, err = c.http.Get(p.web.URL + "/api/v1/sites/" + site.ID + "/files/download?path=/public_html/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello!" || !strings.Contains(resp.Header.Get("Content-Disposition"), `filename=hello.txt`) {
		t.Fatalf("download = %q %q", body, resp.Header.Get("Content-Disposition"))
	}
}

func TestWordPressInstall(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	serverID, cfg, _, _ := enrolled(t, p, c, "web-1")
	tlsCfg, _ := cfg.TLSConfig()
	conn, _, err := dialAgent(t, p, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	dbHashes := map[string]string{}
	var mu sync.Mutex
	wpCalls := make(chan agentproto.WordPressInstallParams, 2)
	fakeAgent(t, conn, func(op string, params json.RawMessage) (any, *agentproto.OpError) {
		switch op {
		case agentproto.OpStateApply:
			var in agentproto.StateApplyParams
			json.Unmarshal(params, &in)
			var out agentproto.StateApplyResult
			for _, s := range in.Sites {
				out.Sites = append(out.Sites, agentproto.SiteResult{ID: s.ID, Generation: s.Generation, OK: true})
			}
			mu.Lock()
			for _, d := range in.Databases {
				dbHashes[d.Name] = d.PasswordHash
				out.Databases = append(out.Databases, agentproto.DatabaseResult{ID: d.ID, Generation: d.Generation, OK: true})
			}
			mu.Unlock()
			return out, nil
		case agentproto.OpWordPressInstall:
			var in agentproto.WordPressInstallParams
			json.Unmarshal(params, &in)
			wpCalls <- in
			return agentproto.WordPressInstallResult{Version: "6.9"}, nil
		}
		return nil, &agentproto.OpError{Code: agentproto.ErrUnknownOp}
	})

	var static, site struct {
		ID         string  `json:"id"`
		LinuxUser  string  `json:"linux_user"`
		App        *string `json:"app"`
		AppVersion *string `json:"app_version"`
	}
	c.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "static.example.com", "kind": "static"}, &static)
	var e errBody
	if st := c.do("POST", "/api/v1/sites/"+static.ID+"/wordpress", map[string]any{"admin_user": "admin"}, &e); st != http.StatusConflict || e.Code != "wordpress_needs_php" {
		t.Fatalf("static site: %d %s", st, e.Code)
	}

	c.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "blog.example.com", "kind": "php", "php_version": "8.4"}, &site)
	if st := c.do("POST", "/api/v1/sites/"+site.ID+"/wordpress", map[string]any{"admin_user": "admin --role=evil"}, &e); st != http.StatusBadRequest {
		t.Fatalf("bad admin user: %d", st)
	}
	var started struct {
		Task          store.Task `json:"task"`
		AdminPassword string     `json:"admin_password"`
		AdminURL      string     `json:"admin_url"`
	}
	if st := c.do("POST", "/api/v1/sites/"+site.ID+"/wordpress", map[string]any{"admin_user": "owner", "title": "আমার ব্লগ", "locale": "bn_BD"}, &started); st != http.StatusAccepted || len(started.AdminPassword) != 24 {
		t.Fatalf("start: %d %+v", st, started)
	}
	if started.AdminURL != "https://blog.example.com/wp-admin/" {
		t.Fatalf("admin url %s", started.AdminURL)
	}

	var call agentproto.WordPressInstallParams
	select {
	case call = <-wpCalls:
	case <-time.After(10 * time.Second):
		t.Fatal("install never reached the agent")
	}
	wantDB := "wp_" + site.LinuxUser
	mu.Lock()
	dbHash := dbHashes[wantDB]
	mu.Unlock()
	if call.AdminPassword != started.AdminPassword || call.DBName != wantDB || call.Title != "আমার ব্লগ" || call.Locale != "bn_BD" || call.AdminEmail != "admin@example.com" {
		t.Fatalf("agent got %+v", call)
	}
	if dbHash != auth.MariaDBNativeHash(call.DBPassword) {
		t.Fatal("the database password given to WordPress must match the database's hash")
	}

	eventually(t, "install task succeeded and site marked", func() bool {
		c.do("GET", "/api/v1/sites/"+site.ID, nil, &site)
		return site.App != nil && *site.App == "wordpress" && site.AppVersion != nil && *site.AppVersion == "6.9"
	})
	// No password is ever stored in the task.
	var list []map[string]any
	c.do("GET", "/api/v1/tasks?server_id="+serverID, nil, &list)
	for _, task := range list {
		b, _ := json.Marshal(task)
		if strings.Contains(string(b), started.AdminPassword) || strings.Contains(string(b), call.DBPassword) {
			t.Fatalf("password stored in task %s", b)
		}
	}
	if st := c.do("POST", "/api/v1/sites/"+site.ID+"/wordpress", map[string]any{"admin_user": "owner"}, &e); st != http.StatusConflict || e.Code != "app_installed" {
		t.Fatalf("second install: %d %s", st, e.Code)
	}
}

func TestBackups(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	serverID, cfg, _, _ := enrolled(t, p, c, "web-1")
	tlsCfg, _ := cfg.TLSConfig()
	conn, _, err := dialAgent(t, p, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	runs := make(chan agentproto.BackupRunParams, 5)
	restores := make(chan agentproto.BackupRestoreParams, 5)
	removed := make(chan string, 1)
	var failDB atomic.Bool
	fakeAgent(t, conn, func(op string, params json.RawMessage) (any, *agentproto.OpError) {
		switch op {
		case agentproto.OpStateApply:
			var in agentproto.StateApplyParams
			json.Unmarshal(params, &in)
			var out agentproto.StateApplyResult
			for _, s := range in.Sites {
				out.Sites = append(out.Sites, agentproto.SiteResult{ID: s.ID, Generation: s.Generation, OK: true})
			}
			for _, d := range in.Databases {
				out.Databases = append(out.Databases, agentproto.DatabaseResult{ID: d.ID, Generation: d.Generation, OK: true})
			}
			return out, nil
		case agentproto.OpBackupSetup:
			var in agentproto.BackupSetupParams
			json.Unmarshal(params, &in)
			return agentproto.BackupSetupResult{RecoveryKey: "RecoveryKeyRecoveryKeyRecover", Location: "/var/backups/ekpanel/" + in.RepoID}, nil
		case agentproto.OpBackupRun:
			var in agentproto.BackupRunParams
			json.Unmarshal(params, &in)
			runs <- in
			var res agentproto.BackupRunResult
			for _, s := range in.Sites {
				res.Items = append(res.Items, agentproto.BackupItemResult{Kind: "site", ID: s.ID, Name: s.Name, OK: true})
			}
			for _, d := range in.Databases {
				res.Items = append(res.Items, agentproto.BackupItemResult{Kind: "database", ID: d.ID, Name: d.Name, OK: !failDB.Load(), Error: map[bool]string{true: "dump failed"}[failDB.Load()]})
			}
			return res, nil
		case agentproto.OpBackupRestore:
			var in agentproto.BackupRestoreParams
			json.Unmarshal(params, &in)
			restores <- in
			return agentproto.BackupRestoreResult{Path: "/restored-x"}, nil
		case agentproto.OpBackupRemove:
			var in agentproto.BackupRemoveParams
			json.Unmarshal(params, &in)
			removed <- in.RepoID
			return agentproto.BackupRestoreResult{}, nil
		}
		return nil, &agentproto.OpError{Code: agentproto.ErrUnknownOp}
	})

	var site, db struct {
		ID string `json:"id"`
	}
	c.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "shop.example.com", "kind": "static"}, &site)
	var dbc struct {
		Database struct{ ID string } `json:"database"`
	}
	c.do("POST", "/api/v1/databases", map[string]string{"server_id": serverID, "engine": "mariadb", "name": "shop"}, &dbc)
	db.ID = dbc.Database.ID

	var e errBody
	if st := c.do("POST", "/api/v1/backups", map[string]any{"server_id": serverID, "name": "x", "kind": "local", "schedule": "daily",
		"keep_daily": 0, "keep_weekly": 0, "keep_monthly": 0}, &e); st != http.StatusBadRequest || e.Code != "invalid_retention" {
		t.Fatalf("no retention: %d %s", st, e.Code)
	}
	if st := c.do("POST", "/api/v1/backups", map[string]any{"server_id": serverID, "name": "x", "kind": "local", "schedule": "daily",
		"keep_daily": 7, "recovery_key": "AAAAAAAAAAAAAAAAAAAA"}, &e); st != http.StatusBadRequest || e.Code != "connect_needs_s3" {
		t.Fatalf("connect a local location: %d %s", st, e.Code)
	}
	var created struct {
		Repo struct {
			ID        string     `json:"id"`
			Location  string     `json:"location"`
			NextRunAt *time.Time `json:"next_run_at"`
		} `json:"repo"`
		RecoveryKey string `json:"recovery_key"`
	}
	if st := c.do("POST", "/api/v1/backups", map[string]any{"server_id": serverID, "name": "On this server", "kind": "local",
		"schedule": "daily", "hour_utc": 2, "keep_daily": 7, "keep_weekly": 4, "keep_monthly": 6}, &created); st != http.StatusCreated {
		t.Fatalf("create: %d", st)
	}
	if created.RecoveryKey == "" || !strings.HasSuffix(created.Repo.Location, created.Repo.ID) || created.Repo.NextRunAt == nil || created.Repo.NextRunAt.UTC().Hour() != 2 {
		t.Fatalf("created = %+v", created)
	}
	repo := created.Repo.ID

	// Back up now: every site and database on the server is included.
	c.do("POST", "/api/v1/backups/"+repo+"/run", nil, nil)
	var run agentproto.BackupRunParams
	select {
	case run = <-runs:
	case <-time.After(5 * time.Second):
		t.Fatal("backup never reached the agent")
	}
	if len(run.Sites) != 1 || len(run.Databases) != 1 || run.KeepDaily != 7 || run.Sites[0].Name != "shop.example.com" || run.Databases[0].Owner != "shop" {
		t.Fatalf("run = %+v", run)
	}
	type repoView struct {
		LastStatus  *string `json:"last_status"`
		LastMessage *string `json:"last_message"`
	}
	eventually(t, "run recorded", func() bool {
		var v repoView
		c.do("GET", "/api/v1/backups/"+repo, nil, &v)
		return v.LastStatus != nil && *v.LastStatus == "succeeded"
	})
	failDB.Store(true)
	c.do("POST", "/api/v1/backups/"+repo+"/run", nil, nil)
	<-runs
	eventually(t, "partial run recorded", func() bool {
		var v repoView
		c.do("GET", "/api/v1/backups/"+repo, nil, &v)
		return v.LastStatus != nil && *v.LastStatus == "partial" && v.LastMessage != nil && strings.Contains(*v.LastMessage, "shop: dump failed")
	})

	// Restores go to the right item and mode.
	if st := c.do("POST", "/api/v1/backups/"+repo+"/restore", map[string]string{"snapshot_id": "abcd1234; rm -rf /", "database_id": db.ID}, &e); st != http.StatusBadRequest || e.Code != "invalid_restore" {
		t.Fatalf("bad snapshot id: %d %s", st, e.Code)
	}
	if st := c.do("POST", "/api/v1/backups/"+repo+"/restore", map[string]string{"snapshot_id": "abcd1234", "site_id": site.ID, "mode": "everything"}, &e); st != http.StatusBadRequest {
		t.Fatalf("bad mode: %d", st)
	}
	c.do("POST", "/api/v1/backups/"+repo+"/restore", map[string]string{"snapshot_id": "abcd1234", "site_id": site.ID, "mode": "replace"}, nil)
	if r := <-restores; r.Site == nil || r.Site.ID != site.ID || r.Mode != "replace" || r.SnapshotID != "abcd1234" {
		t.Fatalf("site restore = %+v", r)
	}
	c.do("POST", "/api/v1/backups/"+repo+"/restore", map[string]string{"snapshot_id": "abcd1234", "database_id": db.ID}, nil)
	if r := <-restores; r.Database == nil || r.Database.Name != "shop" {
		t.Fatalf("database restore = %+v", r)
	}

	if st := c.do("DELETE", "/api/v1/backups/"+repo, nil, nil); st != http.StatusNoContent {
		t.Fatalf("delete: %d", st)
	}
	if id := <-removed; id != repo {
		t.Fatalf("removed %s", id)
	}
}

func TestFirewall(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	serverID, cfg, _, _ := enrolled(t, p, c, "web-1")
	tlsCfg, _ := cfg.TLSConfig()
	conn, _, err := dialAgent(t, p, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	applied := make(chan *agentproto.FirewallSpec, 10)
	fakeAgent(t, conn, func(op string, raw json.RawMessage) (any, *agentproto.OpError) {
		if op != agentproto.OpStateApply {
			return nil, &agentproto.OpError{Code: agentproto.ErrUnknownOp}
		}
		var in agentproto.StateApplyParams
		json.Unmarshal(raw, &in)
		applied <- in.Firewall
		res := agentproto.StateApplyResult{}
		if in.Firewall != nil {
			res.Firewall = &agentproto.FirewallResult{Active: in.Firewall.Enabled, SSHPorts: []int{22, 2222}}
		}
		return res, nil
	})
	next := func() *agentproto.FirewallSpec {
		t.Helper()
		for {
			select {
			case fw := <-applied:
				if fw != nil || len(applied) == 0 {
					return fw
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no state.apply")
			}
		}
	}
	base := "/api/v1/servers/" + serverID + "/firewall"
	var fw struct {
		Mode   string `json:"mode"`
		Status *struct {
			Active   bool  `json:"active"`
			SSHPorts []int `json:"ssh_ports"`
		} `json:"status"`
		Rules []struct {
			ID     string `json:"id"`
			Source string `json:"source"`
		} `json:"rules"`
	}
	c.do("GET", base, nil, &fw)
	if fw.Mode != "unmanaged" || fw.Status != nil {
		t.Fatalf("new server firewall = %+v", fw)
	}

	c.do("PUT", base, map[string]string{"mode": "on"}, nil)
	if spec := next(); spec == nil || !spec.Enabled || len(spec.Rules) != 0 {
		t.Fatalf("turning on sent %+v", spec)
	}
	eventually(t, "status recorded", func() bool {
		c.do("GET", base, nil, &fw)
		return fw.Status != nil && fw.Status.Active && len(fw.Status.SSHPorts) == 2
	})

	var e errBody
	for _, bad := range []struct {
		body map[string]string
		code string
	}{
		{map[string]string{"action": "allow", "protocol": "tcp", "ports": ""}, "invalid_ports"},
		{map[string]string{"action": "allow", "protocol": "tcp", "ports": "70000"}, "invalid_ports"},
		{map[string]string{"action": "allow", "protocol": "tcp", "ports": "90-80"}, "invalid_ports"},
		{map[string]string{"action": "allow", "protocol": "icmp", "ports": "80"}, "invalid_protocol"},
		{map[string]string{"action": "deny", "protocol": "both", "source": "not-an-ip"}, "invalid_source"},
		{map[string]string{"action": "deny", "protocol": "both", "source": "127.0.0.0/8"}, "firewall_blocks_you"},
	} {
		if st := c.do("POST", base+"/rules", bad.body, &e); st != http.StatusBadRequest || e.Code != bad.code {
			t.Errorf("%v: %d %s, want %s", bad.body, st, e.Code, bad.code)
		}
	}
	c.do("POST", base+"/rules", map[string]string{"action": "allow", "protocol": "tcp", "ports": "5432", "source": "10.1.2.3/8", "note": "app servers"}, &fw)
	if len(fw.Rules) != 1 || fw.Rules[0].Source != "10.0.0.0/8" {
		t.Fatalf("rules = %+v", fw.Rules)
	}
	if spec := next(); spec == nil || len(spec.Rules) != 1 || spec.Rules[0].PortFrom != 5432 || spec.Rules[0].PortTo != 5432 {
		t.Fatalf("rule sent as %+v", spec)
	}
	c.do("DELETE", base+"/rules/"+fw.Rules[0].ID, nil, &fw)
	if spec := next(); len(fw.Rules) != 0 || spec == nil || len(spec.Rules) != 0 {
		t.Fatalf("after delete: %+v / %+v", fw.Rules, spec)
	}

	c.do("PUT", base, map[string]string{"mode": "unmanaged"}, nil)
	if spec := next(); spec != nil {
		t.Fatalf("unmanaged must send no firewall, sent %+v", spec)
	}
}

func TestApps(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	serverID, cfg, _, _ := enrolled(t, p, c, "web-1")
	tlsCfg, _ := cfg.TLSConfig()
	conn, _, err := dialAgent(t, p, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	applied := make(chan []agentproto.AppSpec, 20)
	fakeAgent(t, conn, func(op string, raw json.RawMessage) (any, *agentproto.OpError) {
		switch op {
		case agentproto.OpStateApply:
			var in agentproto.StateApplyParams
			json.Unmarshal(raw, &in)
			applied <- in.Apps
			res := agentproto.StateApplyResult{}
			for _, a := range in.Apps {
				res.Apps = append(res.Apps, agentproto.AppResult{ID: a.ID, Generation: a.Generation, OK: true})
			}
			return res, nil
		case agentproto.OpAppLogs:
			var in agentproto.AppParams
			json.Unmarshal(raw, &in)
			return agentproto.AppLogsResult{Logs: in.Name + " says hello"}, nil
		}
		return nil, &agentproto.OpError{Code: agentproto.ErrUnknownOp}
	})
	lastApps := func(name string) agentproto.AppSpec {
		t.Helper()
		for {
			select {
			case apps := <-applied:
				for _, a := range apps {
					if a.Name == name && len(applied) == 0 {
						return a
					}
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("no state.apply with %s", name)
			}
		}
	}
	type appView struct {
		ID            string  `json:"id"`
		HostPort      *int    `json:"host_port"`
		State         string  `json:"state"`
		Domain        *string `json:"domain"`
		DomainDisplay string  `json:"domain_display"`
	}
	var e errBody
	base := map[string]any{"server_id": serverID, "name": "uptime", "kind": "image", "image": "louislam/uptime-kuma:1"}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for a, b := range base {
			m[a] = b
		}
		m[k] = v
		return m
	}
	for _, bad := range []struct {
		body map[string]any
		code string
	}{
		{with("name", "Bad Name"), "invalid_app_name"},
		{with("image", "nginx latest"), "invalid_image"},
		{with("domain", "status.example.com"), "domain_needs_port"},
		{with("env", []map[string]string{{"key": "1BAD", "value": "x"}}), "invalid_env"},
		{with("env", []map[string]string{{"key": "A", "value": "1"}, {"key": "A", "value": "2"}}), "invalid_env"},
		{with("volumes", []string{"relative/path"}), "invalid_volume"},
		{with("kind", "vm"), "invalid_app_kind"},
		{with("container_port", 70000), "invalid_port"},
		{with("memory_mb", 1), "invalid_limits"},
	} {
		if st := c.do("POST", "/api/v1/apps", bad.body, &e); st != http.StatusBadRequest || e.Code != bad.code {
			t.Errorf("%v: %d %s, want %s", bad.body, st, e.Code, bad.code)
		}
	}

	good := with("container_port", 3001)
	good["domain"] = "স্ট্যাটাস.example.com"
	good["env"] = []map[string]string{{"key": "TZ", "value": "Asia/Dhaka"}}
	good["volumes"] = []string{"/app/data/", "/app/data"}
	var app appView
	if st := c.do("POST", "/api/v1/apps", good, &app); st != http.StatusCreated || app.HostPort == nil || *app.HostPort != 20000 {
		t.Fatalf("create: %d %+v", st, app)
	}
	if !strings.HasPrefix(*app.Domain, "xn--") || app.DomainDisplay != "স্ট্যাটাস.example.com" {
		t.Fatalf("domain %v / %s", *app.Domain, app.DomainDisplay)
	}
	spec := lastApps("uptime")
	if spec.HostPort != 20000 || spec.ContainerPort != 3001 || spec.Service != agentproto.AppServiceName || len(spec.Volumes) != 1 || spec.Env[0].Value != "Asia/Dhaka" {
		t.Fatalf("spec %+v", spec)
	}
	eventually(t, "app active", func() bool {
		c.do("GET", "/api/v1/apps/"+app.ID, nil, &app)
		return app.State == "active"
	})

	if st := c.do("POST", "/api/v1/apps", good, &e); st != http.StatusConflict || e.Code != "app_name_taken" {
		t.Errorf("same name: %d %s", st, e.Code)
	}
	c.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "blog.example.com", "kind": "static"}, nil)
	clash := with("name", "blog")
	clash["container_port"], clash["domain"] = 80, "blog.example.com"
	if st := c.do("POST", "/api/v1/apps", clash, &e); st != http.StatusConflict || e.Code != "hostname_taken" {
		t.Errorf("domain of a site: %d %s", st, e.Code)
	}

	var second appView
	compose := map[string]any{"server_id": serverID, "name": "wiki", "kind": "compose", "compose": "services:\n  web:\n    image: nginx\n",
		"service": "web", "container_port": 80}
	if st := c.do("POST", "/api/v1/apps", compose, &second); st != http.StatusCreated || *second.HostPort != 20001 {
		t.Fatalf("second app: %d %+v", st, second)
	}
	if spec := lastApps("wiki"); spec.Kind != "compose" || spec.Service != "web" || spec.HostPort != 20001 {
		t.Fatalf("compose spec %+v", spec)
	}

	// Changing settings keeps the port.
	good["env"] = []map[string]string{{"key": "TZ", "value": "UTC"}}
	c.do("PUT", "/api/v1/apps/"+app.ID, good, &app)
	if spec := lastApps("uptime"); spec.HostPort != 20000 || spec.Env[0].Value != "UTC" || spec.Generation != 2 {
		t.Fatalf("after update %+v", spec)
	}
	c.do("POST", "/api/v1/apps/"+app.ID+"/stop", nil, nil)
	if spec := lastApps("uptime"); !spec.Stopped {
		t.Fatal("stop not sent")
	}
	var logs agentproto.AppLogsResult
	if st := c.do("GET", "/api/v1/apps/"+app.ID+"/logs?tail=50", nil, &logs); st != http.StatusOK || logs.Logs != "uptime says hello" {
		t.Fatalf("logs: %d %+v", st, logs)
	}

	c.do("DELETE", "/api/v1/apps/"+app.ID+"?data=delete", nil, nil)
	if spec := lastApps("uptime"); !spec.Deleted || !spec.DeleteData {
		t.Fatalf("delete spec %+v", spec)
	}
	eventually(t, "app removed", func() bool {
		return c.do("GET", "/api/v1/apps/"+app.ID, nil, nil) == http.StatusNotFound
	})
	// Its port and domain are free again.
	c.do("POST", "/api/v1/apps", with("name", "again"), nil)
}
