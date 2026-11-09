package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"ekpanel/internal/agentproto"
)

// Name servers run PowerDNS from the distribution with its SQLite backend, driven through
// its HTTP API on loopback. PowerDNS listens on the server's own addresses rather than on
// all of them: systemd-resolved holds port 53 on 127.0.0.53.

const (
	pdnsConf    = "/etc/powerdns/pdns.conf"
	pdnsConfDir = "/etc/powerdns/pdns.d"
	pdnsKeyFile = "/etc/powerdns/ekpanel-api.key"
	pdnsDB      = "/var/lib/powerdns/pdns.sqlite3"
	pdnsSchema  = "/usr/share/pdns-backend-sqlite3/schema/schema.sqlite3.sql"
	pdnsManaged = "/var/lib/ekpanel/pdns-managed" // ekPanel set PowerDNS up
	pdnsAPIAddr = "127.0.0.1:18920"
	// soaTimers are refresh, retry, expire and the negative-answer TTL.
	soaTimers = "10800 3600 604800 3600"
)

// pdnsAPI is PowerDNS's API for its own server; tests point it elsewhere.
var pdnsAPI = "http://" + pdnsAPIAddr + "/api/v1/servers/localhost"

// applyNameServer sets PowerDNS up as the spec says and brings its zones in line. Without a
// spec, PowerDNS that ekPanel set up is stopped (its zones stay, for turning it on again).
func applyNameServer(ctx context.Context, sys *system, spec *agentproto.NameServerSpec) *agentproto.NameServerResult {
	if spec == nil {
		if _, err := os.Stat(sys.path(pdnsManaged)); err == nil {
			sys.run(ctx, "systemctl", "disable", "--now", "pdns")
			os.Remove(sys.path(pdnsManaged))
		}
		return nil
	}
	res := &agentproto.NameServerResult{Zones: []agentproto.HostedZoneResult{}}
	var api *pdnsClient
	err := func() error {
		if err := spec.Validate(); err != nil {
			return err
		}
		key, err := ensurePowerDNS(ctx, sys, *spec)
		if err != nil {
			return err
		}
		api = &pdnsClient{key: key, sys: sys}
		if err := api.wait(ctx, sys); err != nil {
			return err
		}
		res.Zones, err = api.syncZones(ctx, *spec)
		return err
	}()
	if err != nil {
		res.Error = err.Error()
	}
	res.Active = unitActive(ctx, sys, "pdns")
	if api != nil && res.Active {
		var info struct {
			Version string `json:"version"`
		}
		if api.call(ctx, http.MethodGet, "", nil, &info) == nil {
			res.Version = info.Version
		}
	}
	return res
}

