package api_test

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/auth"
)

func TestFTP(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	serverID, cfg, _, _ := enrolled(t, p, c, "ftp-1")
	tlsCfg, _ := cfg.TLSConfig()
	conn, _, err := dialAgent(t, p, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	var mu sync.Mutex
	var last agentproto.StateApplyParams
	fakeAgent(t, conn, func(op string, raw json.RawMessage) (any, *agentproto.OpError) {
		var in agentproto.StateApplyParams
		json.Unmarshal(raw, &in)
		mu.Lock()
		last = in
		mu.Unlock()
		var out agentproto.StateApplyResult
		for _, s := range in.Sites {
			out.Sites = append(out.Sites, agentproto.SiteResult{ID: s.ID, Generation: s.Generation, OK: true})
		}
		return out, nil
	})
	sent := func() (agentproto.SiteSpec, *agentproto.FirewallSpec) {
		mu.Lock()
		defer mu.Unlock()
		if len(last.Sites) == 0 {
			return agentproto.SiteSpec{}, nil
		}
		return last.Sites[0], last.Firewall
	}

	c.do("PUT", "/api/v1/servers/"+serverID+"/firewall", map[string]any{"mode": "on"}, nil)
	var site struct {
		ID         string `json:"id"`
		FTPEnabled bool   `json:"ftp_enabled"`
	}
	c.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "files.example.com", "kind": "static"}, &site)
	var on struct {
		Site struct {
			FTPEnabled bool `json:"ftp_enabled"`
		} `json:"site"`
		Password string `json:"password"`
	}
	if st := c.do("POST", "/api/v1/sites/"+site.ID+"/ftp", map[string]bool{"enabled": true}, &on); st != http.StatusOK || !on.Site.FTPEnabled || on.Password == "" {
		t.Fatalf("turn on: %d %+v", st, on)
	}
	eventually(t, "FTP sent", func() bool {
		spec, fw := sent()
		return spec.FTPEnabled && auth.CheckLinuxPassword(spec.FTPPasswordHash, on.Password) && fw != nil && fw.FTP
	})
	var reset struct {
		Password string `json:"password"`
	}
	c.do("POST", "/api/v1/sites/"+site.ID+"/ftp/password", nil, &reset)
	eventually(t, "new password sent", func() bool {
		spec, _ := sent()
		return reset.Password != "" && auth.CheckLinuxPassword(spec.FTPPasswordHash, reset.Password)
	})
	c.do("POST", "/api/v1/sites/"+site.ID+"/ftp", map[string]bool{"enabled": false}, nil)
	eventually(t, "FTP off", func() bool {
		spec, fw := sent()
		return !spec.FTPEnabled && spec.FTPPasswordHash == "" && fw != nil && !fw.FTP
	})
	var e errBody
	if st := c.do("POST", "/api/v1/sites/"+site.ID+"/ftp/password", nil, &e); st != http.StatusConflict {
		t.Errorf("password while off: %d", st)
	}

	// A package without FTP refuses it.
	var pkg struct {
		ID string `json:"id"`
	}
	c.do("POST", "/api/v1/packages", map[string]any{"name": "NoFTP", "max_sites": 1}, &pkg)
	var cust struct {
		Password string `json:"password"`
	}
	c.do("POST", "/api/v1/customers", map[string]string{"email": "f@example.com", "name": "F", "package_id": pkg.ID}, &cust)
	cc := login(t, p.web.URL, "f@example.com", cust.Password)
	var mine struct {
		ID string `json:"id"`
	}
	cc.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "mine.example.com", "kind": "static"}, &mine)
	if st := cc.do("POST", "/api/v1/sites/"+mine.ID+"/ftp", map[string]bool{"enabled": true}, &e); st != http.StatusForbidden || e.Code != "ftp_not_allowed" {
		t.Errorf("customer without FTP: %d %s", st, e.Code)
	}
}
