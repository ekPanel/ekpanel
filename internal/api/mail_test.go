package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/auth"
	"ekpanel/internal/cloudflare"
	"ekpanel/internal/cloudflare/cloudflaretest"
)

func TestEmail(t *testing.T) {
	fake := cloudflaretest.New("good-token")
	cf := httptest.NewServer(fake)
	defer cf.Close()
	t.Setenv("EKPANEL_CLOUDFLARE_API", cf.URL)

	p := newTestPanel(t)
	admin := newClient(t, p.web.URL)
	setUp(t, admin)
	serverID, cfg, _, _ := enrolled(t, p, admin, "mail-1")
	tlsCfg, _ := cfg.TLSConfig()
	conn, _, err := dialAgent(t, p, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	// The agent reports for each domain the records Stalwart would, and some usage.
	var mu sync.Mutex
	var last *agentproto.MailSpec
	var firewall *agentproto.FirewallSpec
	fakeAgent(t, conn, func(op string, raw json.RawMessage) (any, *agentproto.OpError) {
		switch op {
		case agentproto.OpMailCheck:
			return agentproto.MailCheckResult{Hostname: "mail.example.com", Port25: "open", HostnameAddresses: []string{"203.0.113.10"}}, nil
		case agentproto.OpStateApply:
		default:
			return nil, &agentproto.OpError{Code: agentproto.ErrUnknownOp}
		}
		var in agentproto.StateApplyParams
		json.Unmarshal(raw, &in)
		mu.Lock()
		last, firewall = in.Mail, in.Firewall
		mu.Unlock()
		var out agentproto.StateApplyResult
		if in.Mail != nil {
			out.Mail = &agentproto.MailResult{OK: true}
			for _, d := range in.Mail.Domains {
				if err := d.Validate(); err != nil {
					out.Mail.Domains = append(out.Mail.Domains, agentproto.MailDomainResult{ID: d.ID, Generation: d.Generation, Error: err.Error()})
					continue
				}
				r := agentproto.MailDomainResult{ID: d.ID, Generation: d.Generation, OK: true, Usage: map[string]int64{}, DNS: []agentproto.MailDNSRecord{
					{Type: "MX", Name: d.Domain, Content: in.Mail.Hostname, Priority: 10},
					{Type: "TXT", Name: d.Domain, Content: "v=spf1 mx -all"},
					{Type: "TXT", Name: "v1-rsa-20261107._domainkey." + d.Domain, Content: "v=DKIM1; k=rsa; p=MIIB"},
					{Type: "TXT", Name: "_dmarc." + d.Domain, Content: "v=DMARC1; p=reject"},
				}}
				for _, mb := range d.Mailboxes {
					r.Usage[mb.ID] = 1 << 20
				}
				out.Mail.Domains = append(out.Mail.Domains, r)
			}
		}
		return out, nil
	})
	spec := func() *agentproto.MailSpec {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
	// waitDomain polls a domain until check passes.
	type domainView struct {
		Domain struct {
			ID       string                     `json:"id"`
			State    string                     `json:"state"`
			CatchAll *string                    `json:"catch_all"`
			Records  []agentproto.MailDNSRecord `json:"dns_records"`
		} `json:"domain"`
		Mailboxes []struct {
			ID        string `json:"id"`
			LocalPart string `json:"local_part"`
			QuotaMB   int    `json:"quota_mb"`
			UsedBytes *int64 `json:"used_bytes"`
		} `json:"mailboxes"`
		Forwarders []struct {
			ID string `json:"id"`
		} `json:"forwarders"`
		Zone *struct {
			ID string `json:"id"`
		} `json:"zone"`
	}
	waitDomain := func(c *client, id string, check func(domainView) bool) domainView {
		t.Helper()
		var v domainView
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			v = domainView{}
			if c.do("GET", "/api/v1/mail/domains/"+id, nil, &v) == http.StatusOK && check(v) {
				return v
			}
		}
		t.Fatalf("domain %s: %+v", id, v)
		return v
	}

	admin.do("PUT", "/api/v1/servers/"+serverID+"/addresses", map[string]string{"public_ipv4": "203.0.113.10"}, nil)
	admin.do("PUT", "/api/v1/servers/"+serverID+"/firewall", map[string]any{"mode": "on"}, nil)
	var e errBody
	var ms struct {
		Hostname string `json:"hostname"`
	}
	if st := admin.do("PUT", "/api/v1/servers/"+serverID+"/mail", map[string]string{"hostname": "localhost"}, &e); st != http.StatusBadRequest {
		t.Errorf("bad host name: %d", st)
	}
	if st := admin.do("PUT", "/api/v1/servers/"+serverID+"/mail", map[string]string{"hostname": "Mail.Example.com"}, &ms); st != http.StatusOK || ms.Hostname != "mail.example.com" {
		t.Fatalf("turn on email: %d %+v", st, ms)
	}

	var dom struct {
		ID string `json:"id"`
	}
	if st := admin.do("POST", "/api/v1/mail/domains", map[string]string{"server_id": serverID, "domain": "mail.example.com"}, &e); st != http.StatusBadRequest {
		t.Errorf("the host name as a domain: %d", st)
	}
	if st := admin.do("POST", "/api/v1/mail/domains", map[string]string{"server_id": serverID, "domain": "example.com"}, &dom); st != http.StatusCreated {
		t.Fatalf("domain: %d", st)
	}
	if st := admin.do("POST", "/api/v1/mail/domains", map[string]string{"server_id": serverID, "domain": "example.com"}, &e); st != http.StatusConflict || e.Code != "mail_domain_taken" {
		t.Errorf("domain twice: %d %s", st, e.Code)
	}

	// Mailboxes: a chosen password, or one made by the panel and shown once.
	type mailboxResp struct {
		Mailbox struct {
			ID string `json:"id"`
		} `json:"mailbox"`
		Password string `json:"password"`
	}
	var info, sales mailboxResp
	if st := admin.do("POST", "/api/v1/mail/domains/"+dom.ID+"/mailboxes", map[string]any{"local_part": "info", "password": "short"}, &e); st != http.StatusBadRequest || e.Code != "weak_password" {
		t.Errorf("weak password: %d %s", st, e.Code)
	}
	if st := admin.do("POST", "/api/v1/mail/domains/"+dom.ID+"/mailboxes", map[string]any{"local_part": "In..fo"}, &e); st != http.StatusBadRequest || e.Code != "invalid_local_part" {
		t.Errorf("bad name: %d %s", st, e.Code)
	}
	if st := admin.do("POST", "/api/v1/mail/domains/"+dom.ID+"/mailboxes", map[string]any{"local_part": "Info", "quota_mb": 500}, &info); st != http.StatusCreated || info.Password == "" {
		t.Fatalf("mailbox: %d %+v", st, info)
	}
	if st := admin.do("POST", "/api/v1/mail/domains/"+dom.ID+"/mailboxes", map[string]any{"local_part": "sales", "password": "a-long-sales-password"}, &sales); st != http.StatusCreated || sales.Password != "" {
		t.Fatalf("mailbox with a chosen password: %d %+v", st, sales)
	}
	if st := admin.do("PATCH", "/api/v1/mail/mailboxes/"+sales.Mailbox.ID, map[string]any{"keep_copy": false}, &e); st != http.StatusBadRequest || e.Code != "invalid_forward" {
		t.Errorf("keeping nothing: %d %s", st, e.Code)
	}
	if st := admin.do("PATCH", "/api/v1/mail/mailboxes/"+sales.Mailbox.ID, map[string]any{"forward_to": []string{"boss@other.org"}, "keep_copy": false,
		"auto_reply": map[string]string{"subject": "Thanks", "body": "We will answer soon.", "start": "2026-11-01"}}, nil); st != http.StatusOK {
		t.Errorf("forwarding: %d", st)
	}
	if st := admin.do("POST", "/api/v1/mail/domains/"+dom.ID+"/forwarders", map[string]any{"local_part": "info", "destinations": []string{"a@b.org"}}, &e); st != http.StatusConflict || e.Code != "mail_address_taken" {
		t.Errorf("forwarder over a mailbox: %d %s", st, e.Code)
	}
	if st := admin.do("POST", "/api/v1/mail/domains/"+dom.ID+"/forwarders", map[string]any{"local_part": "team", "destinations": []string{"a@b.org\nBcc: x@y.org"}}, &e); st != http.StatusBadRequest {
		t.Errorf("header in an address: %d", st)
	}
	if st := admin.do("POST", "/api/v1/mail/domains/"+dom.ID+"/forwarders", map[string]any{"local_part": "team", "destinations": []string{"info@example.com", "a@b.org"}}, nil); st != http.StatusCreated {
		t.Errorf("forwarder: %d", st)
	}
	if st := admin.do("PATCH", "/api/v1/mail/domains/"+dom.ID, map[string]any{"catch_all": "nobody"}, &e); st != http.StatusBadRequest {
		t.Errorf("catch-all to no mailbox: %d", st)
	}
	admin.do("PATCH", "/api/v1/mail/domains/"+dom.ID, map[string]any{"catch_all": "info"}, nil)

	v := waitDomain(admin, dom.ID, func(v domainView) bool { return v.Domain.State == "active" && len(v.Domain.Records) == 4 })
	if v.Domain.CatchAll == nil || *v.Domain.CatchAll != "info" || len(v.Mailboxes) != 2 || len(v.Forwarders) != 1 || v.Zone != nil {
		t.Errorf("domain: %+v", v)
	}
	if v.Mailboxes[0].UsedBytes == nil || *v.Mailboxes[0].UsedBytes != 1<<20 || v.Mailboxes[0].QuotaMB != 500 {
		t.Errorf("mailbox usage: %+v", v.Mailboxes[0])
	}
	got := spec()
	if got == nil || got.Hostname != "mail.example.com" || len(got.Domains) != 1 {
		t.Fatalf("mail spec: %+v", got)
	}
	d := got.Domains[0]
	if d.CatchAll != "info" || len(d.Mailboxes) != 2 || len(d.Forwarders) != 1 || d.Forwarders[0].Name != "team" {
		t.Errorf("domain spec: %+v", d)
	}
	for _, mb := range d.Mailboxes {
		switch mb.Name {
		case "info":
			if !auth.CheckLinuxPassword(mb.PasswordHash, info.Password) || mb.QuotaMB != 500 || !mb.KeepCopy {
				t.Errorf("info: %+v", mb)
			}
		case "sales":
			if mb.KeepCopy || len(mb.ForwardTo) != 1 || mb.AutoReply == nil || mb.AutoReply.Start != "2026-11-01" {
				t.Errorf("sales: %+v", mb)
			}
		}
	}
	mu.Lock()
	if firewall == nil || !firewall.Mail {
		t.Errorf("the firewall must open the mail ports: %+v", firewall)
	}
	mu.Unlock()

	// The deliverability check reports what the agent found and where DNS should point.
	var check struct {
		Check  agentproto.MailCheckResult `json:"check"`
		Public []string                   `json:"public_addresses"`
	}
	if st := admin.do("POST", "/api/v1/servers/"+serverID+"/mail/check", nil, &check); st != http.StatusOK || check.Check.Port25 != "open" || len(check.Public) != 1 {
		t.Errorf("check: %d %+v", st, check)
	}

	// With the zone in Cloudflare, the records are published, DNS only. Other TXT records
	// at the name stay.
	zid := fake.AddZone("example.com")
	fake.SetRecord(zid, cloudflare.Record{Type: "TXT", Name: "example.com", Content: `"google-site-verification=abc"`})
	var acct struct {
		ID string `json:"id"`
	}
	admin.do("POST", "/api/v1/dns/cloudflare", map[string]string{"token": "good-token"}, &acct)
	var zone zoneView
	admin.do("POST", "/api/v1/dns/zones", map[string]any{"account_id": acct.ID, "cf_zone_id": zid}, &zone)
	zone = syncZone(t, admin, zone.ID)
	recs := fake.Records(zid)
	for _, want := range [][3]string{{"MX", "example.com", "mail.example.com"}, {"TXT", "example.com", "v=spf1 mx -all"},
		{"TXT", "_dmarc.example.com", "v=DMARC1; p=reject"}, {"A", "mail.example.com", "203.0.113.10"}} {
		var found *cloudflare.Record
		for i, r := range recs {
			if r.Type == want[0] && r.Name == want[1] && r.Content == want[2] {
				found = &recs[i]
			}
		}
		if found == nil || found.Proxied {
			t.Errorf("record %v: %+v", want, found)
		}
	}
	if len(zone.Issues) != 0 || len(recs) != 6 {
		t.Errorf("issues %+v, records %+v", zone.Issues, recs)
	}
	waitDomain(admin, dom.ID, func(v domainView) bool { return v.Zone != nil && v.Zone.ID == zone.ID })

	// A customer may take mail only for their own websites' domains, within their package.
	var pkg struct {
		ID string `json:"id"`
	}
	admin.do("POST", "/api/v1/packages", map[string]any{"name": "Mail", "max_sites": 2, "max_mail_domains": 1, "max_mailboxes": 1, "mailbox_quota_mb": 100}, &pkg)
	var cust struct {
		Password string `json:"password"`
	}
	admin.do("POST", "/api/v1/customers", map[string]string{"email": "c@shop.test", "name": "Shop", "package_id": pkg.ID}, &cust)
	c := login(t, p.web.URL, "c@shop.test", cust.Password)
	c.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "www.shop.test", "kind": "static"}, nil)
	var servers []struct {
		Hostname string `json:"hostname"`
	}
	if c.do("GET", "/api/v1/mail/servers", nil, &servers); len(servers) != 1 || servers[0].Hostname != "mail.example.com" {
		t.Errorf("mail servers for a customer: %+v", servers)
	}
	if st := c.do("POST", "/api/v1/mail/domains", map[string]string{"server_id": serverID, "domain": "other.test"}, &e); st != http.StatusForbidden || e.Code != "mail_domain_not_owned" {
		t.Errorf("someone else's domain: %d %s", st, e.Code)
	}
	var cdom struct {
		ID string `json:"id"`
	}
	if st := c.do("POST", "/api/v1/mail/domains", map[string]string{"server_id": serverID, "domain": "shop.test"}, &cdom); st != http.StatusCreated {
		t.Fatalf("own domain: %d", st)
	}
	if st := c.do("POST", "/api/v1/mail/domains", map[string]string{"server_id": serverID, "domain": "www.shop.test"}, &e); st != http.StatusConflict || e.Code != "limit_reached_mail_domain" {
		t.Errorf("second domain: %d %s", st, e.Code)
	}
	if st := c.do("POST", "/api/v1/mail/domains/"+cdom.ID+"/mailboxes", map[string]any{"local_part": "me", "quota_mb": 0}, &e); st != http.StatusBadRequest || e.Code != "quota_too_large" {
		t.Errorf("unlimited mailbox: %d %s", st, e.Code)
	}
	var mine mailboxResp
	if st := c.do("POST", "/api/v1/mail/domains/"+cdom.ID+"/mailboxes", map[string]any{"local_part": "me"}, &mine); st != http.StatusCreated {
		t.Fatalf("customer mailbox: %d", st)
	}
	if st := c.do("POST", "/api/v1/mail/domains/"+cdom.ID+"/mailboxes", map[string]any{"local_part": "you"}, &e); st != http.StatusConflict || e.Code != "limit_reached_mailbox" {
		t.Errorf("second mailbox: %d %s", st, e.Code)
	}
	if st := c.do("GET", "/api/v1/mail/domains/"+dom.ID, nil, &e); st != http.StatusNotFound {
		t.Errorf("the administrators' domain seen by a customer: %d", st)
	}
	if st := c.do("DELETE", "/api/v1/mail/mailboxes/"+info.Mailbox.ID, nil, &e); st != http.StatusNotFound {
		t.Errorf("another's mailbox deleted by a customer: %d", st)
	}
	var list []struct {
		Domain string `json:"domain"`
	}
	if c.do("GET", "/api/v1/mail/domains", nil, &list); len(list) != 1 || list[0].Domain != "shop.test" {
		t.Errorf("customer's list: %+v", list)
	}

	// A mailbox goes, and the catch-all that pointed at it.
	if st := admin.do("DELETE", "/api/v1/mail/mailboxes/"+info.Mailbox.ID, nil, nil); st != http.StatusNoContent {
		t.Errorf("delete mailbox: %d", st)
	}
	waitDomain(admin, dom.ID, func(v domainView) bool { return v.Domain.CatchAll == nil && len(v.Mailboxes) == 1 })

	// Email cannot be turned off while domains remain; deleted domains go once applied.
	if st := admin.do("DELETE", "/api/v1/servers/"+serverID+"/mail", nil, &e); st != http.StatusConflict || e.Code != "mail_in_use" {
		t.Errorf("turn off with domains: %d %s", st, e.Code)
	}
	for _, id := range []string{dom.ID, cdom.ID} {
		if st := admin.do("DELETE", "/api/v1/mail/domains/"+id, nil, nil); st != http.StatusAccepted {
			t.Errorf("delete domain: %d", st)
		}
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		var all []struct{}
		admin.do("GET", "/api/v1/mail/domains", nil, &all)
		if len(all) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("domains not removed: %d left", len(all))
		}
	}
	if st := admin.do("DELETE", "/api/v1/servers/"+serverID+"/mail", nil, nil); st != http.StatusNoContent {
		t.Errorf("turn off: %d", st)
	}
	time.Sleep(200 * time.Millisecond)
	if s := spec(); s != nil {
		t.Errorf("email is off, yet the agent got %+v", s)
	}
	zone = syncZone(t, admin, zone.ID)
	for _, r := range fake.Records(zid) {
		if r.Comment == cloudflare.ManagedComment {
			t.Errorf("record left behind: %+v", r)
		}
	}
}
