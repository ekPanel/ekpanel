package api_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"ekpanel/internal/cloudflare"
	"ekpanel/internal/cloudflare/cloudflaretest"
)

type zoneView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Records   int    `json:"records"`
	SyncError *string
	SyncedAt  *time.Time `json:"synced_at"`
	Issues    []struct {
		Kind, Name, Type, Want, Have string
	} `json:"issues"`
}

// syncZone runs a sync of the zone and waits for it.
func syncZone(t *testing.T, c *client, id string) zoneView {
	t.Helper()
	time.Sleep(20 * time.Millisecond) // a sync already queued may be running; wait for a later one
	start := time.Now()
	c.do("POST", "/api/v1/dns/zones/"+id+"/sync", nil, nil)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		var list []zoneView
		c.do("GET", "/api/v1/dns/zones", nil, &list)
		for _, z := range list {
			if z.ID == id && z.SyncedAt != nil && z.SyncedAt.After(start) {
				return z
			}
		}
	}
	t.Fatalf("zone %s did not sync", id)
	return zoneView{}
}

func findRecord(records []cloudflare.Record, typ, name string) *cloudflare.Record {
	i := slices.IndexFunc(records, func(r cloudflare.Record) bool { return r.Type == typ && r.Name == name })
	if i < 0 {
		return nil
	}
	return &records[i]
}

