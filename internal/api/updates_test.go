package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"ekpanel/internal/agent"
	"ekpanel/internal/agentproto"
	"ekpanel/internal/api"
	"ekpanel/internal/release"
	"ekpanel/internal/store"
	"ekpanel/internal/tasks"
	"ekpanel/internal/updates"
)

type updatesBody struct {
	Current string `json:"current"`
	Release bool   `json:"release"`
	Latest  struct {
		Version string `json:"version"`
		Error   string `json:"error"`
	} `json:"latest"`
	UpdateAvailable bool               `json:"update_available"`
	CanUpdate       bool               `json:"can_update"`
	State           *tasks.UpdateState `json:"state"`
	Outdated        int                `json:"outdated"`
	Servers         []struct {
		Name         string `json:"name"`
		AgentVersion string `json:"agent_version"`
		PanelHost    bool   `json:"panel_host"`
		Outdated     bool   `json:"outdated"`
	} `json:"servers"`
}

// releasesAt serves release.json naming version, as GitHub's "latest" link does.
func releasesAt(t *testing.T, version string) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/latest/download/release.json" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"version":"` + version + `","notes_url":"https://example.com/notes"}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func withRelease(version, releasesURL string) func(*api.Options, *store.Store) {
	return func(o *api.Options, _ *store.Store) {
		o.Version, o.ReleasesURL = version, releasesURL
	}
}

