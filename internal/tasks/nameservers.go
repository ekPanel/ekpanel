package tasks

import (
	"context"
	"errors"
	"slices"
	"strings"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/dnssync"
	"ekpanel/internal/store"
)

// Hosted zones are served by ekPanel's name servers. The primary gets each zone whole:
// the records people keep, the name servers' own NS (and glue) records, and the records
// websites, apps and email need, unless a kept record is in their way.

const (
	hostTTL = 300  // A and AAAA records of websites, apps and the mail host: quick to move
	zoneTTL = 3600 // everything else ekPanel makes
)

// QueueNameServers re-applies every name server, after zones or records changed.
func (r *Runner) QueueNameServers(ctx context.Context, createdBy *string) {
	ns, err := r.store.ListNameServers(ctx)
	if err != nil {
		r.log.Error("list name servers", "err", err)
		return
	}
	for _, n := range ns {
		if _, err := r.QueueReconcile(ctx, n.ServerID, createdBy); err != nil {
			r.log.Error("queue reconcile", "server", n.ServerName, "err", err)
		}
	}
}

// TransferAddress is where other name servers reach n: set by hand, or its public address.
func TransferAddress(n store.NameServer) string { return transferAddress(n) }

func transferAddress(n store.NameServer) string {
	if n.TransferAddress != nil {
		return *n.TransferAddress
	}
	if a := publicAddress(n.Target(), "A"); a != "" {
		return a
	}
	return publicAddress(n.Target(), "AAAA")
}

// HostedZoneBuild is a hosted zone as the primary serves it.
type HostedZoneBuild struct {
	Zone store.HostedZone
	Spec agentproto.HostedZoneSpec
	// Made are the records ekPanel adds by itself; Issues what it could not add.
	Made   []agentproto.HostedRecord
	Issues []dnssync.Issue
}

// ErrNoNameServers: hosted zones need name servers, with one primary.
var ErrNoNameServers = errors.New("set up ekPanel's name servers first")

// HostedZones builds every hosted zone (or the one with zoneID).
func (r *Runner) HostedZones(ctx context.Context, zoneID string) ([]HostedZoneBuild, error) {
	ns, err := r.store.ListNameServers(ctx)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(ns, func(n store.NameServer) bool { return n.Primary })
	if i < 0 {
		return nil, ErrNoNameServers
	}
	primary := ns[i]
	hosted, err := r.store.ListHostedZones(ctx, true, "")
	if err != nil {
		return nil, err
	}
	cf, err := r.store.ListZones(ctx, true, "")
	if err != nil {
		return nil, err
	}
	refs := append(hostedRefs(hosted), cloudflareRefs(cf)...)
	want, noAddress, err := r.wantedRecords(ctx, refs)
	if err != nil {
		return nil, err
	}
	kept, err := r.store.AllHostedRecords(ctx)
	if err != nil {
		return nil, err
	}
	var out []HostedZoneBuild
	for _, z := range hosted {
		if zoneID != "" && z.ID != zoneID {
			continue
		}
		b := HostedZoneBuild{Zone: z, Issues: append([]dnssync.Issue{}, noAddress[z.ID]...)}
		b.Spec = agentproto.HostedZoneSpec{Name: z.Name, DNSSEC: z.DNSSEC, PrimaryNS: primary.Hostname, Hostmaster: "hostmaster." + z.Name}
		for _, n := range ns {
			b.Made = append(b.Made, agentproto.HostedRecord{Name: z.Name, Type: "NS", Content: n.Hostname, TTL: zoneTTL})
			// Glue: a name server named inside the zone needs its addresses there too.
			if ref := zoneFor(n.Hostname, nil, refs); ref != nil && ref.ID == z.ID {
				for _, typ := range []string{"A", "AAAA"} {
					if a := publicAddress(n.Target(), typ); a != "" {
						b.Made = append(b.Made, agentproto.HostedRecord{Name: n.Hostname, Type: typ, Content: a, TTL: zoneTTL})
					}
				}
			}
		}
		var own []agentproto.HostedRecord
		for _, k := range kept[z.ID] {
			own = append(own, k.Spec())
		}
		for _, w := range want[z.ID] {
			rec := agentproto.HostedRecord{Name: w.Name, Type: w.Type, Content: w.Content, Priority: w.Priority, TTL: zoneTTL}
			if w.Type == "A" || w.Type == "AAAA" {
				rec.TTL = hostTTL
			}
			if w.Type == "TXT" {
				rec.Content = dnssync.TXTText(w.Content)
			}
			if blocking := inTheWay(rec, own); blocking != nil {
				b.Issues = append(b.Issues, dnssync.Issue{Kind: "conflict", Name: rec.Name, Type: rec.Type, Want: rec.Content,
					Have: blocking.Type + " " + blocking.Content})
				continue
			}
			if slices.ContainsFunc(b.Made, func(m agentproto.HostedRecord) bool {
				return m.Name == rec.Name && m.Type == rec.Type && m.Content == rec.Content
			}) {
				continue // the same host name for two things
			}
			if err := rec.Check(z.Name); err != nil {
				continue
			}
			b.Made = append(b.Made, rec)
		}
		b.Spec.Records = append(append([]agentproto.HostedRecord{}, b.Made...), own...)
		out = append(out, b)
	}
	return out, nil
}

