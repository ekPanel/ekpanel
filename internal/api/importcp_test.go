package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"

	"ekpanel/internal/agent"
	"ekpanel/internal/agentproto"
)

// importAgent is a fake agent with one cPanel backup: it applies state and records the import.
type importAgent struct {
	conn     *websocket.Conn
	mu       sync.Mutex
	applies  []agentproto.StateApplyParams
	inspects int
	imports  []agentproto.ImportApplyParams
}

var importPlan = agentproto.CPanelPlan{
	User:       "olduser",
	MainDomain: "old.example",
	Domains: []agentproto.CPanelDomain{
		{Domain: "old.example", Kind: "main", DocRoot: "public_html", Aliases: []string{"parked.example"}, PHPVersion: "8.2", HasCert: true},
		{Domain: "blog.example", Kind: "addon", DocRoot: "blog.example", PHPVersion: "5.6"},
	},
	Databases: []agentproto.CPanelDatabase{
		{Name: "olduser_wp", User: "olduser_wp", Bytes: 1000, PasswordHash: "*2470C0C06DEE42FD1618BB99005ADCA2EC9D1E19"},
		{Name: "olduser_shop", User: "olduser_wp", Bytes: 500, PasswordHash: "*2470C0C06DEE42FD1618BB99005ADCA2EC9D1E19"},
	},
	Cron:         []string{"0 3 * * * php /home/olduser/public_html/cron.php", "not a cron line"},
	MailDomains:  1,
	MailAccounts: 2,
	DNSZones:     []string{"old.example"},
}

func newImportAgent(t *testing.T, p *testPanel, cfg agent.Config) *importAgent {
	tlsCfg, _ := cfg.TLSConfig()
	conn, _, err := dialAgent(t, p, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	a := &importAgent{conn: conn}
	a.send(agentproto.Envelope{Type: agentproto.TypeHello, Hello: &agentproto.Hello{Hostname: "h", AgentVersion: "v1"}})
	go a.loop()
	t.Cleanup(func() { conn.CloseNow() })
	return a
}

func (a *importAgent) send(env agentproto.Envelope) {
	b, _ := json.Marshal(env)
	a.conn.Write(context.Background(), websocket.MessageText, b)
}

func (a *importAgent) loop() {
	for {
		_, data, err := a.conn.Read(context.Background())
		if err != nil {
			return
		}
		var env agentproto.Envelope
		json.Unmarshal(data, &env)
		if env.Type != agentproto.TypeRequest {
			continue
		}
		resp := agentproto.Envelope{Type: agentproto.TypeResponse, ID: env.ID}
		var result any = struct{}{}
		switch env.Op {
		case agentproto.OpStateApply:
			var p agentproto.StateApplyParams
			json.Unmarshal(env.Params, &p)
			a.mu.Lock()
			a.applies = append(a.applies, p)
			a.mu.Unlock()
			res := agentproto.StateApplyResult{}
			for _, s := range p.Sites {
				res.Sites = append(res.Sites, agentproto.SiteResult{ID: s.ID, Generation: s.Generation, OK: true})
			}
			for _, d := range p.Databases {
				res.Databases = append(res.Databases, agentproto.DatabaseResult{ID: d.ID, Generation: d.Generation, OK: true})
			}
			result = res
		case agentproto.OpImportInspect:
			var p agentproto.ImportInspectParams
			json.Unmarshal(env.Params, &p)
			a.mu.Lock()
			a.inspects++
			a.mu.Unlock()
			if p.File != "cpmove-olduser.tar.gz" {
				resp.Error, result = &agentproto.OpError{Code: agentproto.ErrNoImportFile, Message: "no such backup"}, nil
			} else {
				result = importPlan
			}
		case agentproto.OpImportApply:
			var p agentproto.ImportApplyParams
			json.Unmarshal(env.Params, &p)
			a.mu.Lock()
			a.imports = append(a.imports, p)
			a.mu.Unlock()
			dbs := []string{}
			for _, d := range p.Databases {
				dbs = append(dbs, d.Name)
			}
			result = agentproto.ImportApplyResult{Files: 12, Bytes: 3400, Databases: dbs, Skipped: []agentproto.ImportSkip{{Name: "mail", Count: 3}}}
		}
		if result != nil {
			resp.Result, _ = json.Marshal(result)
		}
		a.send(resp)
	}
}

func (a *importAgent) lastApply() agentproto.StateApplyParams {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.applies) == 0 {
		return agentproto.StateApplyParams{}
	}
	return a.applies[len(a.applies)-1]
}

