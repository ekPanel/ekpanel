package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"ekpanel/internal/agentproto"
)

// terminalAgent answers terminal.open, echoes keystrokes back as "echo:<input>", and ends a
// stream when it receives "exit".
type terminalAgent struct {
	mu      sync.Mutex
	opened  []agentproto.TerminalParams
	resized []int
	closed  []string
}

func (fa *terminalAgent) run(t *testing.T, conn *websocket.Conn) {
	write := func(env agentproto.Envelope) {
		b, _ := json.Marshal(env)
		conn.Write(context.Background(), websocket.MessageText, b)
	}
	go func() {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var env agentproto.Envelope
			json.Unmarshal(data, &env)
			switch env.Type {
			case agentproto.TypeRequest:
				resp := agentproto.Envelope{Type: agentproto.TypeResponse, ID: env.ID, Result: json.RawMessage("{}")}
				if env.Op == agentproto.OpTerminalOpen {
					var p agentproto.TerminalParams
					json.Unmarshal(env.Params, &p)
					fa.mu.Lock()
					fa.opened = append(fa.opened, p)
					fa.mu.Unlock()
					write(resp)
					write(agentproto.Envelope{Type: agentproto.TypeStreamData, Stream: p.Stream, Data: []byte("root@web-1:~# ")})
					continue
				}
				write(resp)
			case agentproto.TypeStreamData:
				if strings.TrimSpace(string(env.Data)) == "exit" {
					write(agentproto.Envelope{Type: agentproto.TypeStreamClose, Stream: env.Stream, Reason: "exit"})
					continue
				}
				write(agentproto.Envelope{Type: agentproto.TypeStreamData, Stream: env.Stream, Data: append([]byte("echo:"), env.Data...)})
			case agentproto.TypeStreamResize:
				fa.mu.Lock()
				fa.resized = append(fa.resized, env.Cols, env.Rows)
				fa.mu.Unlock()
			case agentproto.TypeStreamClose:
				fa.mu.Lock()
				fa.closed = append(fa.closed, env.Stream)
				fa.mu.Unlock()
			}
		}
	}()
}

func dialTerminal(t *testing.T, c *client, path string) (*websocket.Conn, int) {
	t.Helper()
	conn, resp, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(c.base, "http")+path, &websocket.DialOptions{HTTPClient: c.http})
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	if err != nil {
		return nil, status
	}
	return conn, status
}

func readUntil(t *testing.T, conn *websocket.Conn, want string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got strings.Builder
	for !strings.Contains(got.String(), want) {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %q, got %q: %v", want, got.String(), err)
		}
		got.Write(data)
	}
	return got.String()
}