// inTheWay returns the kept record that keeps ekPanel from adding rec: one of the same
// type at the name (for TXT, one for the same purpose), or a CNAME either way.
func inTheWay(rec agentproto.HostedRecord, own []agentproto.HostedRecord) *agentproto.HostedRecord {
	for i, o := range own {
		if o.Name != rec.Name {
			continue
		}
		switch {
		case o.Type == "CNAME" || rec.Type == "CNAME":
			return &own[i]
		case o.Type == rec.Type && (rec.Type != "TXT" || dnssync.TXTKind(o.Content) == dnssync.TXTKind(rec.Content)):
			return &own[i]
		}
	}
	return nil
}

// nameServerSpec is what a server serves as a name server, or nil if it is none.
func (r *Runner) nameServerSpec(ctx context.Context, serverID string) (*agentproto.NameServerSpec, error) {
	ns, err := r.store.ListNameServers(ctx)
	if err != nil {
		return nil, err
	}
	me := slices.IndexFunc(ns, func(n store.NameServer) bool { return n.ServerID == serverID })
	pi := slices.IndexFunc(ns, func(n store.NameServer) bool { return n.Primary })
	if me < 0 || pi < 0 {
		return nil, nil
	}
	spec := &agentproto.NameServerSpec{Primary: ns[me].Primary, Zones: []agentproto.HostedZoneSpec{}}
	if spec.Primary {
		for _, n := range ns {
			if a := transferAddress(n); !n.Primary && a != "" && !slices.Contains(spec.Secondaries, a) {
				spec.Secondaries = append(spec.Secondaries, a)
			}
		}
		builds, err := r.HostedZones(ctx, "")
		if err != nil {
			return nil, err
		}
		for _, b := range builds {
			spec.Zones = append(spec.Zones, b.Spec)
			if err := r.store.SetHostedZoneIssues(ctx, b.Zone.ID, b.Issues); err != nil {
				return nil, err
			}
		}
		return spec, nil
	}
	spec.PrimaryAddress = transferAddress(ns[pi])
	if spec.PrimaryAddress == "" {
		return nil, errors.New("the primary name server has no address for zone transfers; set one on the DNS page")
	}
	zones, err := r.store.ListHostedZones(ctx, true, "")
	if err != nil {
		return nil, err
	}
	for _, z := range zones {
		// Signed once the primary has signed it: a copy fetched before that would be unsigned.
		spec.Zones = append(spec.Zones, agentproto.HostedZoneSpec{Name: z.Name, DNSSEC: z.DNSSEC && len(z.DS) > 0})
	}
	return spec, nil
}

// recordNameServer keeps what a name server reported, and on the primary each zone's.
func (r *Runner) recordNameServer(ctx context.Context, serverID string, spec *agentproto.NameServerSpec, res *agentproto.NameServerResult) error {
	if err := r.store.RecordNameServerStatus(ctx, serverID, *res); err != nil {
		return err
	}
	if !spec.Primary {
		return nil
	}
	before, err := r.store.ListHostedZones(ctx, true, "")
	if err != nil {
		return err
	}
	signed := map[string]bool{}
	for _, z := range before {
		signed[z.Name] = len(z.DS) > 0
	}
	newlySigned := false
	for _, z := range res.Zones {
		newlySigned = newlySigned || (len(z.DS) > 0 && !signed[z.Name])
		var msg *string
		if z.Error != "" {
			e := strings.TrimSpace(z.Error)
			msg = &e
		}
		if err := r.store.HostedZoneApplied(ctx, z.Name, z.Serial, z.DS, msg); err != nil {
			return err
		}
	}
	if newlySigned {
		// The secondaries now serve those zones as signed.
		ns, err := r.store.ListNameServers(ctx)
		if err != nil {
			return err
		}
		for _, n := range ns {
			if !n.Primary {
				if _, err := r.QueueReconcile(ctx, n.ServerID, nil); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