func TestCloudflareDNS(t *testing.T) {
	fake := cloudflaretest.New("good-token")
	cf := httptest.NewServer(fake)
	defer cf.Close()
	t.Setenv("EKPANEL_CLOUDFLARE_API", cf.URL)

	p := newTestPanel(t)
	admin := newClient(t, p.web.URL)
	setUp(t, admin)
	serverID, _, _, _ := enrolled(t, p, admin, "web-1")
	var e errBody
	if st := admin.do("PUT", "/api/v1/servers/"+serverID+"/addresses", map[string]string{"public_ipv4": "2001:db8::1"}, &e); st != http.StatusBadRequest {
		t.Errorf("IPv6 as IPv4: %d", st)
	}
	admin.do("PUT", "/api/v1/servers/"+serverID+"/addresses", map[string]string{"public_ipv4": "203.0.113.10", "public_ipv6": "2001:db8::10"}, nil)

	// Connecting checks the token.
	if st := admin.do("POST", "/api/v1/dns/cloudflare", map[string]string{"token": "wrong"}, &e); st != http.StatusBadGateway || e.Code != "cloudflare_denied" {
		t.Fatalf("bad token: %d %s", st, e.Code)
	}
	var acct struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		AccountID string `json:"account_id"`
	}
	if st := admin.do("POST", "/api/v1/dns/cloudflare", map[string]string{"token": "good-token"}, &acct); st != http.StatusCreated || acct.Name != "Test account" || acct.AccountID != "acc1" {
		t.Fatalf("connect: %d %+v", st, acct)
	}

	// The zone already has records of its own, and one in the way.
	zid := fake.AddZone("example.com")
	mx := 10
	fake.SetRecord(zid, cloudflare.Record{Type: "MX", Name: "example.com", Content: "mx.mailhost.net", Priority: &mx})
	fake.SetRecord(zid, cloudflare.Record{Type: "A", Name: "taken.example.com", Content: "192.0.2.1"})
	var site, taken, other struct {
		ID string `json:"id"`
	}
	admin.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "shop.example.com", "aliases": []string{"www.shop.example.com"}, "kind": "static"}, &site)
	admin.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "taken.example.com", "kind": "static"}, &taken)
	admin.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "other.org", "kind": "static"}, &other)
	admin.do("POST", "/api/v1/apps", map[string]any{"server_id": serverID, "name": "web", "kind": "image", "image": "nginx", "container_port": 80, "domain": "app.example.com"}, nil)

	var listed []struct {
		ID        string  `json:"id"`
		Name      string  `json:"name"`
		ManagedID *string `json:"managed_id"`
	}
	admin.do("GET", "/api/v1/dns/cloudflare/"+acct.ID+"/zones", nil, &listed)
	if len(listed) != 1 || listed[0].ID != zid || listed[0].ManagedID != nil {
		t.Fatalf("zones of the account: %+v", listed)
	}
	var zone zoneView
	if st := admin.do("POST", "/api/v1/dns/zones", map[string]any{"account_id": acct.ID, "cf_zone_id": zid}, &zone); st != http.StatusCreated || zone.Name != "example.com" {
		t.Fatalf("manage: %d %+v", st, zone)
	}
	zone = syncZone(t, admin, zone.ID)
	recs := fake.Records(zid)
	for _, want := range [][3]string{{"A", "shop.example.com", "203.0.113.10"}, {"AAAA", "shop.example.com", "2001:db8::10"},
		{"A", "www.shop.example.com", "203.0.113.10"}, {"A", "app.example.com", "203.0.113.10"}} {
		r := findRecord(recs, want[0], want[1])
		if r == nil || r.Content != want[2] || !r.Proxied || r.Comment != cloudflare.ManagedComment {
			t.Errorf("record %v: %+v", want, r)
		}
	}
	if findRecord(recs, "A", "other.org") != nil || findRecord(recs, "MX", "example.com") == nil || findRecord(recs, "A", "taken.example.com").Content != "192.0.2.1" {
		t.Errorf("records of others must stay as they are: %+v", recs)
	}
	if len(zone.Issues) != 1 || zone.Issues[0].Kind != "conflict" || zone.Issues[0].Name != "taken.example.com" || zone.SyncError != nil {
		t.Fatalf("issues: %+v %v", zone.Issues, zone.SyncError)
	}

	// Resolving writes ekPanel's value.
	admin.do("POST", "/api/v1/dns/zones/"+zone.ID+"/resolve", map[string]string{"type": "A", "name": "taken.example.com"}, &zone)
	if r := findRecord(fake.Records(zid), "A", "taken.example.com"); r == nil || r.Content != "203.0.113.10" || len(zone.Issues) != 0 {
		t.Fatalf("resolved: %+v %+v", r, zone.Issues)
	}

	// The server moves: records follow. A record changed by hand is reported, not overwritten.
	admin.do("PUT", "/api/v1/servers/"+serverID+"/addresses", map[string]string{"public_ipv4": "203.0.113.20"}, nil)
	r := findRecord(fake.Records(zid), "A", "app.example.com")
	r.Content = "198.51.100.5"
	fake.SetRecord(zid, *r)
	zone = syncZone(t, admin, zone.ID)
	recs = fake.Records(zid)
	if findRecord(recs, "A", "shop.example.com").Content != "203.0.113.20" || findRecord(recs, "AAAA", "shop.example.com") != nil {
		t.Errorf("records after the move (no IPv6 any more): %+v", recs)
	}
	if findRecord(recs, "A", "app.example.com").Content != "198.51.100.5" || len(zone.Issues) != 1 || zone.Issues[0].Kind != "changed" {
		t.Errorf("hand change: %+v", zone.Issues)
	}

	// Deleting a site removes its records.
	admin.do("DELETE", "/api/v1/sites/"+site.ID, nil, nil)
	syncZone(t, admin, zone.ID)
	if findRecord(fake.Records(zid), "A", "shop.example.com") != nil || findRecord(fake.Records(zid), "A", "www.shop.example.com") != nil {
		t.Errorf("records of a deleted site: %+v", fake.Records(zid))
	}

	// The zone editor: any record of ours can switch its proxy; others are edited freely.
	var view struct {
		Records []struct {
			cloudflare.Record
			Managed bool `json:"managed"`
		} `json:"records"`
		SSLMode string `json:"ssl_mode"`
	}
	admin.do("GET", "/api/v1/dns/zones/"+zone.ID, nil, &view)
	var managedID string
	for _, rec := range view.Records {
		if rec.Managed && rec.Name == "taken.example.com" {
			managedID = rec.ID
		}
	}
	if managedID == "" || view.SSLMode != "flexible" {
		t.Fatalf("zone view: %+v", view)
	}
	if st := admin.do("DELETE", "/api/v1/dns/zones/"+zone.ID+"/records/"+managedID, nil, &e); st != http.StatusConflict || e.Code != "managed_record" {
		t.Errorf("deleting ekPanel's record: %d %s", st, e.Code)
	}
	admin.do("PUT", "/api/v1/dns/zones/"+zone.ID+"/records/"+managedID, map[string]any{"proxied": false, "content": "6.6.6.6"}, nil)
	if r := findRecord(fake.Records(zid), "A", "taken.example.com"); r.Proxied || r.Content != "203.0.113.20" {
		t.Errorf("only the proxy of ekPanel's record changes: %+v", r)
	}
	if z := syncZone(t, admin, zone.ID); findRecord(fake.Records(zid), "A", "taken.example.com").Proxied || len(z.Issues) != 1 {
		t.Errorf("a sync keeps the chosen proxy: %+v", z.Issues)
	}
	var txt cloudflare.Record
	for _, bad := range []map[string]any{{"type": "A", "name": "x", "content": "not-an-ip"}, {"type": "SRV", "name": "x", "content": "1 2 3 x"}, {"type": "MX", "name": "@", "content": "mx.example.com"}} {
		if st := admin.do("POST", "/api/v1/dns/zones/"+zone.ID+"/records", bad, &e); st != http.StatusBadRequest {
			t.Errorf("record %v: %d", bad, st)
		}
	}
	if st := admin.do("POST", "/api/v1/dns/zones/"+zone.ID+"/records", map[string]any{"type": "TXT", "name": "@", "content": "v=spf1 -all"}, &txt); st != http.StatusCreated || txt.Name != "example.com" {
		t.Fatalf("add TXT: %d %+v", st, txt)
	}
	if st := admin.do("DELETE", "/api/v1/dns/zones/"+zone.ID+"/records/"+txt.ID, nil, nil); st != http.StatusNoContent {
		t.Errorf("delete TXT: %d", st)
	}
	admin.do("POST", "/api/v1/dns/zones/"+zone.ID+"/ssl", map[string]string{"mode": "strict"}, nil)
	admin.do("POST", "/api/v1/dns/zones/"+zone.ID+"/purge", nil, nil)
	if fake.SSL(zid) != "strict" || fake.Purges(zid) != 1 {
		t.Errorf("ssl %s, purges %d", fake.SSL(zid), fake.Purges(zid))
	}

	// Customers do not see the administrators' accounts and zones.
	var pkg struct {
		ID string `json:"id"`
	}
	admin.do("POST", "/api/v1/packages", map[string]any{"name": "P", "max_sites": 1}, &pkg)
	var c struct {
		Password string `json:"password"`
	}
	admin.do("POST", "/api/v1/customers", map[string]string{"email": "c@example.com", "name": "C", "package_id": pkg.ID}, &c)
	cust := login(t, p.web.URL, "c@example.com", c.Password)
	var mine []zoneView
	cust.do("GET", "/api/v1/dns/zones", nil, &mine)
	if len(mine) != 0 {
		t.Errorf("customer sees zones: %+v", mine)
	}
	if st := cust.do("GET", "/api/v1/dns/zones/"+zone.ID, nil, &e); st != http.StatusNotFound {
		t.Errorf("customer reads the admin's zone: %d", st)
	}
	if st := cust.do("POST", "/api/v1/dns/zones", map[string]any{"account_id": acct.ID, "cf_zone_id": zid}, &e); st != http.StatusNotFound {
		t.Errorf("customer uses the admin's account: %d", st)
	}

	// Stopping, with the records ekPanel made.
	if st := admin.do("DELETE", "/api/v1/dns/zones/"+zone.ID+"?records=delete", nil, nil); st != http.StatusNoContent {
		t.Fatalf("remove: %d", st)
	}
	recs = fake.Records(zid)
	if findRecord(recs, "A", "taken.example.com") != nil || findRecord(recs, "MX", "example.com") == nil || findRecord(recs, "A", "app.example.com") == nil {
		t.Errorf("after removing (ekPanel's go, others and the hand-changed one stay): %+v", recs)
	}
}
