package api_test

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ekpanel/internal/agentproto"
)

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// fakeSMTP accepts one connection at a time and keeps each message's DATA.
func fakeSMTP(t *testing.T) (port string, messages func() []string) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	var mu sync.Mutex
	var got []string
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				w := func(s string) { io.WriteString(c, s+"\r\n") }
				w("220 fake ESMTP")
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					cmd := strings.ToUpper(strings.TrimSpace(line))
					switch {
					case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
						w("250 fake")
					case strings.HasPrefix(cmd, "DATA"):
						w("354 go on")
						var b strings.Builder
						for {
							l, err := r.ReadString('\n')
							if err != nil || l == ".\r\n" {
								break
							}
							b.WriteString(l)
						}
						mu.Lock()
						got = append(got, b.String())
						mu.Unlock()
						w("250 queued")
					case strings.HasPrefix(cmd, "QUIT"):
						w("221 bye")
						return
					default:
						w("250 ok")
					}
				}
			}(c)
		}
	}()
	_, port, _ = net.SplitHostPort(l.Addr().String())
	return port, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

type hook struct {
	mu   sync.Mutex
	got  []map[string]any
	sigs []string
	raw  [][]byte
}

func (h *hook) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	json.Unmarshal(b, &m)
	h.mu.Lock()
	h.got, h.sigs, h.raw = append(h.got, m), append(h.sigs, r.Header.Get("X-Ekpanel-Signature")), append(h.raw, b)
	h.mu.Unlock()
}

func (h *hook) events() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, m := range h.got {
		out = append(out, m["event"].(string))
	}
	return out
}

