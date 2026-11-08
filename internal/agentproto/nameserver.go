package agentproto

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// ---- Built-in DNS ----
//
// Name servers are ekPanel servers running PowerDNS. The primary holds every hosted zone,
// signs those with DNSSEC on, and sends them to the secondaries by zone transfer (NOTIFY,
// then AXFR). The panel sends the primary each zone's records; the secondaries only learn
// which zones to fetch, and from where.

// NameServerSpec is part of the desired state of a name server; nil means the server is
// not one (PowerDNS, if ekPanel set it up, is stopped).
type NameServerSpec struct {
	Primary bool `json:"primary"`
	// Zones: on the primary with their records, on secondaries the names only.
	Zones []HostedZoneSpec `json:"zones"`
	// PrimaryAddress is where secondaries transfer zones from and accept NOTIFY from.
	PrimaryAddress string `json:"primary_address,omitempty"`
	// Secondaries are the addresses the primary allows transfers to and notifies.
	Secondaries []string `json:"secondaries,omitempty"`
}

type HostedZoneSpec struct {
	Name   string `json:"name"` // "example.com"
	DNSSEC bool   `json:"dnssec,omitempty"`
	// SOA: the primary name server's host name and the zone's contact ("hostmaster.example.com").
	PrimaryNS  string         `json:"primary_ns,omitempty"`
	Hostmaster string         `json:"hostmaster,omitempty"`
	Records    []HostedRecord `json:"records,omitempty"`
}

// HostedRecord is one record. Names are fully qualified without the final dot; contents
// are as people write them: host names without the final dot, TXT text without quotes.
type HostedRecord struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Content  string `json:"content"`
	Priority int    `json:"priority,omitempty"` // MX and SRV
	TTL      int    `json:"ttl"`
}

type NameServerResult struct {
	Active  bool               `json:"active"`
	Version string             `json:"version,omitempty"`
	Error   string             `json:"error,omitempty"`
	Zones   []HostedZoneResult `json:"zones"`
}

type HostedZoneResult struct {
	Name   string `json:"name"`
	Serial int64  `json:"serial"`
	// DS are the records to give the registrar for DNSSEC (primary, signed zones).
	DS    []string `json:"ds,omitempty"`
	Error string   `json:"error,omitempty"`
}

// RecordTypes are the types the zone editor offers.
var RecordTypes = []string{"A", "AAAA", "CNAME", "MX", "TXT", "SRV", "CAA", "NS"}

const (
	MinTTL         = 60
	MaxTTL         = 604800
	MaxTXT         = 4000
	MaxZoneRecords = 2000
)

var (
	dnsLabelRE  = regexp.MustCompile(`^(\*|_?[a-z0-9]([a-z0-9_-]{0,61}[a-z0-9])?)$`)
	caaRE       = regexp.MustCompile(`^(0|128) (issue|issuewild|iodef) "[\x21\x23-\x7e]*"$`)
	srvRE       = regexp.MustCompile(`^(\d{1,5}) (\d{1,5}) (\S+)$`)
	zoneLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// ValidZoneName reports whether s is a domain that may be a zone: lower case ASCII
// (punycode for international names), at least two labels.
func ValidZoneName(s string) bool {
	labels := strings.Split(s, ".")
	if len(s) > 253 || len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if !zoneLabelRE.MatchString(l) {
			return false
		}
	}
	return strings.Trim(labels[len(labels)-1], "0123456789") != "" // not an IPv4 address
}

// ValidDNSName reports whether name is a host name in zone (or the zone itself). Labels
// may start with "_" (SRV, DKIM) and the first may be "*".
func ValidDNSName(name, zone string) bool {
	if name != zone && !strings.HasSuffix(name, "."+zone) || len(name) > 253 {
		return false
	}
	for i, l := range strings.Split(name, ".") {
		if !dnsLabelRE.MatchString(l) || (l == "*" && i > 0) {
			return false
		}
	}
	return true
}

// validTarget reports whether s is a host name a record may point at.
func validTarget(s string) bool {
	if s == "" || len(s) > 253 || strings.HasSuffix(s, ".") {
		return false
	}
	for _, l := range strings.Split(s, ".") {
		if l == "*" || !dnsLabelRE.MatchString(l) {
			return false
		}
	}
	return true
}

