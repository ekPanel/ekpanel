package tasks

import (
	"fmt"
	"slices"
	"strings"

	"ekpanel/internal/agentproto"
)

// CPanelZoneFilter says what this panel makes itself in an imported zone.
type CPanelZoneFilter struct {
	// Managed are host names of websites and apps here: their A, AAAA and CNAME records are
	// made by ekPanel, pointing at this server.
	Managed map[string]bool
	// MailHere: the zone's email moves here too, so ekPanel makes its MX, SPF, DKIM, DMARC
	// and TLS-RPT records.
	MailHere bool
	// OldIP is the cPanel server's address.
	OldIP string
}

// cpanelServiceLabels are host names cPanel adds for its own services.
var cpanelServiceLabels = []string{"cpanel", "whm", "webmail", "webdisk", "cpcalendars", "cpcontacts", "autodiscover", "autoconfig"}

// cpanelServicePrefixes are SRV and TXT names cPanel adds for its services and checks.
var cpanelServicePrefixes = []string{"_cpanel-dcv-test-record.", "_autodiscover._tcp.", "_caldav._tcp.", "_caldavs._tcp.",
	"_carddav._tcp.", "_carddavs._tcp.", "_imap._tcp.", "_imaps._tcp.", "_pop3._tcp.", "_pop3s._tcp.", "_submission._tcp."}

// FilterCPanelZone returns the records of a cPanel zone to keep in a hosted zone here, and
// notes for the report about the rest.
func FilterCPanelZone(z agentproto.CPanelZone, f CPanelZoneFilter) ([]agentproto.HostedRecord, []ImportNote) {
	var keep []agentproto.HostedRecord
	var notes []ImportNote
	var oldIP []string
	zone := z.Name
	for _, rec := range z.Records {
		name, typ := rec.Name, rec.Type
		rel := strings.TrimSuffix(strings.TrimSuffix(name, zone), ".")
		first, _, _ := strings.Cut(rel, ".")
		switch {
		case typ == "NS" && name == zone:
			continue // ekPanel's own name servers
		case slices.Contains(cpanelServiceLabels, first) && rel != "" ||
			slices.ContainsFunc(cpanelServicePrefixes, func(p string) bool { return strings.HasPrefix(name+".", p) }):
			continue // cPanel's services, which are not here
		case f.Managed[name] && (typ == "A" || typ == "AAAA" || typ == "CNAME"):
			continue // made by ekPanel for the website
		case f.MailHere && isMailRecord(rec, zone):
			continue // made by ekPanel for the email
		}
		if !f.MailHere && typ == "MX" && name == zone && (rec.Content == zone || f.Managed[rec.Content]) {
			notes = append(notes, ImportNote{Code: "zone_mx_here", Name: zone, Detail: rec.Content})
		}
		if (typ == "A" || typ == "AAAA") && f.OldIP != "" && rec.Content == f.OldIP && !slices.Contains(oldIP, name) {
			oldIP = append(oldIP, name)
		}
		if err := rec.Check(zone); err != nil {
			notes = append(notes, ImportNote{Code: "zone_record", Name: fmt.Sprintf("%s %s %s", name, typ, rec.Content), Detail: zone})
			continue
		}
		if slices.ContainsFunc(keep, func(k agentproto.HostedRecord) bool {
			return k.Name == rec.Name && k.Type == rec.Type && k.Content == rec.Content && k.Priority == rec.Priority
		}) {
			continue
		}
		keep = append(keep, rec)
	}
	if len(oldIP) > 0 {
		notes = append(notes, ImportNote{Code: "zone_old_ip", Name: strings.Join(oldIP, ", "), Detail: f.OldIP})
	}
	if z.Unread > 0 {
		notes = append(notes, ImportNote{Code: "zone_unread", Name: zone, Count: z.Unread})
	}
	return keep, notes
}

// isMailRecord: a record ekPanel makes for a mail domain (agentproto.ParseMailZone keeps
// the same ones).
func isMailRecord(r agentproto.HostedRecord, zone string) bool {
	switch {
	case r.Type == "MX":
		return r.Name == zone
	case r.Type != "TXT":
		return false
	case r.Name == zone:
		return strings.HasPrefix(strings.ToLower(r.Content), "v=spf1")
	}
	return strings.HasSuffix(r.Name, "._domainkey."+zone) || r.Name == "_dmarc."+zone || r.Name == "_smtp._tls."+zone
}