func TestNotifications(t *testing.T) {
	p := newTestPanel(t)
	admin := newClient(t, p.web.URL)
	setUp(t, admin)
	serverID, cfg, _, _ := enrolled(t, p, admin, "web-1")
	tlsCfg, _ := cfg.TLSConfig()
	conn, _, err := dialAgent(t, p, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	fakeAgent(t, conn, func(op string, raw json.RawMessage) (any, *agentproto.OpError) {
		if op != agentproto.OpStateApply {
			return nil, &agentproto.OpError{Code: agentproto.ErrUnknownOp}
		}
		var in agentproto.StateApplyParams
		json.Unmarshal(raw, &in)
		res := agentproto.StateApplyResult{}
		for _, s := range in.Sites {
			res.Sites = append(res.Sites, agentproto.SiteResult{ID: s.ID, Generation: s.Generation, OK: false, Error: "web server rejected the configuration"})
		}
		return res, nil
	})

	// A webhook, signed with a key shown once.
	h := &hook{}
	recv := httptest.NewServer(http.HandlerFunc(h.handler))
	defer recv.Close()
	var ch struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if st := admin.do("POST", "/api/v1/notifications/channels", map[string]any{"kind": "webhook", "name": "ops", "url": recv.URL}, &ch); st != http.StatusCreated || len(ch.Key) != 48 {
		t.Fatalf("create webhook: %d %+v", st, ch)
	}
	if st := admin.do("POST", "/api/v1/notifications/channels/"+ch.ID+"/test", nil, nil); st != http.StatusNoContent {
		t.Fatalf("test: %d", st)
	}
	if ev := h.events(); len(ev) != 1 || ev[0] != "test" {
		t.Fatalf("webhook got %v", ev)
	}
	mac := hmac.New(sha256.New, []byte(ch.Key))
	mac.Write(h.raw[0])
	if h.sigs[0] != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatal("bad signature")
	}
	if title := h.got[0]["title"].(string); !strings.Contains(title, "পরীক্ষামূলক") { // the admin set Bangla at setup
		t.Errorf("test message not in the user's language: %q", title)
	}
	var channels struct {
		Channels []map[string]any `json:"channels"`
	}
	admin.do("GET", "/api/v1/notifications/channels", nil, &channels)
	if b, _ := json.Marshal(channels); strings.Contains(string(b), ch.Key) || strings.Contains(string(b), recv.URL) {
		t.Fatal("the channel list leaks its secrets")
	}

	// Email through the panel's mail server, with a Bangla subject.
	port, mails := fakeSMTP(t)
	var e errBody
	if st := admin.do("PUT", "/api/v1/settings/notifications", map[string]any{"panel_url": "https://panel.example.com",
		"smtp": map[string]any{"host": "127.0.0.1", "port": port, "from": "ekPanel <panel@example.com>", "tls": "none", "username": "u", "password": "s3cret"}}, &e); st != http.StatusBadRequest {
		t.Errorf("port as text: %d", st)
	}
	var settings map[string]any
	admin.do("PUT", "/api/v1/settings/notifications", map[string]any{"panel_url": "https://panel.example.com",
		"smtp": map[string]any{"host": "127.0.0.1", "port": atoi(port), "from": "panel@example.com", "tls": "none"}}, &settings)
	var mailCh struct{ ID string }
	admin.do("POST", "/api/v1/notifications/channels", map[string]any{"kind": "email", "address": "admin@example.com", "events": []string{"site_error"}}, &mailCh)
	if st := admin.do("POST", "/api/v1/notifications/channels/"+mailCh.ID+"/test", nil, &e); st != http.StatusNoContent {
		t.Fatalf("email test: %d %s", st, e.Code)
	}
	if m := mails(); len(m) != 1 || !strings.Contains(m[0], "Subject: =?utf-8?") || !strings.Contains(m[0], "To: admin@example.com") {
		t.Fatalf("mail %q", m)
	}

	// A customer: only their own events and channels.
	var pkg struct{ ID string }
	admin.do("POST", "/api/v1/packages", map[string]any{"name": "N", "max_sites": 1}, &pkg)
	var cust struct {
		Customer struct{ ID string } `json:"customer"`
		Password string              `json:"password"`
	}
	admin.do("POST", "/api/v1/customers", map[string]string{"email": "c@example.com", "name": "C", "package_id": pkg.ID, "locale": "en"}, &cust)
	c := login(t, p.web.URL, "c@example.com", cust.Password)
	for _, bad := range []map[string]any{
		{"kind": "webhook", "url": "http://example.com/hook"}, // plain HTTP is for administrators
		{"kind": "webhook", "url": "https://example.com/h", "events": []string{"server_offline"}},
		{"kind": "discord", "url": "https://evil.example.com/api/webhooks/1"},
		{"kind": "telegram", "chat_id": "123", "token": "not-a-token"},
	} {
		if st := c.do("POST", "/api/v1/notifications/channels", bad, &e); st != http.StatusBadRequest {
			t.Errorf("%v: %d", bad, st)
		}
	}
	for _, m := range []string{"PUT", "DELETE"} {
		if st := c.do(m, "/api/v1/notifications/channels/"+ch.ID, map[string]any{"kind": "webhook"}, nil); st != http.StatusNotFound {
			t.Errorf("customer %s admin channel: %d", m, st)
		}
	}
	if st := c.do("POST", "/api/v1/notifications/channels/"+ch.ID+"/test", nil, nil); st != http.StatusNotFound {
		t.Errorf("customer tests admin channel: %d", st)
	}
	if st := c.do("GET", "/api/v1/settings/notifications", nil, nil); st != http.StatusForbidden {
		t.Errorf("customer reads mail settings: %d", st)
	}

	// A website that fails to apply alerts its owner and the administrators, once.
	c.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "broken.example.com", "kind": "static"}, nil)
	waitFor(t, "site_error at the webhook", 15*time.Second, func() bool { return strings.Contains(strings.Join(h.events(), ","), "site_error") })
	var inbox struct {
		Notifications []struct {
			Event string `json:"event"`
			Title string `json:"title"`
			Link  string `json:"link"`
		} `json:"notifications"`
		Unread int `json:"unread"`
	}
	c.do("GET", "/api/v1/notifications", nil, &inbox)
	if len(inbox.Notifications) != 1 || inbox.Notifications[0].Title != "Website broken.example.com has a problem" ||
		!strings.HasPrefix(inbox.Notifications[0].Link, "https://panel.example.com/sites/") {
		t.Fatalf("customer inbox %+v", inbox)
	}
	waitFor(t, "email for site_error", 15*time.Second, func() bool { return len(mails()) == 2 })
	c.do("POST", "/api/v1/notifications/read", map[string]any{}, nil)
	c.do("GET", "/api/v1/notifications", nil, &inbox)
	if inbox.Unread != 0 {
		t.Errorf("unread after reading all: %d", inbox.Unread)
	}

	// Signing in from a new address.
	login(t, p.web.URL, "c@example.com", cust.Password)
	c2 := newClient(t, p.web.URL)
	if st := c2.do("POST", "/api/v1/auth/login", map[string]string{"email": "c@example.com", "password": cust.Password}, nil, "X-Forwarded-For", "203.0.113.50"); st != http.StatusOK {
		t.Fatalf("login: %d", st)
	}
	c.do("GET", "/api/v1/notifications", nil, &inbox)
	found := 0
	for _, n := range inbox.Notifications {
		if n.Event == "login_new_ip" {
			found++
		}
	}
	if found != 1 {
		t.Errorf("new sign-in alerts: %d (%+v)", found, inbox.Notifications)
	}
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