// ensurePowerDNS installs and configures PowerDNS, and returns its API key.
func ensurePowerDNS(ctx context.Context, sys *system, spec agentproto.NameServerSpec) (string, error) {
	// The package starts PowerDNS with no backend, which fails until it is set up below.
	if err := ensurePackage(ctx, sys, "pdns-server", "pdns-backend-sqlite3", "sqlite3"); err != nil {
		return "", fmt.Errorf("install PowerDNS: %w", err)
	}
	if err := os.MkdirAll(sys.path("/var/lib/ekpanel"), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(sys.path(pdnsManaged), nil, 0o644); err != nil {
		return "", err
	}
	if _, err := os.Stat(sys.path(pdnsDB)); err != nil {
		schema, err := os.ReadFile(sys.path(pdnsSchema))
		if err != nil {
			return "", fmt.Errorf("PowerDNS's database schema is missing: %w", err)
		}
		if err := os.MkdirAll(sys.path(filepath.Dir(pdnsDB)), 0o750); err != nil {
			return "", err
		}
		sys.run(ctx, "chown", "pdns:pdns", filepath.Dir(pdnsDB))
		if _, err := sys.runInput(ctx, string(schema), "runuser", "-u", "pdns", "--", "sqlite3", pdnsDB); err != nil {
			os.Remove(sys.path(pdnsDB))
			return "", fmt.Errorf("create PowerDNS's database: %w", err)
		}
	}
	if err := os.MkdirAll(sys.path(filepath.Dir(pdnsConf)), 0o755); err != nil {
		return "", err
	}
	key, err := os.ReadFile(sys.path(pdnsKeyFile))
	if err != nil || len(bytes.TrimSpace(key)) < 32 {
		key = []byte(randomHex(24))
		if err := os.WriteFile(sys.path(pdnsKeyFile), key, 0o600); err != nil {
			return "", err
		}
	}
	apiKey := string(bytes.TrimSpace(key))
	changed, err := writeSecretIfChanged(sys.path(pdnsConf), pdnsConfig(spec, apiKey, listenAddresses()))
	if err != nil {
		return "", err
	}
	// Root's, readable by PowerDNS's group: it holds the API key.
	sys.run(ctx, "chgrp", "pdns", pdnsConf)
	os.Chmod(sys.path(pdnsConf), 0o640)
	// The package's own example files (the BIND backend) would start a second backend.
	entries, _ := os.ReadDir(sys.path(pdnsConfDir))
	for _, e := range entries {
		if os.Remove(sys.path(filepath.Join(pdnsConfDir, e.Name()))) == nil {
			changed = true
		}
	}
	sys.run(ctx, "systemctl", "enable", "pdns")
	if changed || !unitActive(ctx, sys, "pdns") {
		sys.run(ctx, "systemctl", "reset-failed", "pdns")
		if _, err := sys.run(ctx, "systemctl", "restart", "pdns"); err != nil {
			return "", fmt.Errorf("start PowerDNS: %w", err)
		}
	}
	return apiKey, nil
}

func pdnsConfig(spec agentproto.NameServerSpec, apiKey string, listen []string) []byte {
	var b strings.Builder
	line := func(k, v string) { fmt.Fprintf(&b, "%s=%s\n", k, v) }
	b.WriteString("# Managed by ekPanel. Changes here are overwritten.\n")
	line("launch", "gsqlite3")
	line("gsqlite3-database", pdnsDB)
	line("gsqlite3-dnssec", "yes")
	line("local-address", strings.Join(listen, ", "))
	line("local-address-nonexist-fail", "no")
	line("api", "yes")
	line("api-key", apiKey)
	line("webserver", "yes")
	line("webserver-address", "127.0.0.1")
	line("webserver-port", strings.TrimPrefix(pdnsAPIAddr, "127.0.0.1:"))
	line("webserver-allow-from", "127.0.0.1,::1")
	line("version-string", "anonymous")
	line("security-poll-suffix", "")
	line("default-ttl", "3600")
	transfer := []string{"127.0.0.1", "::1"}
	if spec.Primary {
		line("primary", "yes")
		line("allow-axfr-ips", strings.Join(append(transfer, spec.Secondaries...), ","))
		if len(spec.Secondaries) > 0 {
			line("also-notify", strings.Join(spec.Secondaries, ","))
		}
	} else {
		line("secondary", "yes")
		line("autosecondary", "no")
		line("allow-axfr-ips", strings.Join(transfer, ","))
		line("allow-notify-from", spec.PrimaryAddress)
	}
	return []byte(b.String())
}

// listenAddresses are loopback and the addresses of the server's own interfaces (not
// Docker's or other virtual bridges'), without link-local IPv6 addresses.
func listenAddresses() []string {
	out := []string{"127.0.0.1", "::1"}
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 ||
			strings.HasPrefix(ifc.Name, "docker") || strings.HasPrefix(ifc.Name, "br-") || strings.HasPrefix(ifc.Name, "veth") {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && !n.IP.IsLinkLocalUnicast() && !n.IP.IsLoopback() && !slices.Contains(out, n.IP.String()) {
				out = append(out, n.IP.String())
			}
		}
	}
	sort.Strings(out[2:])
	return out
}

// ---- PowerDNS's API ----

type pdnsClient struct {
	key string
	sys *system
}

type pdnsError struct {
	Status  int
	Message string
}

func (e *pdnsError) Error() string { return fmt.Sprintf("PowerDNS: %s (%d)", e.Message, e.Status) }