func TestTerminals(t *testing.T) {
	p := newTestPanel(t)
	admin := newClient(t, p.web.URL)
	setUp(t, admin)
	serverID, cfg, _, _ := enrolled(t, p, admin, "web-1")
	tlsCfg, _ := cfg.TLSConfig()
	agentConn, _, err := dialAgent(t, p, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer agentConn.CloseNow()
	fa := &terminalAgent{}
	fa.run(t, agentConn)
	path := "/api/v1/servers/" + serverID + "/terminal?cols=120&rows=40"

	// Not without the password again.
	var e errBody
	if st := admin.do("GET", path, nil, &e); st != http.StatusForbidden || e.Code != "confirm_required" {
		t.Fatalf("unconfirmed: %d %s", st, e.Code)
	}
	if st := admin.do("POST", "/api/v1/auth/confirm", map[string]string{"password": "wrong"}, &e); st != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", st)
	}
	if st := admin.do("POST", "/api/v1/auth/confirm", map[string]string{"password": "a-long-password"}, nil); st != http.StatusOK {
		t.Fatalf("confirm: %d", st)
	}

	conn, st := dialTerminal(t, admin, path)
	if conn == nil {
		t.Fatalf("dial: %d", st)
	}
	readUntil(t, conn, "root@web-1:~# ")
	conn.Write(context.Background(), websocket.MessageBinary, []byte("uptime\n"))
	readUntil(t, conn, "echo:uptime")
	conn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"resize","cols":90,"rows":20}`))
	conn.Write(context.Background(), websocket.MessageBinary, []byte("exit\n"))
	if got := readUntil(t, conn, `"type":"exit"`); !strings.Contains(got, `"reason":"exit"`) {
		t.Fatalf("exit message %q", got)
	}
	fa.mu.Lock()
	if len(fa.opened) != 1 || fa.opened[0].Kind != "server" || fa.opened[0].Cols != 120 || fa.opened[0].Rows != 40 {
		t.Errorf("opened %+v", fa.opened)
	}
	if len(fa.resized) != 2 || fa.resized[0] != 90 {
		t.Errorf("resized %v", fa.resized)
	}
	fa.mu.Unlock()

	// A browser that goes away closes the shell on the server.
	conn, _ = dialTerminal(t, admin, path)
	readUntil(t, conn, "root@web-1")
	conn.CloseNow()
	eventually(t, "agent told to close", func() bool {
		fa.mu.Lock()
		defer fa.mu.Unlock()
		return len(fa.closed) == 1
	})
	var audit []struct {
		Action  string         `json:"action"`
		Details map[string]any `json:"details"`
	}
	admin.do("GET", "/api/v1/audit", nil, &audit)
	opened, closed := 0, 0
	for _, a := range audit {
		switch a.Action {
		case "terminal.opened":
			opened++
		case "terminal.closed":
			closed++
		}
	}
	if opened != 2 || closed < 1 {
		t.Errorf("audit: %d opened, %d closed", opened, closed)
	}

	// Customers: their own app's containers only, and never the server.
	var pkg struct{ ID string }
	admin.do("POST", "/api/v1/packages", map[string]any{"name": "T", "max_sites": 1, "max_apps": 1}, &pkg)
	var a, b struct {
		Customer struct{ ID string } `json:"customer"`
		Password string              `json:"password"`
	}
	admin.do("POST", "/api/v1/customers", map[string]string{"email": "a@example.com", "name": "A", "package_id": pkg.ID}, &a)
	admin.do("POST", "/api/v1/customers", map[string]string{"email": "b@example.com", "name": "B", "package_id": pkg.ID}, &b)
	ca := login(t, p.web.URL, "a@example.com", a.Password)
	cb := login(t, p.web.URL, "b@example.com", b.Password)
	var app struct{ ID string }
	ca.do("POST", "/api/v1/apps", map[string]any{"server_id": serverID, "name": "shop", "kind": "image", "image": "nginx", "container_port": 80}, &app)
	for _, c := range []*client{ca, cb} {
		c.do("POST", "/api/v1/auth/confirm", map[string]string{"password": map[*client]string{ca: a.Password, cb: b.Password}[c]}, nil)
	}
	if st := ca.do("GET", path, nil, nil); st != http.StatusForbidden {
		t.Errorf("customer server terminal: %d", st)
	}
	if st := cb.do("GET", "/api/v1/apps/"+app.ID+"/terminal", nil, nil); st != http.StatusNotFound {
		t.Errorf("another customer's container: %d", st)
	}
	conn, st = dialTerminal(t, ca, "/api/v1/apps/"+app.ID+"/terminal")
	if conn == nil {
		t.Fatalf("customer's own container: %d", st)
	}
	readUntil(t, conn, "#")
	conn.CloseNow()
	fa.mu.Lock()
	if last := fa.opened[len(fa.opened)-1]; last.Kind != "container" || last.App != "shop" || last.Service != "app" {
		t.Errorf("container terminal %+v", last)
	}
	fa.mu.Unlock()

	// An administrator acting as a customer gets no terminal.
	admin.do("POST", "/api/v1/customers/"+a.Customer.ID+"/impersonate", nil, nil)
	if st := admin.do("GET", "/api/v1/apps/"+app.ID+"/terminal", nil, &e); st != http.StatusForbidden {
		t.Errorf("terminal while impersonating: %d %s", st, e.Code)
	}
}
