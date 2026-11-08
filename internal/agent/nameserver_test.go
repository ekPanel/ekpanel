package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"ekpanel/internal/agentproto"
)

// fakePowerDNS keeps zones in memory and answers the parts of PowerDNS's API the agent uses.
type fakePowerDNS struct {
	mu    sync.Mutex
	zones map[string]*pdnsZone
	calls []string
}

func newFakePowerDNS(t *testing.T) *fakePowerDNS {
	f := &fakePowerDNS{zones: map[string]*pdnsZone{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	old := pdnsAPI
	pdnsAPI = srv.URL + "/api/v1/servers/localhost"
	t.Cleanup(func() { pdnsAPI = old })
	return f
}

func (f *fakePowerDNS) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("X-API-Key") == "" {
		http.Error(w, `{"error":"no key"}`, 401)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/servers/localhost")
	body, _ := io.ReadAll(r.Body)
	if r.Method != http.MethodGet {
		f.calls = append(f.calls, r.Method+" "+path)
	}
	reply := func(v any) { json.NewEncoder(w).Encode(v) }
	name, sub, _ := strings.Cut(strings.TrimPrefix(path, "/zones/"), "/")
	z := f.zones[name]
	switch {
	case path == "":
		reply(map[string]string{"version": "4.8.3"})
	case path == "/zones" && r.Method == http.MethodGet:
		out := []pdnsZone{}
		for _, z := range f.zones {
			c := *z
			c.RRsets = nil
			out = append(out, c)
		}
		reply(out)
	case path == "/zones" && r.Method == http.MethodPost:
		var nz pdnsZone
		json.Unmarshal(body, &nz)
		nz.Serial = 1
		if nz.Kind == "Slave" {
			nz.Serial = 0
		}
		f.zones[nz.Name] = &nz
		w.WriteHeader(201)
	case z == nil:
		http.Error(w, `{"error":"Could not find domain"}`, 404)
	case sub == "cryptokeys":
		reply([]map[string]any{{"active": true, "ds": []string{"1234 13 1 aa", "1234 13 2 bbbb", "1234 13 4 cc"}}})
	case sub == "axfr-retrieve":
		z.Serial = 5
	case sub == "notify":
	case sub == "metadata":
		reply([]map[string]any{{"kind": "SOA-EDIT-API", "metadata": []string{"DEFAULT"}}})
	case r.Method == http.MethodGet:
		reply(z)
	case r.Method == http.MethodDelete:
		delete(f.zones, name)
	case r.Method == http.MethodPut:
		var ch map[string]json.RawMessage
		json.Unmarshal(body, &ch)
		if v, ok := ch["dnssec"]; ok {
			json.Unmarshal(v, &z.DNSSEC)
		}
		if v, ok := ch["masters"]; ok {
			json.Unmarshal(v, &z.Masters)
		}
	case r.Method == http.MethodPatch:
		var ch struct {
			RRsets []pdnsRRset `json:"rrsets"`
		}
		json.Unmarshal(body, &ch)
		for _, c := range ch.RRsets {
			z.RRsets = slices.DeleteFunc(z.RRsets, func(s pdnsRRset) bool { return rrsetKey(s) == rrsetKey(c) })
			if c.ChangeType == "REPLACE" {
				c.ChangeType = ""
				z.RRsets = append(z.RRsets, c)
			}
		}
		z.Serial++
	}
}

func (f *fakePowerDNS) zone(name string) *pdnsZone {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.zones[name]
}

func nameServerSystem(t *testing.T) *fakeSystem {
	f := newFakeSystem(t)
	f.isActive = map[string]bool{"pdns": true}
	os.MkdirAll(f.path("/usr/share/pdns-backend-sqlite3/schema"), 0o755)
	os.WriteFile(f.path(pdnsSchema), []byte("CREATE TABLE domains (id INTEGER);"), 0o644)
	return f
}

func primarySpec() agentproto.NameServerSpec {
	return agentproto.NameServerSpec{Primary: true, Secondaries: []string{"192.0.2.53"}, Zones: []agentproto.HostedZoneSpec{{
		Name: "example.com", PrimaryNS: "ns1.example.net", Hostmaster: "hostmaster.example.com",
		Records: []agentproto.HostedRecord{
			{Name: "example.com", Type: "NS", Content: "ns1.example.net", TTL: 3600},
			{Name: "example.com", Type: "NS", Content: "ns2.example.net", TTL: 3600},
			{Name: "www.example.com", Type: "A", Content: "198.51.100.7", TTL: 300},
			{Name: "example.com", Type: "MX", Content: "mail.example.com", Priority: 10, TTL: 3600},
			{Name: "example.com", Type: "TXT", Content: `v=spf1 mx "-all"`, TTL: 3600},
		},
	}}}
}

func TestNameServerPrimary(t *testing.T) {
	f := nameServerSystem(t)
	pdns := newFakePowerDNS(t)
	ctx := context.Background()
	spec := primarySpec()
	res := applyNameServer(ctx, f.system, &spec)
	if res.Error != "" || !res.Active || res.Version != "4.8.3" || len(res.Zones) != 1 || res.Zones[0].Serial != 1 {
		t.Fatalf("first apply: %+v", res)
	}
	conf := f.read(t, pdnsConf)
	for _, want := range []string{"launch=gsqlite3", "primary=yes", "allow-axfr-ips=127.0.0.1,::1,192.0.2.53", "also-notify=192.0.2.53",
		"webserver-address=127.0.0.1", "local-address=127.0.0.1, ::1", "api-key=" + strings.TrimSpace(f.read(t, pdnsKeyFile))} {
		if !strings.Contains(conf, want) {
			t.Errorf("pdns.conf lacks %q:\n%s", want, conf)
		}
	}
	if info, _ := os.Stat(f.path(pdnsConf)); info.Mode().Perm() != 0o640 {
		t.Errorf("pdns.conf mode %v", info.Mode())
	}
	if !f.ran("apt-get -o DPkg::Lock::Timeout=300 install -y --no-install-recommends pdns-server pdns-backend-sqlite3 sqlite3") ||
		!f.ran("runuser -u pdns -- sqlite3 "+pdnsDB) || !f.ran("systemctl restart pdns") {
		t.Errorf("setup: %v", f.calls)
	}
	z := pdns.zone("example.com.")
	sets := map[string][]string{}
	for _, s := range z.RRsets {
		for _, r := range s.Records {
			sets[s.Name+" "+s.Type] = append(sets[s.Name+" "+s.Type], r.Content)
		}
	}
	if z.Kind != "Master" || z.SOAEditAPI != "DEFAULT" || sets["example.com. SOA"][0] != "ns1.example.net. hostmaster.example.com. 0 10800 3600 604800 3600" ||
		len(sets["example.com. NS"]) != 2 || sets["example.com. MX"][0] != "10 mail.example.com." || sets["example.com. TXT"][0] != `"v=spf1 mx \"-all\""` {
		t.Fatalf("zone: %+v", sets)
	}

	// The same again changes nothing, and does not restart PowerDNS.
	f.calls, pdns.calls = nil, nil
	applyNameServer(ctx, f.system, &spec)
	if len(pdns.calls) != 0 || f.ran("systemctl restart pdns") {
		t.Errorf("an unchanged zone was written again: %v %v", pdns.calls, f.calls)
	}

	// A changed address and a removed MX; DNSSEC on.
	spec.Zones[0].Records[2].Content = "198.51.100.8"
	spec.Zones[0].Records = slices.Delete(spec.Zones[0].Records, 3, 4)
	spec.Zones[0].DNSSEC = true
	res = applyNameServer(ctx, f.system, &spec)
	if !slices.Contains(pdns.calls, "PATCH /zones/example.com.") || !slices.Contains(pdns.calls, "PUT /zones/example.com.") ||
		!slices.Contains(pdns.calls, "PUT /zones/example.com./notify") {
		t.Errorf("calls: %v", pdns.calls)
	}
	// Two writes: the records, then the SOA again for the signing.
	if len(res.Zones[0].DS) != 1 || res.Zones[0].DS[0] != "1234 13 2 bbbb" || res.Zones[0].Serial != 3 {
		t.Errorf("result: %+v", res.Zones[0])
	}
	for _, s := range pdns.zone("example.com.").RRsets {
		if s.Type == "MX" || (s.Type == "A" && s.Records[0].Content != "198.51.100.8") {
			t.Errorf("rrset %+v", s)
		}
		// The SOA written back keeps the serial PowerDNS had.
		if s.Type == "SOA" && !strings.Contains(s.Records[0].Content, " 1 10800 ") && !strings.Contains(s.Records[0].Content, " 2 10800 ") {
			t.Errorf("SOA %s", s.Records[0].Content)
		}
	}

	// A zone no longer wanted goes.
	spec.Zones = nil
	applyNameServer(ctx, f.system, &spec)
	if pdns.zone("example.com.") != nil {
		t.Error("the zone was not removed")
	}

	// No longer a name server: stopped.
	f.calls = nil
	if res := applyNameServer(ctx, f.system, nil); res != nil || !f.ran("systemctl disable --now pdns") {
		t.Errorf("off: %+v %v", res, f.calls)
	}
}

func TestNameServerSecondary(t *testing.T) {
	f := nameServerSystem(t)
	pdns := newFakePowerDNS(t)
	spec := agentproto.NameServerSpec{PrimaryAddress: "192.0.2.1", Zones: []agentproto.HostedZoneSpec{{Name: "example.com"}}}
	res := applyNameServer(context.Background(), f.system, &spec)
	if res.Error != "" {
		t.Fatal(res.Error)
	}
	z := pdns.zone("example.com.")
	if z == nil || z.Kind != "Slave" || !slices.Equal(z.Masters, []string{"192.0.2.1"}) || !slices.Contains(pdns.calls, "PUT /zones/example.com./axfr-retrieve") {
		t.Fatalf("secondary zone: %+v %v", z, pdns.calls)
	}
	conf := f.read(t, pdnsConf)
	if !strings.Contains(conf, "secondary=yes") || !strings.Contains(conf, "allow-notify-from=192.0.2.1") || strings.Contains(conf, "primary=yes") {
		t.Errorf("pdns.conf:\n%s", conf)
	}
	// Signed on the primary: served as transferred, signatures and all.
	spec.Zones[0].DNSSEC = true
	applyNameServer(context.Background(), f.system, &spec)
	if !f.ran("pdnsutil set-presigned example.com") {
		t.Errorf("not marked presigned: %v", f.calls)
	}
	spec.Zones[0].DNSSEC = false
	// Moved primary: the zone follows.
	spec.PrimaryAddress = "192.0.2.2"
	applyNameServer(context.Background(), f.system, &spec)
	if z := pdns.zone("example.com."); !slices.Equal(z.Masters, []string{"192.0.2.2"}) {
		t.Errorf("masters %v", z.Masters)
	}
	// This server becomes the primary: the secondary zone is replaced by a primary one.
	p := primarySpec()
	applyNameServer(context.Background(), f.system, &p)
	if z := pdns.zone("example.com."); z.Kind != "Master" {
		t.Errorf("kind %s", z.Kind)
	}
}

func TestHostedRecordCheck(t *testing.T) {
	good := []agentproto.HostedRecord{
		{Name: "WWW.example.com.", Type: "a", Content: "198.51.100.1", TTL: 300},
		{Name: "example.com", Type: "AAAA", Content: "2001:DB8::1", TTL: 300},
		{Name: "*.example.com", Type: "CNAME", Content: "example.com.", TTL: 300},
		{Name: "example.com", Type: "MX", Content: "mail.example.com", Priority: 10, TTL: 3600},
		{Name: "_dmarc.example.com", Type: "TXT", Content: "v=DMARC1; p=none", TTL: 3600},
		{Name: "_sip._tcp.example.com", Type: "SRV", Content: "5 5060 sip.example.com.", Priority: 10, TTL: 3600},
		{Name: "example.com", Type: "CAA", Content: `0 issue "letsencrypt.org"`, TTL: 3600},
		{Name: "dev.example.com", Type: "NS", Content: "ns1.other.net", TTL: 3600},
	}
	for _, r := range good {
		if err := r.Check("example.com"); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	r := good[0]
	r.Check("example.com")
	if r.Name != "www.example.com" || r.Type != "A" {
		t.Errorf("normalized %+v", r)
	}
	bad := []agentproto.HostedRecord{
		{Name: "www.other.com", Type: "A", Content: "198.51.100.1", TTL: 300},
		{Name: "www.example.com", Type: "A", Content: "2001:db8::1", TTL: 300},
		{Name: "example.com", Type: "CNAME", Content: "other.com", TTL: 300},
		{Name: "example.com", Type: "NS", Content: "ns.other.com", TTL: 300},
		{Name: "a.*.example.com", Type: "A", Content: "198.51.100.1", TTL: 300},
		{Name: "www.example.com", Type: "A", Content: "198.51.100.1", TTL: 5},
		{Name: "www.example.com", Type: "TXT", Content: "line\nbreak", TTL: 300},
		{Name: "sip.example.com", Type: "SRV", Content: "5 5060 sip.example.com", TTL: 300},
		{Name: "example.com", Type: "CAA", Content: `0 issue letsencrypt.org`, TTL: 300},
		{Name: "www.example.com", Type: "HINFO", Content: "x", TTL: 300},
		{Name: "www.example.com", Type: "A", Content: "198.51.100.1", Priority: 3, TTL: 300},
		{Name: "www.example.com", Type: "MX", Content: "bad host!", TTL: 300},
	}
	for _, r := range bad {
		if r.Check("example.com") == nil {
			t.Errorf("accepted %+v", r)
		}
	}
	if agentproto.CheckRecords("example.com", []agentproto.HostedRecord{{Name: "a.example.com", Type: "CNAME"}, {Name: "a.example.com", Type: "TXT"}}) == nil {
		t.Error("a CNAME next to another record was accepted")
	}
	for _, z := range []string{"example.com", "xn--bcher-kva.example", "a-b.co.uk"} {
		if !agentproto.ValidZoneName(z) {
			t.Errorf("zone %s refused", z)
		}
	}
	for _, z := range []string{"com", "Example.com", "example..com", "-a.com", "1.2.3.4", "a_b.com"} {
		if agentproto.ValidZoneName(z) {
			t.Errorf("zone %s accepted", z)
		}
	}
	if got := quoteTXT(strings.Repeat("x", 300)); !strings.HasPrefix(got, `"`+strings.Repeat("x", 255)+`" "`) {
		t.Errorf("long TXT: %s", got)
	}
}