func (c *pdnsClient) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, pdnsAPI+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return &pdnsError{Status: resp.StatusCode, Message: e.Error}
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// wait waits for the API after a (re)start.
func (c *pdnsClient) wait(ctx context.Context, sys *system) error {
	var err error
	for range 30 {
		if err = c.call(ctx, http.MethodGet, "", nil, nil); err == nil {
			return nil
		}
		if sys.socketWait == 0 || ctx.Err() != nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("PowerDNS does not answer: %w", err)
}

type pdnsZone struct {
	ID          string      `json:"id,omitempty"`
	Name        string      `json:"name"`
	Kind        string      `json:"kind"`
	Serial      int64       `json:"serial,omitempty"`
	DNSSEC      bool        `json:"dnssec"`
	Masters     []string    `json:"masters,omitempty"`
	SOAEditAPI  string      `json:"soa_edit_api,omitempty"`
	Nameservers []string    `json:"nameservers"`
	RRsets      []pdnsRRset `json:"rrsets,omitempty"`
}

type pdnsRRset struct {
	Name       string       `json:"name"`
	Type       string       `json:"type"`
	TTL        int          `json:"ttl,omitempty"`
	ChangeType string       `json:"changetype,omitempty"`
	Records    []pdnsRecord `json:"records"`
}

type pdnsRecord struct {
	Content  string `json:"content"`
	Disabled bool   `json:"disabled"`
}

func fqdn(name string) string { return name + "." }

// zonePath is a zone's API path; zone names here are plain host names.
func zonePath(name string) string { return "/zones/" + fqdn(name) }

// syncZones makes PowerDNS hold exactly the spec's zones.
func (c *pdnsClient) syncZones(ctx context.Context, spec agentproto.NameServerSpec) ([]agentproto.HostedZoneResult, error) {
	var have []pdnsZone
	if err := c.call(ctx, http.MethodGet, "/zones", nil, &have); err != nil {
		return nil, err
	}
	byName := map[string]pdnsZone{}
	for _, z := range have {
		byName[strings.TrimSuffix(z.Name, ".")] = z
	}
	wanted := map[string]bool{}
	results := []agentproto.HostedZoneResult{}
	var failed []string
	for _, z := range spec.Zones {
		wanted[z.Name] = true
		var r agentproto.HostedZoneResult
		var err error
		if spec.Primary {
			r, err = c.syncPrimaryZone(ctx, z, byName[z.Name])
		} else {
			r, err = c.syncSecondaryZone(ctx, z, byName[z.Name], spec.PrimaryAddress)
		}
		r.Name = z.Name
		if err != nil {
			r.Error = err.Error()
			failed = append(failed, z.Name)
		}
		results = append(results, r)
	}
	for name := range byName {
		if !wanted[name] {
			if err := c.call(ctx, http.MethodDelete, zonePath(name), nil, nil); err != nil {
				failed = append(failed, name)
			}
		}
	}
	if len(failed) > 0 {
		return results, fmt.Errorf("%d zones could not be updated: %s", len(failed), strings.Join(failed, ", "))
	}
	return results, nil
}

func (c *pdnsClient) syncPrimaryZone(ctx context.Context, z agentproto.HostedZoneSpec, have pdnsZone) (agentproto.HostedZoneResult, error) {
	res := agentproto.HostedZoneResult{}
	want := wantRRsets(z)
	changed := false
	if have.Name != "" && have.Kind != "Master" && have.Kind != "Native" {
		// It was a secondary zone here: this server became the primary.
		if err := c.call(ctx, http.MethodDelete, zonePath(z.Name), nil, nil); err != nil {
			return res, err
		}
		have = pdnsZone{}
	}
	if have.Name == "" {
		zone := pdnsZone{Name: fqdn(z.Name), Kind: "Master", SOAEditAPI: "DEFAULT", DNSSEC: z.DNSSEC, Nameservers: []string{}}
		for _, set := range want {
			zone.RRsets = append(zone.RRsets, set)
		}
		sort.Slice(zone.RRsets, func(i, j int) bool { return rrsetKey(zone.RRsets[i]) < rrsetKey(zone.RRsets[j]) })
		if err := c.call(ctx, http.MethodPost, "/zones", zone, nil); err != nil {
			return res, err
		}
		changed = true
	} else {
		var cur pdnsZone
		if err := c.call(ctx, http.MethodGet, zonePath(z.Name), nil, &cur); err != nil {
			return res, err
		}
		// A SOA written back carries the current serial, which PowerDNS then raises; with 0
		// it would start again from today's date, below the serial secondaries hold.
		soaKey := rrsetKey(pdnsRRset{Name: fqdn(z.Name), Type: "SOA"})
		soa := want[soaKey]
		soa.Records = []pdnsRecord{{Content: fqdn(z.PrimaryNS) + " " + fqdn(z.Hostmaster) + " " + strconv.FormatInt(cur.Serial, 10) + " " + soaTimers}}
		want[soaKey] = soa
		if changes := rrsetChanges(want, cur.RRsets); len(changes) > 0 {
			if err := c.call(ctx, http.MethodPatch, zonePath(z.Name), map[string]any{"rrsets": changes}, nil); err != nil {
				return res, err
			}
			changed = true
		}
		if cur.DNSSEC != z.DNSSEC {
			if err := c.call(ctx, http.MethodPut, zonePath(z.Name), map[string]any{"dnssec": z.DNSSEC}, nil); err != nil {
				return res, err
			}
			// Signing or unsigning keeps the serial, so secondaries would keep their copy;
			// writing the SOA again moves it on.
			soa.ChangeType = "REPLACE"
			if err := c.call(ctx, http.MethodPatch, zonePath(z.Name), map[string]any{"rrsets": []pdnsRRset{soa}}, nil); err != nil {
				return res, err
			}
			changed = true
		}
	}
	if changed {
		// PowerDNS notices a new serial by itself only once a minute; the secondaries are
		// told now. They check again on their own if this NOTIFY is lost.
		c.call(ctx, http.MethodPut, zonePath(z.Name)+"/notify", nil, nil)
	}
	var now pdnsZone
	if err := c.call(ctx, http.MethodGet, zonePath(z.Name)+"?rrsets=false", nil, &now); err != nil {
		return res, err
	}
	res.Serial = now.Serial
	if z.DNSSEC {
		var keys []struct {
			Active bool     `json:"active"`
			DS     []string `json:"ds"`
		}
		if err := c.call(ctx, http.MethodGet, zonePath(z.Name)+"/cryptokeys", nil, &keys); err != nil {
			return res, err
		}
		for _, k := range keys {
			for _, ds := range k.DS {
				// "18326 13 2 76…": key tag, algorithm, digest type 2 (SHA-256), digest.
				if f := strings.Fields(ds); k.Active && len(f) == 4 && f[2] == "2" {
					res.DS = append(res.DS, ds)
				}
			}
		}
	}
	return res, nil
}

func (c *pdnsClient) syncSecondaryZone(ctx context.Context, z agentproto.HostedZoneSpec, have pdnsZone, primary string) (agentproto.HostedZoneResult, error) {
	res := agentproto.HostedZoneResult{Serial: have.Serial}
	if have.Name != "" && have.Kind != "Slave" {
		if err := c.call(ctx, http.MethodDelete, zonePath(z.Name), nil, nil); err != nil {
			return res, err
		}
		have = pdnsZone{}
	}
	switch {
	case have.Name == "":
		zone := pdnsZone{Name: fqdn(z.Name), Kind: "Slave", Masters: []string{primary}, Nameservers: []string{}}
		if err := c.call(ctx, http.MethodPost, "/zones", zone, nil); err != nil {
			return res, err
		}
	case !slices.Equal(have.Masters, []string{primary}):
		if err := c.call(ctx, http.MethodPut, zonePath(z.Name), map[string]any{"kind": "Slave", "masters": []string{primary}}, nil); err != nil {
			return res, err
		}
	}
	retrieve := res.Serial == 0 // not transferred yet: ask for it now rather than at the next check
	// A signed zone is served as transferred, signatures and all ("presigned"). PowerDNS marks
	// that by itself only on a zone's first transfer, not when the primary signs it later.
	var meta []pdnsMetadata
	if err := c.call(ctx, http.MethodGet, zonePath(z.Name)+"/metadata", nil, &meta); err != nil {
		return res, err
	}
	presigned := slices.ContainsFunc(meta, func(m pdnsMetadata) bool { return m.Kind == "PRESIGNED" && slices.Contains(m.Metadata, "1") })
	if presigned != z.DNSSEC {
		cmd := "unset-presigned"
		if z.DNSSEC {
			cmd = "set-presigned"
		}
		// The API does not change this setting; PowerDNS reads it again within a minute.
		if _, err := c.sys.run(ctx, "pdnsutil", cmd, z.Name); err != nil {
			return res, fmt.Errorf("mark the zone as signed: %w", err)
		}
		retrieve = true
	}
	if retrieve {
		c.call(ctx, http.MethodPut, zonePath(z.Name)+"/axfr-retrieve", nil, nil)
	}
	return res, nil
}

type pdnsMetadata struct {
	Kind     string   `json:"kind"`
	Metadata []string `json:"metadata"`
}

func rrsetKey(s pdnsRRset) string { return s.Name + " " + s.Type }

// wantRRsets groups a zone's records into PowerDNS's record sets, with its SOA.
func wantRRsets(z agentproto.HostedZoneSpec) map[string]pdnsRRset {
	sets := map[string]pdnsRRset{}
	soa := pdnsRRset{Name: fqdn(z.Name), Type: "SOA", TTL: 3600,
		Records: []pdnsRecord{{Content: fqdn(z.PrimaryNS) + " " + fqdn(z.Hostmaster) + " 0 " + soaTimers}}}
	sets[rrsetKey(soa)] = soa
	for _, r := range z.Records {
		set := pdnsRRset{Name: fqdn(r.Name), Type: r.Type, TTL: r.TTL}
		if cur, ok := sets[rrsetKey(set)]; ok {
			set = cur
			set.TTL = min(set.TTL, r.TTL)
		}
		set.Records = append(set.Records, pdnsRecord{Content: pdnsContent(r)})
		sets[rrsetKey(set)] = set
	}
	return sets
}

// pdnsContent writes a record's content the way zone files (and PowerDNS) do.
func pdnsContent(r agentproto.HostedRecord) string {
	switch r.Type {
	case "CNAME", "NS":
		return fqdn(r.Content)
	case "MX":
		return strconv.Itoa(r.Priority) + " " + fqdn(r.Content)
	case "SRV":
		f := strings.Fields(r.Content) // weight port target
		return fmt.Sprintf("%d %s %s %s", r.Priority, f[0], f[1], fqdn(f[2]))
	case "TXT":
		return quoteTXT(r.Content)
	}
	return r.Content
}

// quoteTXT quotes text as DNS strings of up to 255 bytes each.
func quoteTXT(s string) string {
	var parts []string
	for len(s) > 0 {
		n := min(255, len(s))
		chunk := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s[:n])
		parts = append(parts, `"`+chunk+`"`)
		s = s[n:]
	}
	return strings.Join(parts, " ")
}

