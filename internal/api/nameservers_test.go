package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"testing"

	"ekpanel/internal/agentproto"
)

func TestHostedDNS(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	serverID, cfg, _, _ := enrolled(t, p, c, "ns-1")
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
		if ns := in.NameServer; ns != nil {
			out.NameServer = &agentproto.NameServerResult{Active: true, Version: "4.8.3", Zones: []agentproto.HostedZoneResult{}}
			for _, z := range ns.Zones {
				r := agentproto.HostedZoneResult{Name: z.Name, Serial: 7}
				if z.DNSSEC {
					r.DS = []string{"1234 13 2 abcd"}
				}
				out.NameServer.Zones = append(out.NameServer.Zones, r)
			}
		}
		return out, nil
	})
	sent := func() agentproto.StateApplyParams {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
	zone := func(name string) *agentproto.HostedZoneSpec {
		if ns := sent().NameServer; ns != nil {
			for i, z := range ns.Zones {
				if z.Name == name {
					return &ns.Zones[i]
				}
			}
		}
		return nil
	}
	has := func(z *agentproto.HostedZoneSpec, name, typ, content string) bool {
		return z != nil && slices.ContainsFunc(z.Records, func(r agentproto.HostedRecord) bool { return r.Name == name && r.Type == typ && r.Content == content })
	}

	c.do("PUT", "/api/v1/servers/"+serverID+"/addresses", map[string]string{"public_ipv4": "203.0.113.10"}, nil)
	c.do("PUT", "/api/v1/servers/"+serverID+"/firewall", map[string]any{"mode": "on"}, nil)
	var e errBody
	if st := c.do("POST", "/api/v1/dns/hosted", map[string]string{"name": "example.com"}, &e); st != http.StatusConflict || e.Code != "no_name_servers" {
		t.Errorf("zone without name servers: %d %s", st, e.Code)
	}
	for _, bad := range []any{
		[]map[string]any{{"server_id": serverID, "hostname": "ns1.example.com"}},                                           // no primary
		[]map[string]any{{"server_id": serverID, "hostname": "not a host", "primary": true}},                               // bad name
		[]map[string]any{{"server_id": serverID, "hostname": "ns1.example.com", "primary": true, "transfer_address": "x"}}, // bad address
	} {
		if st := c.do("PUT", "/api/v1/dns/nameservers", bad, &e); st != http.StatusBadRequest {
			t.Errorf("%v: %d", bad, st)
		}
	}
	var ns []struct {
		Hostname string `json:"hostname"`
		IPv4     string `json:"ipv4"`
	}
	if st := c.do("PUT", "/api/v1/dns/nameservers", []map[string]any{{"server_id": serverID, "hostname": "NS1.example.com", "primary": true}}, &ns); st != http.StatusOK ||
		len(ns) != 1 || ns[0].Hostname != "ns1.example.com" || ns[0].IPv4 != "203.0.113.10" {
		t.Fatalf("name servers: %d %+v", st, ns)
	}

	// A site on the domain, then the zone.
	var site struct {
		ID string `json:"id"`
	}
	c.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "www.example.com", "kind": "static"}, &site)
	for _, bad := range []string{"com", "co.uk", "not a domain", "1.2.3.4"} {
		if st := c.do("POST", "/api/v1/dns/hosted", map[string]string{"name": bad}, &e); st != http.StatusBadRequest {
			t.Errorf("zone %q: %d", bad, st)
		}
	}
	var tpl struct {
		ID string `json:"id"`
	}
	if st := c.do("POST", "/api/v1/dns/templates", map[string]any{"name": "Basic", "records": []map[string]any{
		{"name": "@", "type": "TXT", "content": "v=DMARC1; p=none; rua=mailto:dmarc@{domain}"},
		{"name": "ftp", "type": "CNAME", "content": "{domain}"},
	}}, &tpl); st != http.StatusCreated {
		t.Fatalf("template: %d", st)
	}
	if st := c.do("POST", "/api/v1/dns/templates", map[string]any{"name": "Bad", "records": []map[string]any{{"name": "@", "type": "CNAME", "content": "x.com"}}}, &e); st != http.StatusBadRequest {
		t.Errorf("a template with a CNAME at the domain: %d", st)
	}
	var z struct {
		ID string `json:"id"`
	}
	if st := c.do("POST", "/api/v1/dns/hosted", map[string]string{"name": "Example.com.", "template_id": tpl.ID}, &z); st != http.StatusCreated {
		t.Fatalf("zone: %d", st)
	}
	if st := c.do("POST", "/api/v1/dns/hosted", map[string]string{"name": "example.com"}, &e); st != http.StatusConflict || e.Code != "zone_taken" {
		t.Errorf("the same zone again: %d %s", st, e.Code)
	}
	eventually(t, "zone sent", func() bool {
		z := zone("example.com")
		return has(z, "example.com", "NS", "ns1.example.com") && has(z, "ns1.example.com", "A", "203.0.113.10") &&
			has(z, "www.example.com", "A", "203.0.113.10") && has(z, "ftp.example.com", "CNAME", "example.com") &&
			has(z, "example.com", "TXT", "v=DMARC1; p=none; rua=mailto:dmarc@example.com") && z.PrimaryNS == "ns1.example.com"
	})
	if s := sent(); s.NameServer == nil || !s.NameServer.Primary || s.Firewall == nil || !s.Firewall.DNS {
		t.Errorf("primary and firewall: %+v %+v", s.NameServer, s.Firewall)
	}

	// Records: a kept A record on www is in the way of the site's.
	recs := "/api/v1/dns/hosted/" + z.ID + "/records"
	for _, bad := range []map[string]any{
		{"name": "@", "type": "CNAME", "content": "other.com"},
		{"name": "other.net.", "type": "A", "content": "198.51.100.1"},
		{"name": "ftp", "type": "A", "content": "198.51.100.1"}, // next to the CNAME
		{"name": "@", "type": "NS", "content": "ns.other.net"},
	} {
		if st := c.do("POST", recs, bad, &e); st != http.StatusBadRequest {
			t.Errorf("%v: %d %s", bad, st, e.Code)
		}
	}
	var rec struct {
		ID string `json:"id"`
	}
	if st := c.do("POST", recs, map[string]any{"name": "www", "type": "A", "content": "198.51.100.9", "ttl": 600}, &rec); st != http.StatusCreated {
		t.Fatalf("record: %d", st)
	}
	if st := c.do("POST", recs, map[string]any{"name": "www", "type": "A", "content": "198.51.100.9", "ttl": 600}, &e); st != http.StatusConflict {
		t.Errorf("the same record twice: %d", st)
	}
	var view struct {
		Kept   []struct{ ID, Name, Content string } `json:"kept"`
		Made   []agentproto.HostedRecord            `json:"made"`
		Issues []struct{ Kind, Name string }        `json:"issues"`
		Serial int64                                `json:"serial"`
		DS     []string                             `json:"ds"`
	}
	eventually(t, "kept record wins", func() bool {
		z := zone("example.com")
		return has(z, "www.example.com", "A", "198.51.100.9") && !has(z, "www.example.com", "A", "203.0.113.10")
	})
	c.do("GET", "/api/v1/dns/hosted/"+z.ID, nil, &view)
	if len(view.Kept) != 3 || len(view.Issues) != 1 || view.Issues[0].Kind != "conflict" || view.Serial != 7 {
		t.Errorf("view: %+v", view)
	}
	c.do("PUT", recs+"/"+rec.ID, map[string]any{"name": "mail", "type": "A", "content": "198.51.100.9"}, nil)
	eventually(t, "record changed", func() bool {
		z := zone("example.com")
		return has(z, "mail.example.com", "A", "198.51.100.9") && has(z, "www.example.com", "A", "203.0.113.10")
	})

	// DNSSEC: the DS records come back from the primary.
	c.do("PATCH", "/api/v1/dns/hosted/"+z.ID, map[string]bool{"dnssec": true}, nil)
	eventually(t, "DS recorded", func() bool {
		c.do("GET", "/api/v1/dns/hosted/"+z.ID, nil, &view)
		return len(view.DS) == 1 && view.DS[0] == "1234 13 2 abcd"
	})

	// Customers: zones for their own sites' domains only, and not inside another's zone.
	var pkg struct {
		ID string `json:"id"`
	}
	c.do("POST", "/api/v1/packages", map[string]any{"name": "Basic", "max_sites": 2}, &pkg)
	var cust struct {
		Password string `json:"password"`
	}
	c.do("POST", "/api/v1/customers", map[string]string{"email": "d@example.com", "name": "D", "package_id": pkg.ID}, &cust)
	cc := login(t, p.web.URL, "d@example.com", cust.Password)
	if st := cc.do("POST", "/api/v1/dns/hosted", map[string]string{"name": "mine.net"}, &e); st != http.StatusForbidden || e.Code != "zone_not_owned" {
		t.Errorf("customer zone without a site: %d %s", st, e.Code)
	}
	cc.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "shop.mine.net", "kind": "static"}, nil)
	cc.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "blog.example.com", "kind": "static"}, nil)
	if st := cc.do("POST", "/api/v1/dns/hosted", map[string]string{"name": "blog.example.com"}, &e); st != http.StatusConflict || e.Code != "zone_overlaps" {
		t.Errorf("customer zone inside the administrators': %d %s", st, e.Code)
	}
	var mine struct {
		ID string `json:"id"`
	}
	if st := cc.do("POST", "/api/v1/dns/hosted", map[string]string{"name": "mine.net"}, &mine); st != http.StatusCreated {
		t.Fatalf("customer zone: %d", st)
	}
	var list []struct{ Name string }
	if cc.do("GET", "/api/v1/dns/hosted", nil, &list); len(list) != 1 || list[0].Name != "mine.net" {
		t.Errorf("customer list: %+v", list)
	}
	if st := cc.do("GET", "/api/v1/dns/hosted/"+z.ID, nil, &e); st != http.StatusNotFound {
		t.Errorf("customer reading the administrators' zone: %d", st)
	}
	if st := cc.do("PUT", "/api/v1/dns/nameservers", []map[string]any{}, &e); st != http.StatusForbidden {
		t.Errorf("customer setting name servers: %d", st)
	}
	eventually(t, "customer zone sent", func() bool { return has(zone("mine.net"), "shop.mine.net", "A", "203.0.113.10") })

	// Removing the zone and then the name servers.
	c.do("DELETE", "/api/v1/dns/hosted/"+z.ID, nil, nil)
	eventually(t, "zone removed", func() bool { return zone("example.com") == nil && zone("mine.net") != nil })
	c.do("PUT", "/api/v1/dns/nameservers", []map[string]any{}, nil)
	eventually(t, "no longer a name server", func() bool { s := sent(); return s.NameServer == nil && s.Firewall != nil && !s.Firewall.DNS })
}
