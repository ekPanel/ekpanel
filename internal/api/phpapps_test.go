package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/auth"
)

func TestPHPAppInstaller(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	serverID, cfg, _, _ := enrolled(t, p, c, "apps-1")
	tlsCfg, _ := cfg.TLSConfig()
	conn, _, err := dialAgent(t, p, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	var mu sync.Mutex
	var installs []agentproto.PHPAppInstallParams
	var lastSites []agentproto.SiteSpec
	var dbHashes = map[string]string{}
	fakeAgent(t, conn, func(op string, raw json.RawMessage) (any, *agentproto.OpError) {
		mu.Lock()
		defer mu.Unlock()
		switch op {
		case agentproto.OpStateApply:
			var in agentproto.StateApplyParams
			json.Unmarshal(raw, &in)
			lastSites = in.Sites
			var out agentproto.StateApplyResult
			for _, s := range in.Sites {
				out.Sites = append(out.Sites, agentproto.SiteResult{ID: s.ID, Generation: s.Generation, OK: true})
			}
			for _, d := range in.Databases {
				dbHashes[d.Name] = d.PasswordHash
				out.Databases = append(out.Databases, agentproto.DatabaseResult{ID: d.ID, Generation: d.Generation, OK: true})
			}
			for _, cj := range in.Cron {
				out.Cron = append(out.Cron, agentproto.AppResult{ID: cj.ID, Generation: cj.Generation, OK: true})
			}
			return out, nil
		case agentproto.OpPHPAppInstall:
			var in agentproto.PHPAppInstallParams
			json.Unmarshal(raw, &in)
			if err := in.Validate(); err != nil {
				return nil, &agentproto.OpError{Code: agentproto.ErrInvalidParams, Message: err.Error()}
			}
			installs = append(installs, in)
			return agentproto.PHPAppInstallResult{Version: "5.3"}, nil
		}
		return nil, &agentproto.OpError{Code: agentproto.ErrUnknownOp}
	})

	type siteView struct {
		ID         string             `json:"id"`
		App        *string            `json:"app"`
		AppVersion *string            `json:"app_version"`
		Web        agentproto.SiteWeb `json:"web"`
	}
	newSite := func(domain, php string) siteView {
		var s siteView
		if st := c.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": domain, "kind": "php", "php_version": php}, &s); st != http.StatusCreated {
			t.Fatalf("site %s: %d", domain, st)
		}
		return s
	}
	var catalog []agentproto.PHPApp
	if c.do("GET", "/api/v1/php-apps", nil, &catalog); len(catalog) != 4 {
		t.Errorf("catalog: %+v", catalog)
	}

	var e errBody
	old := newSite("old.example.com", "8.2")
	if st := c.do("POST", "/api/v1/sites/"+old.ID+"/app", map[string]string{"app": "joomla", "admin_user": "admin"}, &e); st != http.StatusConflict || e.Code != "app_needs_newer_php" {
		t.Errorf("old PHP: %d %s", st, e.Code)
	}
	if st := c.do("POST", "/api/v1/sites/"+old.ID+"/app", map[string]string{"app": "prestashop"}, &e); st != http.StatusBadRequest {
		t.Errorf("unknown app: %d", st)
	}

	// Moodle: its public folder served, its PHP setting set, a database and a cron job.
	learn := newSite("learn.example.com", "8.4")
	var started struct {
		AdminUser     string `json:"admin_user"`
		AdminPassword string `json:"admin_password"`
		AdminURL      string `json:"admin_url"`
	}
	if st := c.do("POST", "/api/v1/sites/"+learn.ID+"/app", map[string]string{"app": "moodle", "admin_user": "Teacher", "title": "School"}, &started); st != http.StatusAccepted ||
		started.AdminPassword == "" || started.AdminUser != "teacher" || started.AdminURL != "https://learn.example.com/login/index.php" {
		t.Fatalf("moodle: %d %+v", st, started)
	}
	eventually(t, "moodle installed", func() bool {
		c.do("GET", "/api/v1/sites/"+learn.ID, nil, &learn)
		return learn.App != nil && *learn.App == "moodle"
	})
	if learn.Web.DocRoot != "public" || learn.Web.PHP["max_input_vars"] != "5000" {
		t.Errorf("site web: %+v", learn.Web)
	}
	mu.Lock()
	in := installs[0]
	if in.App != "moodle" || in.AdminPassword != started.AdminPassword || in.Title != "School" || dbHashes[in.DB.Name] != auth.MariaDBNativeHash(in.DB.Password) {
		t.Errorf("install sent: %+v", in)
	}
	for _, s := range lastSites {
		if s.ID == learn.ID && s.Web.DocRoot != "public" {
			t.Errorf("document root not sent: %+v", s.Web)
		}
	}
	mu.Unlock()
	var cron []struct {
		Name    string `json:"name"`
		Command string `json:"command"`
	}
	c.do("GET", "/api/v1/cron", nil, &cron)
	if len(cron) != 1 || !strings.HasSuffix(cron[0].Command, "/public_html/admin/cli/cron.php") || !strings.HasPrefix(cron[0].Command, "/usr/bin/php8.4 /home/") {
		t.Errorf("cron: %+v", cron)
	}
	if st := c.do("POST", "/api/v1/sites/"+learn.ID+"/app", map[string]string{"app": "joomla", "admin_user": "admin"}, &e); st != http.StatusConflict || e.Code != "app_installed" {
		t.Errorf("second app: %d %s", st, e.Code)
	}

	// Laravel has no admin account.
	api := newSite("api.example.com", "8.4")
	var lv map[string]any
	if st := c.do("POST", "/api/v1/sites/"+api.ID+"/app", map[string]string{"app": "laravel"}, &lv); st != http.StatusAccepted || lv["admin_password"] != nil {
		t.Errorf("laravel: %d %+v", st, lv)
	}
	eventually(t, "laravel installed", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(installs) == 2 && installs[1].App == "laravel" && installs[1].AdminPassword == ""
	})
}
