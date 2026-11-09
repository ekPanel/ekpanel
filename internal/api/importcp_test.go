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
	Cron:  []string{"0 3 * * * php /home/olduser/public_html/cron.php", "not a cron line"},
	OldIP: "203.0.113.5",
	Mail: []agentproto.CPanelMailDomain{{
		Domain: "old.example",
		Mailboxes: []agentproto.CPanelMailbox{
			{Name: "info", PasswordHash: mailHash, QuotaMB: 1000, Messages: 3, Bytes: 900},
			{Name: "sales", Messages: 0},
		},
		Forwarders: []agentproto.CPanelForwarder{{Name: "hello", To: []string{"info@old.example", "boss@example.org"}}, {Name: "info", To: []string{"copy@example.org"}}},
		CatchAll:   "info",
		Other:      []string{"bot@old.example: |/home/olduser/bot.php"},
	}},
	Zones: []agentproto.CPanelZone{{Name: "old.example", Records: func() []agentproto.HostedRecord {
		r, _ := agentproto.ParseZoneFile("$TTL 3600\n@ IN A 203.0.113.5\nwww IN CNAME old.example.\nftp IN A 203.0.113.5\n"+
			"@ IN MX 0 old.example.\n@ IN TXT \"v=spf1 +a +mx ~all\"\n@ IN TXT \"google-site-verification=abc\"\nwebmail IN A 203.0.113.5\n", "old.example")
		return r
	}()}},
}