// connectAgent connects an enrolled agent that reports version and answers with handle.
func connectAgent(t *testing.T, p *testPanel, cfg agent.Config, version string, panelHost bool, handle func(op string, raw json.RawMessage) (any, *agentproto.OpError)) *websocket.Conn {
	t.Helper()
	tlsCfg, _ := cfg.TLSConfig()
	conn, _, err := dialAgent(t, p, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(agentproto.Envelope{Type: agentproto.TypeHello, Hello: &agentproto.Hello{Hostname: "h", AgentVersion: version, PanelHost: panelHost}})
	conn.Write(context.Background(), websocket.MessageText, b)
	fakeAgent(t, conn, handle)
	return conn
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestUpdatePanel(t *testing.T) {
	url := releasesAt(t, "v0.2.0")
	p := newTestPanel(t, withRelease("v0.1.0", url), func(o *api.Options, st *store.Store) {
		o.Updates = updates.New(st, &release.Client{BaseURL: url}, "v0.1.0", nil)
	})
	c := newClient(t, p.web.URL)
	setUp(t, c)

	var u updatesBody
	if s := c.do("GET", "/api/v1/updates", nil, &u); s != http.StatusOK || u.Current != "v0.1.0" || !u.Release || u.UpdateAvailable {
		t.Fatalf("before a check: %d %+v", s, u)
	}
	var e errBody
	if s := c.do("POST", "/api/v1/updates/panel", map[string]string{"version": "v0.2.0"}, &e); s != http.StatusBadRequest || e.Code != "invalid_version" {
		t.Fatalf("update before checking: %d %s", s, e.Code)
	}
	if s := c.do("POST", "/api/v1/updates/check", nil, &u); s != http.StatusOK || u.Latest.Version != "v0.2.0" || !u.UpdateAvailable || u.CanUpdate {
		t.Fatalf("check: %d %+v", s, u)
	}
	if s := c.do("POST", "/api/v1/updates/panel", map[string]string{"version": "v0.2.0"}, &e); s != http.StatusConflict || e.Code != "panel_agent_offline" {
		t.Fatalf("update without the panel's agent: %d %s", s, e.Code)
	}

	// The panel's own server connects; another server too.
	hostID, hostCfg, _, _ := enrolled(t, p, c, "panel-server")
	_, webCfg, _, _ := enrolled(t, p, c, "web-1")
	var mu sync.Mutex
	var got []agentproto.SystemUpdateParams
	answer := func(op string, raw json.RawMessage) (any, *agentproto.OpError) {
		switch op {
		case agentproto.OpSystemUpdate:
			var in agentproto.SystemUpdateParams
			json.Unmarshal(raw, &in)
			mu.Lock()
			got = append(got, in)
			mu.Unlock()
			return agentproto.SystemUpdateResult{Previous: "v0.1.0", Version: in.Version, Backup: "/var/backups/ekpanel/x.dump"}, nil
		case agentproto.OpStateApply:
			return agentproto.StateApplyResult{}, nil
		}
		return nil, &agentproto.OpError{Code: agentproto.ErrUnknownOp}
	}
	defer connectAgent(t, p, hostCfg, "v0.1.0", true, answer).CloseNow()
	defer connectAgent(t, p, webCfg, "v0.1.0", false, answer).CloseNow()
	waitUntil(t, "agents online", func() bool {
		c.do("GET", "/api/v1/updates", nil, &u)
		return u.CanUpdate && len(u.Servers) == 2 && u.Servers[0].AgentVersion == "v0.1.0"
	})
	if !u.Servers[0].PanelHost || u.Servers[0].Name != "panel-server" || u.Outdated != 0 {
		t.Fatalf("servers: %+v (agents at the panel's version are not outdated)", u)
	}

	for _, v := range []string{"v0.1.0", "v0.3.0", "../../x"} {
		if s := c.do("POST", "/api/v1/updates/panel", map[string]string{"version": v}, &e); s != http.StatusBadRequest {
			t.Fatalf("update to %s: %d", v, s)
		}
	}
	if s := c.do("POST", "/api/v1/updates/panel", map[string]string{"version": "v0.2.0"}, nil); s != http.StatusAccepted {
		t.Fatalf("update: %d", s)
	}
	waitUntil(t, "the panel update to reach the agent", func() bool {
		c.do("GET", "/api/v1/updates", nil, &u)
		return u.State != nil && u.State.Backup != ""
	})
	mu.Lock()
	if len(got) != 1 || got[0].Component != "panel" || got[0].Version != "v0.2.0" || got[0].ReleasesURL != url {
		t.Fatalf("agent got %+v", got)
	}
	mu.Unlock()
	if u.State.Phase != tasks.UpdatePanel || u.State.Target != "v0.2.0" || u.State.From != "v0.1.0" {
		t.Fatalf("state: %+v", u.State)
	}
	// While it runs, nothing else starts.
	if s := c.do("POST", "/api/v1/updates/agents", nil, &e); s != http.StatusConflict || e.Code != "update_running" {
		t.Fatalf("agents during the panel update: %d %s", s, e.Code)
	}
	_ = hostID
}

func TestUpdateAgents(t *testing.T) {
	url := releasesAt(t, "v0.2.0")
	p := newTestPanel(t, withRelease("v0.2.0", url))
	p.runner.SetUpdateWait(3 * time.Second)
	c := newClient(t, p.web.URL)
	setUp(t, c)

	_, hostCfg, _, _ := enrolled(t, p, c, "panel-server")
	_, webCfg, _, _ := enrolled(t, p, c, "web-1")
	_, brokenCfg, _, _ := enrolled(t, p, c, "web-2")
	enrolled(t, p, c, "offline-1")

	// An agent that "restarts" as the new version, and one that comes back as the old one.
	var conns sync.Map
	restartAs := func(name string, cfg agent.Config, panelHost bool, version string) func(string, json.RawMessage) (any, *agentproto.OpError) {
		var handle func(string, json.RawMessage) (any, *agentproto.OpError)
		handle = func(op string, raw json.RawMessage) (any, *agentproto.OpError) {
			switch op {
			case agentproto.OpSystemUpdate:
				var in agentproto.SystemUpdateParams
				json.Unmarshal(raw, &in)
				if in.Component != "agent" || in.Version != "v0.2.0" {
					return nil, &agentproto.OpError{Code: agentproto.ErrInvalidParams, Message: "unexpected " + string(raw)}
				}
				go func() {
					time.Sleep(100 * time.Millisecond)
					if old, ok := conns.Load(name); ok {
						old.(*websocket.Conn).CloseNow()
					}
					conns.Store(name, connectAgent(t, p, cfg, version, panelHost, handle))
				}()
				return agentproto.SystemUpdateResult{Previous: "v0.1.0", Version: in.Version}, nil
			case agentproto.OpStateApply:
				return agentproto.StateApplyResult{}, nil
			}
			return nil, &agentproto.OpError{Code: agentproto.ErrUnknownOp}
		}
		return handle
	}
	conns.Store("host", connectAgent(t, p, hostCfg, "v0.1.0", true, restartAs("host", hostCfg, true, "v0.2.0")))
	conns.Store("web", connectAgent(t, p, webCfg, "v0.1.0", false, restartAs("web", webCfg, false, "v0.2.0")))
	conns.Store("broken", connectAgent(t, p, brokenCfg, "v0.1.0", false, restartAs("broken", brokenCfg, false, "v0.1.0")))
	t.Cleanup(func() {
		conns.Range(func(_, v any) bool { v.(*websocket.Conn).CloseNow(); return true })
	})

	var u updatesBody
	waitUntil(t, "agents online", func() bool {
		c.do("GET", "/api/v1/updates", nil, &u)
		return u.Outdated == 4 // offline-1 has never reported a version
	})
	if s := c.do("POST", "/api/v1/updates/agents", nil, nil); s != http.StatusAccepted {
		t.Fatalf("update agents: %d", s)
	}
	waitUntil(t, "the agents update to finish", func() bool {
		c.do("GET", "/api/v1/updates", nil, &u)
		return u.State != nil && u.State.Phase == tasks.UpdateDone
	})
	status := map[string]string{}
	for _, s := range u.State.Servers {
		status[s.Name] = s.Status
	}
	if u.State.Servers[0].Name != "panel-server" ||
		status["panel-server"] != "updated" || status["web-1"] != "updated" || status["web-2"] != "failed" || status["offline-1"] != "offline" {
		t.Fatalf("result: %+v", u.State.Servers)
	}
	if u.Outdated != 2 { // web-2 is back at v0.1.0, offline-1 never reported a version
		t.Fatalf("outdated after the update: %d", u.Outdated)
	}
}

func TestResumeUpdate(t *testing.T) {
	p := newTestPanel(t, withRelease("v0.2.0", "https://example.com/releases"))
	c := newClient(t, p.web.URL)
	setUp(t, c)
	ctx := context.Background()

	// Started by the update to v0.3.0, but still v0.2.0: the new version did not come up.
	p.store.SetSetting(ctx, "update.state", tasks.UpdateState{Target: "v0.3.0", From: "v0.2.0", Phase: tasks.UpdatePanel, StartedAt: time.Now()})
	p.runner.ResumeUpdate(ctx)
	st, _ := p.runner.UpdateState(ctx)
	if st.Phase != tasks.UpdateFailed || !strings.Contains(st.Error, "did not start") {
		t.Fatalf("state: %+v", st)
	}

	// Started by the update to v0.2.0, which is this version: on to the agents (none here).
	p.store.SetSetting(ctx, "update.state", tasks.UpdateState{Target: "v0.2.0", From: "v0.1.0", Phase: tasks.UpdatePanel, StartedAt: time.Now()})
	p.runner.ResumeUpdate(ctx)
	waitUntil(t, "the agents phase to finish", func() bool {
		st, _ = p.runner.UpdateState(ctx)
		return st.Phase == tasks.UpdateDone
	})
	if st.From != "v0.1.0" || st.FinishedAt == nil {
		t.Fatalf("state: %+v", st)
	}

	// A panel built from source cannot update agents.
	dev := newTestPanel(t)
	dc := newClient(t, dev.web.URL)
	setUp(t, dc)
	var e errBody
	if s := dc.do("POST", "/api/v1/updates/agents", nil, &e); s != http.StatusConflict || e.Code != "not_a_release" {
		t.Fatalf("dev panel: %d %s", s, e.Code)
	}
	if s := dc.do("POST", "/api/v1/updates/panel", map[string]string{"version": "v0.2.0"}, &e); s != http.StatusConflict || e.Code != "not_a_release" {
		t.Fatalf("dev panel updating itself: %d %s", s, e.Code)
	}
	var u updatesBody
	if s := dc.do("GET", "/api/v1/updates", nil, &u); s != http.StatusOK || u.Release || u.Current != "" {
		t.Fatalf("dev panel updates: %d %+v", s, u)
	}
}