// Check validates one record of zone, normalizing what may be normalized.
func (r *HostedRecord) Check(zone string) error {
	r.Name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(r.Name), "."))
	r.Type = strings.ToUpper(strings.TrimSpace(r.Type))
	if r.Type != "TXT" && r.Type != "CAA" {
		r.Content = strings.TrimSpace(r.Content)
	}
	if !ValidDNSName(r.Name, zone) {
		return fmt.Errorf("%s is not a name in %s", r.Name, zone)
	}
	if r.TTL < MinTTL || r.TTL > MaxTTL {
		return fmt.Errorf("the TTL must be from %d to %d seconds", MinTTL, MaxTTL)
	}
	if r.Type != "MX" && r.Type != "SRV" && r.Priority != 0 {
		return errors.New("only MX and SRV records have a priority")
	}
	if r.Priority < 0 || r.Priority > 65535 {
		return errors.New("the priority must be from 0 to 65535")
	}
	switch r.Type {
	case "A", "AAAA":
		a, err := netip.ParseAddr(r.Content)
		if err != nil || a.Is4() != (r.Type == "A") || a.Zone() != "" || a.Is4In6() {
			return fmt.Errorf("%s needs an IPv%s address", r.Type, map[string]string{"A": "4", "AAAA": "6"}[r.Type])
		}
		r.Content = a.String()
	case "CNAME", "NS", "MX":
		r.Content = strings.ToLower(strings.TrimSuffix(r.Content, "."))
		if !validTarget(r.Content) {
			return fmt.Errorf("%s needs a host name", r.Type)
		}
		if r.Type == "CNAME" && r.Name == zone {
			return errors.New("the domain itself cannot be a CNAME; use A and AAAA records")
		}
		if r.Type == "NS" && r.Name == zone {
			return errors.New("the domain's own name servers are set by ekPanel")
		}
	case "TXT":
		if r.Content == "" || len(r.Content) > MaxTXT || strings.IndexFunc(r.Content, func(c rune) bool { return c > unicode.MaxASCII || unicode.IsControl(c) }) >= 0 {
			return fmt.Errorf("TXT needs printable ASCII text of up to %d characters", MaxTXT)
		}
	case "SRV":
		m := srvRE.FindStringSubmatch(r.Content)
		if m == nil || !strings.HasPrefix(r.Name, "_") {
			return errors.New(`SRV needs a name such as _sip._tcp.example.com and "weight port target"`)
		}
		w, _ := strconv.Atoi(m[1])
		p, _ := strconv.Atoi(m[2])
		target := strings.ToLower(strings.TrimSuffix(m[3], "."))
		if w > 65535 || p > 65535 || (target != "" && !validTarget(target)) {
			return errors.New("SRV weight and port go up to 65535, and the target is a host name")
		}
		r.Content = fmt.Sprintf("%d %d %s", w, p, target)
	case "CAA":
		if !caaRE.MatchString(r.Content) {
			return errors.New(`CAA needs flags, a tag and a quoted value, such as 0 issue "letsencrypt.org"`)
		}
	default:
		return fmt.Errorf("unknown record type %s", r.Type)
	}
	return nil
}

// CheckRecords validates a zone's records together: one CNAME alone at a name, and not
// too many records.
func CheckRecords(zone string, records []HostedRecord) error {
	if len(records) > MaxZoneRecords {
		return fmt.Errorf("a zone may have up to %d records", MaxZoneRecords)
	}
	types := map[string][]string{}
	for _, r := range records {
		types[r.Name] = append(types[r.Name], r.Type)
	}
	for name, ts := range types {
		if slices.Contains(ts, "CNAME") && len(ts) > 1 {
			return fmt.Errorf("%s has a CNAME record, which cannot share its name with other records", name)
		}
	}
	return nil
}

// Validate checks a name server's spec as the agent receives it.
func (s NameServerSpec) Validate() error {
	if s.Primary == (s.PrimaryAddress != "") {
		return errors.New("a primary has no primary address; a secondary needs one")
	}
	if s.PrimaryAddress != "" {
		if _, err := netip.ParseAddr(s.PrimaryAddress); err != nil {
			return errors.New("invalid primary address")
		}
	}
	for _, a := range s.Secondaries {
		if _, err := netip.ParseAddr(a); err != nil {
			return errors.New("invalid secondary address " + a)
		}
	}
	for _, z := range s.Zones {
		if !ValidZoneName(z.Name) {
			return errors.New("invalid zone " + z.Name)
		}
		if !s.Primary {
			continue
		}
		if !validTarget(z.PrimaryNS) || !validTarget(z.Hostmaster) {
			return errors.New("zone " + z.Name + " needs its SOA names")
		}
		for i := range z.Records {
			if err := z.Records[i].Check(z.Name); err != nil && !(z.Records[i].Type == "NS" && z.Records[i].Name == z.Name) {
				return fmt.Errorf("zone %s: %w", z.Name, err)
			}
		}
		if err := CheckRecords(z.Name, z.Records); err != nil {
			return fmt.Errorf("zone %s: %w", z.Name, err)
		}
	}
	return nil
}

// DNSPorts are opened in the firewall on name servers.
var DNSPorts = []string{"53/tcp", "53/udp"}