var mailHash = "$6$saltsalt$" + strings.Repeat("aB3./", 17) + "x"

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
			for _, c := range p.Cron {
				res.Cron = append(res.Cron, agentproto.AppResult{ID: c.ID, Generation: c.Generation, OK: true})
			}
			if p.Mail != nil {
				res.Mail = &agentproto.MailResult{OK: true}
				for _, d := range p.Mail.Domains {
					res.Mail.Domains = append(res.Mail.Domains, agentproto.MailDomainResult{ID: d.ID, Generation: d.Generation, OK: true})
				}
			}
			if p.NameServer != nil {
				res.NameServer = &agentproto.NameServerResult{Active: true}
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
			result = agentproto.ImportApplyResult{Files: 12, Bytes: 3400, Databases: dbs, Skipped: []agentproto.ImportSkip{{Name: "mail", Count: 3}},
				MailMessages: 3 * len(p.Mail)}
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
	for _, want := range []string{"mail old.example  2 0", "dns_zone old.example  0 0", "certificate old.example  0 0", "cron_line not a cron line  0 0",
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

func TestImportCPanelMailAndZones(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	srvID, cfg, _, _ := enrolled(t, p, c, "web")
	ag := newImportAgent(t, p, cfg)
	file := "cpmove-olduser.tar.gz"

	// Without email and name servers here, the inspection says so.
	raw := map[string]any{}
	if st := c.do("POST", "/api/v1/imports/inspect", map[string]any{"server_id": srvID, "file": file}, &raw); st != http.StatusOK {
		t.Fatalf("inspect: %d", st)
	}
	if raw["mail_enabled"] != false || raw["name_servers"] != false {
		t.Fatalf("mail %v name servers %v", raw["mail_enabled"], raw["name_servers"])
	}
	var e errBody
	if st := c.do("POST", "/api/v1/imports", map[string]any{"server_id": srvID, "file": file, "mail": []string{"old.example"}}, &e); st != http.StatusBadRequest || e.Code != "mail_not_enabled" {
		t.Fatalf("mail without email here: %d %s", st, e.Code)
	}

	if st := c.do("PUT", "/api/v1/servers/"+srvID+"/mail", map[string]any{"hostname": "mail.panel.example", "tls": "internal"}, nil); st != http.StatusOK {
		t.Fatalf("enable mail: %d", st)
	}
	if st := c.do("PUT", "/api/v1/dns/nameservers", []map[string]any{{"server_id": srvID, "hostname": "ns1.panel.example", "primary": true}}, nil); st != http.StatusOK {
		t.Fatalf("name servers: %d", st)
	}
	var ins struct {
		MailEnabled          bool     `json:"mail_enabled"`
		NameServers          bool     `json:"name_servers"`
		NewPasswordMailboxes []string `json:"new_password_mailboxes"`
		NewPasswordDatabases []string `json:"new_password_databases"`
	}
	var whole json.RawMessage
	c.do("POST", "/api/v1/imports/inspect", map[string]any{"server_id": srvID, "file": file}, &whole)
	if strings.Contains(string(whole), "$6$") || strings.Contains(string(whole), "*2470C0C0") {
		t.Fatal("the inspection sends password hashes to the browser")
	}
	json.Unmarshal(whole, &ins)
	if !ins.MailEnabled || !ins.NameServers || !slices.Equal(ins.NewPasswordMailboxes, []string{"sales@old.example"}) || len(ins.NewPasswordDatabases) != 0 {
		t.Fatalf("inspection %+v", ins)
	}

	var task struct {
		ID string `json:"id"`
	}
	if st := c.do("POST", "/api/v1/imports", map[string]any{"server_id": srvID, "file": file, "sites": []string{"old.example"},
		"mail": []string{"old.example"}, "zones": []string{"old.example"}}, &task); st != http.StatusAccepted {
		t.Fatalf("import: %d", st)
	}
	var done struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Result struct {
			MailDomains     []string `json:"mail_domains"`
			Mailboxes       int      `json:"mailboxes"`
			Forwarders      int      `json:"forwarders"`
			MailMessages    int      `json:"mail_messages"`
			NewMailPassword []string `json:"new_mail_password"`
			Zones           []string `json:"zones"`
			NotImported     []struct {
				Code string `json:"code"`
				Name string `json:"name"`
			} `json:"not_imported"`
		} `json:"result"`
	}
	waitUntil(t, "the import", func() bool {
		c.do("GET", "/api/v1/tasks/"+task.ID, nil, &done)
		return done.Status == "succeeded" || done.Status == "failed"
	})
	r := done.Result
	if done.Status != "succeeded" || !slices.Equal(r.MailDomains, []string{"old.example"}) || r.Mailboxes != 2 || r.Forwarders != 1 ||
		r.MailMessages != 3 || !slices.Equal(r.NewMailPassword, []string{"sales@old.example"}) || !slices.Equal(r.Zones, []string{"old.example"}) {
		t.Fatalf("import: %s %s %+v", done.Status, done.Error, r)
	}
	var notes []string
	for _, n := range r.NotImported {
		notes = append(notes, n.Code+" "+n.Name)
	}
	for _, want := range []string{"mail_unusable bot@old.example: |/home/olduser/bot.php", "zone_old_ip ftp.old.example"} {
		if !slices.Contains(notes, want) {
			t.Errorf("report lacks %q: %q", want, notes)
		}
	}
	ag.mu.Lock()
	imp := ag.imports[len(ag.imports)-1]
	ag.mu.Unlock()
	if !slices.Equal(imp.Mail, []agentproto.ImportMailbox{{Domain: "old.example", Name: "info"}}) {
		t.Fatalf("mail to copy: %+v", imp.Mail)
	}

	// The mail domain, its mailboxes (info keeps its password and forwards a copy), the
	// forwarder and the catch-all.
	var domains []struct {
		ID       string  `json:"id"`
		Domain   string  `json:"domain"`
		CatchAll *string `json:"catch_all"`
	}
	c.do("GET", "/api/v1/mail/domains", nil, &domains)
	if len(domains) != 1 || domains[0].CatchAll == nil || *domains[0].CatchAll != "info" {
		t.Fatalf("mail domains %+v", domains)
	}
	var detail struct {
		Mailboxes []struct {
			LocalPart string   `json:"local_part"`
			QuotaMB   int      `json:"quota_mb"`
			ForwardTo []string `json:"forward_to"`
			KeepCopy  bool     `json:"keep_copy"`
		} `json:"mailboxes"`
		Forwarders []struct {
			LocalPart    string   `json:"local_part"`
			Destinations []string `json:"destinations"`
		} `json:"forwarders"`
	}
	c.do("GET", "/api/v1/mail/domains/"+domains[0].ID, nil, &detail)
	if len(detail.Mailboxes) != 2 || detail.Mailboxes[0].LocalPart != "info" || detail.Mailboxes[0].QuotaMB != 1000 ||
		!slices.Equal(detail.Mailboxes[0].ForwardTo, []string{"copy@example.org"}) || !detail.Mailboxes[0].KeepCopy ||
		len(detail.Forwarders) != 1 || detail.Forwarders[0].LocalPart != "hello" {
		t.Fatalf("mail domain %+v", detail)
	}
	var m struct {
		PasswordHash string
	}
	if err := p.pool.QueryRow(context.Background(), `SELECT password_hash FROM mailboxes WHERE local_part = 'info'`).Scan(&m.PasswordHash); err != nil || m.PasswordHash != mailHash {
		t.Fatalf("info's password hash %q %v", m.PasswordHash, err)
	}

	// The zone keeps only what ekPanel does not make: the website, its email and cPanel's
	// own names are left to ekPanel.
	var zones []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	c.do("GET", "/api/v1/dns/hosted", nil, &zones)
	if len(zones) != 1 {
		t.Fatalf("zones %+v", zones)
	}
	var zone struct {
		Kept []struct {
			Name    string `json:"name"`
			Type    string `json:"type"`
			Content string `json:"content"`
		} `json:"kept"`
	}
	c.do("GET", "/api/v1/dns/hosted/"+zones[0].ID, nil, &zone)
	var kept []string
	for _, k := range zone.Kept {
		kept = append(kept, k.Name+" "+k.Type+" "+k.Content)
	}
	slices.Sort(kept)
	if !slices.Equal(kept, []string{"ftp.old.example A 203.0.113.5", "old.example TXT google-site-verification=abc"}) {
		t.Fatalf("kept %q", kept)
	}
}
