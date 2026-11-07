package api_test

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"ekpanel/internal/agentproto"
)

func TestSecurity(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	serverID, cfg, _, _ := enrolled(t, p, c, "sec-1")
	tlsCfg, _ := cfg.TLSConfig()
	conn, _, err := dialAgent(t, p, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	var mu sync.Mutex
	var spec *agentproto.SecuritySpec
	var banned []agentproto.BanParams
	fakeAgent(t, conn, func(op string, raw json.RawMessage) (any, *agentproto.OpError) {
		mu.Lock()
		defer mu.Unlock()
		switch op {
		case agentproto.OpStateApply:
			var in agentproto.StateApplyParams
			json.Unmarshal(raw, &in)
			spec = in.Security
			res := agentproto.StateApplyResult{}
			if in.Security != nil {
				res.Security = &agentproto.SecurityResult{CrowdSec: agentproto.CrowdSecStatus{Installed: in.Security.CrowdSec, Active: in.Security.CrowdSec, Version: "1.8.2"}}
			}
			return res, nil
		case agentproto.OpSecurityBan:
			var b agentproto.BanParams
			json.Unmarshal(raw, &b)
			banned = append(banned, b)
			return agentproto.BansResult{Bans: []agentproto.Ban{{IP: b.IP, Reason: b.Reason, Manually: true}}}, nil
		case agentproto.OpSecurityUnban:
			return agentproto.BansResult{Bans: []agentproto.Ban{}}, nil
		case agentproto.OpSecurityOverview:
			return agentproto.SecurityOverview{SSH: agentproto.SSHSettings{Ports: []int{22}, RootLogin: "yes", PasswordAuth: true}}, nil
		}
		return nil, &agentproto.OpError{Code: agentproto.ErrUnknownOp}
	})

	var e errBody
	if st := c.do("PUT", "/api/v1/servers/"+serverID+"/security", map[string]any{"crowdsec": true, "allowlist": []string{"not an address"}}, &e); st != http.StatusBadRequest {
		t.Errorf("bad allowlist: %d", st)
	}
	var view struct {
		CrowdSec  bool     `json:"crowdsec"`
		Allowlist []string `json:"allowlist"`
		Always    []string `json:"always"`
		Status    *agentproto.SecurityResult
	}
	if st := c.do("PUT", "/api/v1/servers/"+serverID+"/security", map[string]any{"crowdsec": true,
		"allowlist": []string{" 203.0.113.9 ", "198.51.100.7/24", "203.0.113.9"}}, &view); st != http.StatusOK || !view.CrowdSec ||
		len(view.Allowlist) != 2 || view.Allowlist[1] != "198.51.100.0/24" || len(view.Always) < 20 {
		t.Fatalf("set: %d %+v", st, view)
	}
	eventually(t, "CrowdSec applied", func() bool {
		c.do("GET", "/api/v1/servers/"+serverID+"/security", nil, &view)
		return view.Status != nil && view.Status.CrowdSec.Active
	})
	mu.Lock()
	if spec == nil || !spec.CrowdSec || len(spec.Allowlist) != 2 {
		t.Errorf("sent: %+v", spec)
	}
	mu.Unlock()

	for _, bad := range []map[string]string{
		{"ip": "10.0.0.5", "duration": "4h", "reason": "x"},
		{"ip": "198.51.100.0/24", "duration": "4h", "reason": "x"},
		{"ip": "198.51.100.5", "duration": "forever", "reason": "x"},
		{"ip": "198.51.100.5", "duration": "4h", "reason": "two\nlines"},
	} {
		if st := c.do("POST", "/api/v1/servers/"+serverID+"/security/bans", bad, &e); st != http.StatusBadRequest {
			t.Errorf("ban %v: %d", bad, st)
		}
	}
	var bans agentproto.BansResult
	if st := c.do("POST", "/api/v1/servers/"+serverID+"/security/bans", map[string]string{"ip": "198.51.100.5", "duration": "24h", "reason": "Spam"}, &bans); st != http.StatusOK || len(bans.Bans) != 1 {
		t.Errorf("ban: %d %+v", st, bans)
	}
	if st := c.do("DELETE", "/api/v1/servers/"+serverID+"/security/bans/198.51.100.5", nil, &bans); st != http.StatusOK || len(bans.Bans) != 0 {
		t.Errorf("unban: %d %+v", st, bans)
	}
	var overview struct {
		Server agentproto.SecurityOverview `json:"server"`
		Admins []string                    `json:"admins_without_2fa"`
	}
	if st := c.do("GET", "/api/v1/servers/"+serverID+"/security/overview", nil, &overview); st != http.StatusOK || overview.Server.SSH.RootLogin != "yes" ||
		len(overview.Admins) != 1 || overview.Admins[0] != "admin@example.com" {
		t.Errorf("overview: %d %+v", st, overview)
	}

	// Customers have no say over a server's security.
	var pkg struct {
		ID string `json:"id"`
	}
	c.do("POST", "/api/v1/packages", map[string]any{"name": "Basic", "max_sites": 1}, &pkg)
	var cust struct {
		Password string `json:"password"`
	}
	c.do("POST", "/api/v1/customers", map[string]string{"email": "c@example.com", "name": "C", "package_id": pkg.ID}, &cust)
	cc := login(t, p.web.URL, "c@example.com", cust.Password)
	if st := cc.do("GET", "/api/v1/servers/"+serverID+"/security/bans", nil, &e); st != http.StatusForbidden {
		t.Errorf("customer: %d", st)
	}
}