func TestImportCPanel(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	srvID, cfg, _, _ := enrolled(t, p, c, "web")
	ag := newImportAgent(t, p, cfg)

	var e errBody
	if st := c.do("POST", "/api/v1/imports/inspect", map[string]any{"server_id": srvID, "file": "../etc/passwd"}, &e); st != http.StatusBadRequest || e.Code != "invalid_import_file" {
		t.Fatalf("inspect a path: %d %s", st, e.Code)
	}
	if st := c.do("POST", "/api/v1/imports/inspect", map[string]any{"server_id": srvID, "file": "missing.tar.gz"}, &e); st != http.StatusBadGateway || e.Code != "agent_import_not_found" {
		t.Fatalf("inspect a missing file: %d %s", st, e.Code)
	}

	// A website that is already here shows up as taken.
	var blog struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if st := c.do("POST", "/api/v1/sites", map[string]any{"domain": "blog.example", "server_id": srvID, "kind": "static"}, &blog); st != http.StatusCreated {
		t.Fatalf("create site: %d", st)
	}
	var ins struct {
		User           string   `json:"user"`
		Domains        []any    `json:"domains"`
		TakenDomains   []string `json:"taken_domains"`
		TakenDatabases []string `json:"taken_databases"`
		PHPVersions    []string `json:"php_versions"`
	}
	file := "cpmove-olduser.tar.gz"
	if st := c.do("POST", "/api/v1/imports/inspect", map[string]any{"server_id": srvID, "file": file}, &ins); st != http.StatusOK {
		t.Fatalf("inspect: %d", st)
	}
	if ins.User != "olduser" || len(ins.Domains) != 2 || !slices.Equal(ins.TakenDomains, []string{"blog.example"}) || len(ins.PHPVersions) == 0 {
		t.Fatalf("inspection: %+v", ins)
	}
	body := map[string]any{"server_id": srvID, "file": file, "sites": []string{"old.example", "blog.example"},
		"databases": []string{"olduser_wp", "olduser_shop"}, "cron": true}
	if st := c.do("POST", "/api/v1/imports", body, &e); st != http.StatusConflict || e.Code != "import_domains_taken" {
		t.Fatalf("import over a taken domain: %d %s", st, e.Code)
	}
	if st := c.do("POST", "/api/v1/imports", map[string]any{"server_id": srvID, "file": file, "sites": []string{"nope.example"}}, &e); st != http.StatusBadRequest || e.Code != "not_in_backup" {
		t.Fatalf("import a domain the backup lacks: %d %s", st, e.Code)
	}
	ag.mu.Lock()
	inspected := ag.inspects
	ag.mu.Unlock()

	body["sites"] = []string{"old.example"}
	var task struct {
		ID string `json:"id"`
	}
	if st := c.do("POST", "/api/v1/imports", body, &task); st != http.StatusAccepted {
		t.Fatalf("import: %d", st)
	}
	ag.mu.Lock()
	if ag.inspects != inspected {
		t.Fatalf("the import read the archive again right after an inspection")
	}
	ag.mu.Unlock()
	var done struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Result struct {
			Sites       []string `json:"sites"`
			Databases   []string `json:"databases"`
			Cron        int      `json:"cron"`
			Files       int64    `json:"files"`
			NewPassword []string `json:"new_password"`
			NotImported []struct {
				Code    string `json:"code"`
				Name    string `json:"name"`
				Detail  string `json:"detail"`
				Count   int    `json:"count"`
				Domains int    `json:"domains"`
			} `json:"not_imported"`
		} `json:"result"`
	}
	waitUntil(t, "the import", func() bool {
		c.do("GET", "/api/v1/tasks/"+task.ID, nil, &done)
		return done.Status == "succeeded" || done.Status == "failed"
	})
	if done.Status != "succeeded" {
		t.Fatalf("import task: %+v", done)
	}
	r := done.Result
	if !slices.Equal(r.Sites, []string{"old.example"}) || len(r.Databases) != 2 || r.Cron != 1 || r.Files != 12 ||
		!slices.Equal(r.NewPassword, []string{"olduser_shop"}) {
		t.Fatalf("report: %+v", r)
	}
	var notes []string
	for _, n := range r.NotImported {
		notes = append(notes, fmt.Sprintf("%s %s %s %d %d", n.Code, n.Name, n.Detail, n.Count, n.Domains))
	}
	for _, want := range []string{"mail   2 1", "dns_zone old.example  0 0", "certificate old.example  0 0", "cron_line not a cron line  0 0",
		"db_user olduser_wp olduser_shop 0 0", "folder mail  3 0"} {
		if !slices.Contains(notes, want) {
			t.Errorf("report lacks %q:\n%q", want, notes)
		}
	}

	// The website keeps its PHP version and parked domain; the agent got where things go.
	var sites []struct {
		ID         string   `json:"id"`
		Domain     string   `json:"domain"`
		Aliases    []string `json:"aliases"`
		PHPVersion *string  `json:"php_version"`
		LinuxUser  string   `json:"linux_user"`
	}
	c.do("GET", "/api/v1/sites", nil, &sites)
	i := slices.IndexFunc(sites, func(s struct {
		ID         string   `json:"id"`
		Domain     string   `json:"domain"`
		Aliases    []string `json:"aliases"`
		PHPVersion *string  `json:"php_version"`
		LinuxUser  string   `json:"linux_user"`
	}) bool {
		return s.Domain == "old.example"
	})
	if i < 0 {
		t.Fatalf("no imported website in %+v", sites)
	}
	site := sites[i]
	if site.PHPVersion == nil || *site.PHPVersion != "8.2" || !slices.Equal(site.Aliases, []string{"parked.example"}) {
		t.Fatalf("imported website: %+v", site)
	}
	ag.mu.Lock()
	imp := ag.imports
	ag.mu.Unlock()
	if len(imp) != 1 || imp[0].MainUser != site.LinuxUser || len(imp[0].Sites) != 1 || imp[0].Sites[0].DocRoot != "public_html" ||
		len(imp[0].Databases) != 2 || imp[0].Databases[0].User != "olduser_wp" || imp[0].Databases[1].User != "olduser_shop" {
		t.Fatalf("import params: %+v", imp)
	}
	// The first database keeps its password; the second gets a new one.
	ap := ag.lastApply()
	for _, d := range ap.Databases {
		if d.Name == "olduser_wp" && d.PasswordHash != importPlan.Databases[0].PasswordHash {
			t.Fatalf("olduser_wp lost its password: %+v", d)
		}
		if d.Name == "olduser_shop" && (d.PasswordHash == importPlan.Databases[1].PasswordHash || !strings.HasPrefix(d.PasswordHash, "*")) {
			t.Fatalf("olduser_shop: %+v", d)
		}
	}
	var jobs []struct {
		SiteID  *string `json:"site_id"`
		Command string  `json:"command"`
	}
	c.do("GET", "/api/v1/cron", nil, &jobs)
	if len(jobs) != 1 || jobs[0].SiteID == nil || *jobs[0].SiteID != site.ID ||
		jobs[0].Command != "php8.2 /home/"+site.LinuxUser+"/public_html/cron.php" {
		t.Fatalf("cron jobs: %+v", jobs)
	}

	// The same databases cannot be imported twice.
	if st := c.do("POST", "/api/v1/imports", map[string]any{"server_id": srvID, "file": file, "databases": []string{"olduser_wp"}}, &e); st != http.StatusConflict || e.Code != "import_database_taken" {
		t.Fatalf("import a database again: %d %s", st, e.Code)
	}
}
