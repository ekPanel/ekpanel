package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"ekpanel/internal/cloudflare"
	"ekpanel/internal/dnssync"
	"ekpanel/internal/store"
)

const KindDNSSync = "dns.sync"

// dnsSyncInterval is how often every zone is checked, which also notices records changed
// in Cloudflare by hand.
const dnsSyncInterval = 10 * time.Minute

var zoneLocks sync.Map

// QueueDNSSync syncs one zone soon, or every zone (zoneID ""). Repeated calls collapse.
func (r *Runner) QueueDNSSync(ctx context.Context, zoneID string, createdBy *string) (store.Task, error) {
	return r.Enqueue(ctx, store.NewTask{Kind: KindDNSSync, CreatedBy: createdBy, MaxAttempts: 3,
		Params: map[string]string{"zone_id": zoneID}, DedupeKey: "dns-sync:" + zoneID})
}

func (r *Runner) dnsSync(ctx context.Context, t store.Task) (any, error) {
	var p struct {
		ZoneID string `json:"zone_id"`
	}
	if err := json.Unmarshal(t.Params, &p); err != nil {
		return nil, err
	}
	return nil, r.SyncZones(ctx, p.ZoneID, nil)
}

func (r *Runner) dnsLoop(ctx context.Context) {
	tick := time.NewTicker(dnsSyncInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if err := r.SyncZones(ctx, "", nil); err != nil && ctx.Err() == nil {
			r.log.Warn("dns sync", "err", err)
		}
	}
}

// publicAddress picks the address of a server that records of type ("A" or "AAAA") point
// at: the one set by hand, or the first public one the agent reported.
func publicAddress(t store.HostTarget, typ string) string {
	set := t.PublicIPv4
	if typ == "AAAA" {
		set = t.PublicIPv6
	}
	if set != nil {
		return *set
	}
	// Shared (carrier NAT) and the old site-local IPv6 addresses are not reachable either.
	cgnat := &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}
	siteLocal := &net.IPNet{IP: net.ParseIP("fec0::"), Mask: net.CIDRMask(10, 128)}
	for _, a := range t.Addresses {
		ip := net.ParseIP(a)
		if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || cgnat.Contains(ip) || siteLocal.Contains(ip) || (ip.To4() != nil) != (typ == "A") {
			continue
		}
		return ip.String()
	}
	return ""
}

// zoneFor is the most specific zone a host name belongs to, among those that may hold it:
// the administrators' zones hold anyone's, a customer's only their own.
func zoneFor(host string, owner *string, zones []store.DNSZone) *store.DNSZone {
	var best *store.DNSZone
	for i, z := range zones {
		if host != z.Name && !strings.HasSuffix(host, "."+z.Name) {
			continue
		}
		if z.OwnerID != nil && (owner == nil || *owner != *z.OwnerID) {
			continue
		}
		if best == nil || len(z.Name) > len(best.Name) {
			best = &zones[i]
		}
	}
	return best
}