// rrsetChanges lists the replacements and deletions that turn have into want. The SOA is
// compared without its serial, which PowerDNS keeps.
func rrsetChanges(want map[string]pdnsRRset, have []pdnsRRset) []pdnsRRset {
	var changes []pdnsRRset
	current := map[string]pdnsRRset{}
	for _, s := range have {
		current[rrsetKey(s)] = s
	}
	for key, w := range want {
		h, ok := current[key]
		if ok && h.TTL == w.TTL && sameRecords(w, h) {
			continue
		}
		w.ChangeType = "REPLACE"
		changes = append(changes, w)
	}
	for key, h := range current {
		if _, ok := want[key]; !ok && h.Type != "SOA" {
			changes = append(changes, pdnsRRset{Name: h.Name, Type: h.Type, ChangeType: "DELETE", Records: []pdnsRecord{}})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return rrsetKey(changes[i]) < rrsetKey(changes[j]) })
	return changes
}

func sameRecords(a, b pdnsRRset) bool {
	norm := func(s pdnsRRset) []string {
		var out []string
		for _, r := range s.Records {
			c := r.Content
			if s.Type == "SOA" {
				if f := strings.Fields(c); len(f) == 7 {
					f[2] = "0"
					c = strings.Join(f, " ")
				}
			}
			if s.Type != "TXT" && s.Type != "CAA" {
				c = strings.ToLower(c)
			}
			out = append(out, c)
		}
		sort.Strings(out)
		return out
	}
	return slices.Equal(norm(a), norm(b))
}
