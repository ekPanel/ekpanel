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
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pquerna/otp/totp"

	"ekpanel/internal/agent"
	"ekpanel/internal/agentproto"
	"ekpanel/internal/api"
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
		TRUNCATE users, sessions, servers, audit_log, tasks, server_metrics CASCADE;
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
