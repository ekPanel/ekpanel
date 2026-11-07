package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"ekpanel/internal/agentproto"
)

func TestWordPressToolkit(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	serverID, cfg, _, _ := enrolled(t, p, c, "wp-1")
	tlsCfg, _ := cfg.TLSConfig()
	conn, _, err := dialAgent(t, p, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	// A WordPress found on the live site (installed by hand), and copies made on request.
	var mu sync.Mutex
	var clones []agentproto.WordPressCloneParams
	var updates []agentproto.WordPressUpdateParams
	var manages []string
	wpInfo := func(user string) agentproto.WordPressInfo {
		if strings.HasPrefix(user, "shop") {
			return agentproto.WordPressInfo{Installed: true, Version: "6.7.1", CoreMinor: "6.7.2",
				Plugins: []agentproto.WordPressItem{{Name: "akismet", Title: "Akismet", Status: "active", Version: "5.3", Update: "5.4"}}, Themes: []agentproto.WordPressItem{}}
		}
		mu.Lock()
		defer mu.Unlock()
		return agentproto.WordPressInfo{Installed: len(clones) > 0, Version: "6.7.1", Plugins: []agentproto.WordPressItem{}, Themes: []agentproto.WordPressItem{}}
	}
	fakeAgent(t, conn, func(op string, raw json.RawMessage) (any, *agentproto.OpError) {
		switch op {
		case agentproto.OpStateApply:
			var in agentproto.StateApplyParams
			json.Unmarshal(raw, &in)
			var out agentproto.StateApplyResult
			for _, s := range in.Sites {
				out.Sites = append(out.Sites, agentproto.SiteResult{ID: s.ID, Generation: s.Generation, OK: true})
			}
			for _, d := range in.Databases {
				out.Databases = append(out.Databases, agentproto.DatabaseResult{ID: d.ID, Generation: d.Generation, OK: true})
			}
			return out, nil
		case agentproto.OpWordPressInfo:
			var in agentproto.WordPressInfoParams
			json.Unmarshal(raw, &in)
			return wpInfo(in.Site.User), nil
		case agentproto.OpWordPressManage:
			var in agentproto.WordPressManageParams
			json.Unmarshal(raw, &in)
			mu.Lock()
			manages = append(manages, in.Action+" "+in.Name)
			mu.Unlock()
			info := wpInfo(in.Site.User)
			info.Maintenance = in.Action == "maintenance_on"
			return info, nil
		case agentproto.OpWordPressUpdate:
			var in agentproto.WordPressUpdateParams
			json.Unmarshal(raw, &in)
			mu.Lock()
			updates = append(updates, in)
			mu.Unlock()
			return agentproto.WordPressUpdateResult{Items: []agentproto.WordPressUpdated{{Kind: "plugin", Name: "akismet", From: "5.3", To: "5.4", Outcome: "rolled_back", Error: "the site broke"}},
				Info: wpInfo(in.Site.User)}, nil
		case agentproto.OpWordPressClone:
			var in agentproto.WordPressCloneParams
			json.Unmarshal(raw, &in)
			if err := in.Validate(); err != nil {
				return nil, &agentproto.OpError{Code: agentproto.ErrInvalidParams, Message: err.Error()}
			}
			mu.Lock()
			clones = append(clones, in)
			mu.Unlock()
			return agentproto.WordPressCloneResult{Info: wpInfo(in.To.User)}, nil
		}
		return nil, &agentproto.OpError{Code: agentproto.ErrUnknownOp}
	})

	type siteView struct {
		ID        string                    `json:"id"`
		Domain    string                    `json:"domain"`
		App       *string                   `json:"app"`
		State     string                    `json:"state"`
		StagingOf *string                   `json:"staging_of"`
		WordPress *agentproto.WordPressInfo `json:"wordpress"`
		WPAuto    struct {
			Core    string `json:"core"`
			Plugins bool   `json:"plugins"`
		} `json:"wp_auto"`
	}
	var live siteView
	if st := c.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "shop.example.com", "kind": "php", "php_version": "8.4"}, &live); st != http.StatusCreated {
		t.Fatalf("site: %d", st)
	}
	var e errBody
	if st := c.do("POST", "/api/v1/sites/"+live.ID+"/wordpress/manage", map[string]string{"action": "maintenance_on"}, &e); st != http.StatusConflict || e.Code != "not_wordpress" {
		t.Errorf("before a scan found WordPress: %d %s", st, e.Code)
	}
	if st := c.do("POST", "/api/v1/sites/"+live.ID+"/wordpress/refresh", nil, &live); st != http.StatusOK || live.App == nil || *live.App != "wordpress" ||
		live.WordPress == nil || live.WordPress.CoreMinor != "6.7.2" || live.WPAuto.Core != "minor" {
		t.Fatalf("refresh: %d %+v", st, live)
	}
	var list []siteView
	if c.do("GET", "/api/v1/wordpress", nil, &list); len(list) != 1 || list[0].ID != live.ID {
		t.Errorf("WordPress list: %+v", list)
	}

	// Changes and rules.
	if st := c.do("POST", "/api/v1/sites/"+live.ID+"/wordpress/manage", map[string]string{"action": "rm -rf"}, &e); st != http.StatusBadRequest {
		t.Errorf("unknown action: %d", st)
	}
	if st := c.do("POST", "/api/v1/sites/"+live.ID+"/wordpress/manage", map[string]string{"action": "maintenance_on"}, &live); st != http.StatusOK || !live.WordPress.Maintenance {
		t.Errorf("maintenance: %d %+v", st, live.WordPress)
	}
	if st := c.do("PUT", "/api/v1/sites/"+live.ID+"/wordpress/auto", map[string]any{"core": "everything"}, &e); st != http.StatusBadRequest {
		t.Errorf("bad rule: %d", st)
	}
	if st := c.do("PUT", "/api/v1/sites/"+live.ID+"/wordpress/auto", map[string]any{"core": "all", "plugins": true}, &live); st != http.StatusOK || live.WPAuto.Core != "all" || !live.WPAuto.Plugins {
		t.Errorf("rules: %d %+v", st, live.WPAuto)
	}

	// Updates run as a task; a rollback is reported.
	if st := c.do("POST", "/api/v1/sites/"+live.ID+"/wordpress/update", map[string]any{"plugins": []string{"../x"}}, &e); st != http.StatusBadRequest {
		t.Errorf("bad plugin name: %d", st)
	}
	if st := c.do("POST", "/api/v1/sites/"+live.ID+"/wordpress/update", map[string]any{"core": "minor", "plugins": []string{"akismet"}}, nil); st != http.StatusAccepted {
		t.Fatalf("update: %d", st)
	}
	eventually(t, "update ran", func() bool {
		var tasks []struct {
			Kind   string          `json:"kind"`
			Status string          `json:"status"`
			Result json.RawMessage `json:"result"`
		}
		c.do("GET", "/api/v1/tasks?active=0&server_id="+serverID, nil, &tasks)
		for _, tk := range tasks {
			if tk.Kind == "site.wordpress_update" && tk.Status == "succeeded" && strings.Contains(string(tk.Result), "rolled_back") {
				return true
			}
		}
		return false
	})
	mu.Lock()
	if len(updates) != 1 || updates[0].Core != "minor" || updates[0].Plugins[0] != "akismet" || updates[0].Site.User == "" {
		t.Errorf("update sent: %+v", updates)
	}
	mu.Unlock()

	// A staging copy: its own site and database, then pushed back.
	var started struct {
		StagingID string `json:"staging_id"`
		Domain    string `json:"domain"`
	}
	if st := c.do("POST", "/api/v1/sites/"+live.ID+"/wordpress/staging", nil, &started); st != http.StatusAccepted || started.Domain != "staging.shop.example.com" {
		t.Fatalf("staging: %d %+v", st, started)
	}
	if st := c.do("POST", "/api/v1/sites/"+live.ID+"/wordpress/staging", nil, &e); st != http.StatusConflict || e.Code != "staging_exists" {
		t.Errorf("second staging: %d %s", st, e.Code)
	}
	var staging siteView
	eventually(t, "staging copy made", func() bool {
		c.do("GET", "/api/v1/sites/"+started.StagingID, nil, &staging)
		return staging.App != nil && *staging.App == "wordpress"
	})
	if staging.StagingOf == nil || *staging.StagingOf != live.ID {
		t.Errorf("staging site: %+v", staging)
	}
	if st := c.do("POST", "/api/v1/sites/"+started.StagingID+"/wordpress/staging", nil, &e); st != http.StatusConflict || e.Code != "is_staging" {
		t.Errorf("staging of a staging copy: %d %s", st, e.Code)
	}
	if st := c.do("POST", "/api/v1/sites/"+live.ID+"/wordpress/copy", map[string]any{"direction": "push", "files": true}, &e); st != http.StatusConflict || e.Code != "not_staging" {
		t.Errorf("copy from a live site: %d %s", st, e.Code)
	}
	if st := c.do("POST", "/api/v1/sites/"+started.StagingID+"/wordpress/copy", map[string]any{"direction": "push"}, &e); st != http.StatusBadRequest {
		t.Errorf("copy of nothing: %d", st)
	}
	if st := c.do("POST", "/api/v1/sites/"+started.StagingID+"/wordpress/copy", map[string]any{"direction": "push", "files": true, "database": true}, nil); st != http.StatusAccepted {
		t.Fatalf("push: %d", st)
	}
	eventually(t, "pushed", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(clones) == 2
	})
	mu.Lock()
	first, push := clones[0], clones[1]
	mu.Unlock()
	if first.From.Domain != "shop.example.com" || first.To.Domain != "staging.shop.example.com" || first.ToDB == nil || !first.NoIndex || !first.Files || !first.Database {
		t.Errorf("staging copy: %+v", first)
	}
	if push.From.Domain != "staging.shop.example.com" || push.To.Domain != "shop.example.com" || push.ToDB != nil || push.NoIndex {
		t.Errorf("push: %+v", push)
	}
	var dbs []struct {
		Name string `json:"name"`
	}
	c.do("GET", "/api/v1/databases", nil, &dbs)
	if len(dbs) != 1 || dbs[0].Name != first.ToDB.Name {
		t.Errorf("staging database: %+v", dbs)
	}
	var tasks []map[string]any
	c.do("GET", "/api/v1/tasks?active=0&server_id="+serverID, nil, &tasks)
	for _, tk := range tasks {
		if b, _ := json.Marshal(tk); strings.Contains(string(b), first.ToDB.Password) {
			t.Fatalf("database password stored in a task: %s", b)
		}
	}
}