// SyncZones brings one zone (or all, zoneID "") in line with the panel's websites and apps.
// Keys in force ("A www.example.com") overwrite what is in the way.
func (r *Runner) SyncZones(ctx context.Context, zoneID string, force map[string]bool) error {
	zones, err := r.store.ListZones(ctx, true, "")
	if err != nil || len(zones) == 0 {
		return err
	}
	targets, err := r.store.HostTargets(ctx)
	if err != nil {
		return err
	}
	want := map[string][]dnssync.Want{}
	noAddress := map[string][]dnssync.Issue{}
	for _, t := range targets {
		z := zoneFor(t.Hostname, t.OwnerID, zones)
		if z == nil {
			continue
		}
		v4, v6 := publicAddress(t, "A"), publicAddress(t, "AAAA")
		if v4 != "" {
			want[z.ID] = append(want[z.ID], dnssync.Want{Name: t.Hostname, Type: "A", Content: v4})
		}
		if v6 != "" {
			want[z.ID] = append(want[z.ID], dnssync.Want{Name: t.Hostname, Type: "AAAA", Content: v6})
		}
		if v4 == "" && v6 == "" {
			noAddress[z.ID] = append(noAddress[z.ID], dnssync.Issue{Kind: "no_address", Name: t.Hostname, Type: "A", Server: t.ServerName})
		}
	}
	var errs []error
	for _, z := range zones {
		if zoneID != "" && z.ID != zoneID {
			continue
		}
		if err := r.syncZone(ctx, z, want[z.ID], noAddress[z.ID], force); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// CloudflareClient returns a client for a connected account.
func (r *Runner) CloudflareClient(ctx context.Context, accountID string) (*cloudflare.Client, error) {
	acct, err := r.store.GetCloudflareAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if r.secrets == nil {
		return nil, errors.New("the panel cannot open Cloudflare tokens")
	}
	token, err := r.secrets.Open(acct.Token)
	if err != nil {
		return nil, err
	}
	return cloudflare.New(string(token)), nil
}

func (r *Runner) syncZone(ctx context.Context, z store.DNSZone, want []dnssync.Want, issues []dnssync.Issue, force map[string]bool) error {
	lock, _ := zoneLocks.LoadOrStore(z.ID, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()

	fail := func(err error) error {
		msg := err.Error()
		r.store.ZoneSynced(context.WithoutCancel(ctx), z.ID, z.Status, z.NameServers, issues, &msg)
		return err
	}
	c, err := r.CloudflareClient(ctx, z.AccountID)
	if err != nil {
		return fail(err)
	}
	cz, err := c.Zone(ctx, z.CFZoneID)
	if err != nil {
		return fail(err)
	}
	have, err := c.Records(ctx, z.CFZoneID)
	if err != nil {
		return fail(err)
	}
	managed, err := r.store.ManagedRecords(ctx, z.ID)
	if err != nil {
		return err
	}
	ours := make([]dnssync.Managed, len(managed))
	for i, m := range managed {
		ours[i] = dnssync.Managed{RecordID: m.CFRecordID, Name: m.Name, Type: m.Type, Content: m.Content, Proxied: m.Proxied}
	}
	actions, planned := dnssync.Plan(want, ours, have, z.Proxied, force)
	issues = append(issues, planned...)
	keep := func(rec cloudflare.Record) error {
		return r.store.KeepRecord(ctx, z.ID, store.DNSRecord{CFRecordID: rec.ID, Type: rec.Type, Name: rec.Name, Content: rec.Content, Proxied: rec.Proxied})
	}
	var errs []string
	for _, a := range actions {
		var err error
		switch a.Kind {
		case "create":
			var rec cloudflare.Record
			if rec, err = c.CreateRecord(ctx, z.CFZoneID, a.Record); err == nil {
				err = keep(rec)
			}
		case "update":
			var rec cloudflare.Record
			if rec, err = c.UpdateRecord(ctx, z.CFZoneID, a.Record); err == nil {
				err = keep(rec)
			}
		case "adopt":
			err = keep(a.Record)
		case "delete":
			if err = c.DeleteRecord(ctx, z.CFZoneID, a.Record.ID); err != nil && isNotFound(err) {
				err = nil
			}
		}
		if err == nil && a.Forget != "" {
			err = r.store.ForgetRecord(ctx, z.ID, a.Forget)
		}
		if err != nil {
			errs = append(errs, a.Record.Type+" "+a.Record.Name+": "+err.Error())
		}
	}
	var syncErr *string
	if len(errs) > 0 {
		msg := strings.Join(errs, "; ")
		syncErr = &msg
	}
	return r.store.ZoneSynced(ctx, z.ID, cz.Status, cz.NameServers, issues, syncErr)
}

func isNotFound(err error) bool {
	var e *cloudflare.Error
	return errors.As(err, &e) && e.Status == 404
}
