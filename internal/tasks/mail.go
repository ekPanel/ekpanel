package tasks

import (
	"context"
	"encoding/json"
	"errors"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/store"
)

// mailSpec adds the server's email to the desired state, and returns its domains for
// reading the results.
func (r *Runner) mailSpec(ctx context.Context, serverID string, params *agentproto.StateApplyParams) ([]store.MailDomain, error) {
	ms, err := r.store.GetMailServer(ctx, serverID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	domains, err := r.store.ListMailDomains(ctx, serverID, "")
	if err != nil {
		return nil, err
	}
	mailboxes, err := r.store.MailboxesOn(ctx, "", serverID)
	if err != nil {
		return nil, err
	}
	forwarders, err := r.store.ForwardersOn(ctx, "", serverID)
	if err != nil {
		return nil, err
	}
	spec := &agentproto.MailSpec{Hostname: ms.Hostname, TLS: ms.TLS, Domains: make([]agentproto.MailDomainSpec, 0, len(domains))}
	for _, d := range domains {
		ds := agentproto.MailDomainSpec{ID: d.ID, Generation: d.Generation, Domain: d.Domain, Deleted: d.State == "deleting",
			Mailboxes: []agentproto.MailboxSpec{}, Forwarders: []agentproto.ForwarderSpec{}}
		if d.CatchAll != nil {
			ds.CatchAll = *d.CatchAll
		}
		for _, mb := range mailboxes {
			if mb.DomainID == d.ID {
				ds.Mailboxes = append(ds.Mailboxes, agentproto.MailboxSpec{ID: mb.ID, Name: mb.LocalPart, PasswordHash: mb.PasswordHash,
					QuotaMB: mb.QuotaMB, ForwardTo: mb.ForwardTo, KeepCopy: mb.KeepCopy, AutoReply: mb.AutoReply})
			}
		}
		for _, f := range forwarders {
			if f.DomainID == d.ID {
				ds.Forwarders = append(ds.Forwarders, agentproto.ForwarderSpec{ID: f.ID, Name: f.LocalPart, To: f.Destinations})
			}
		}
		spec.Domains = append(spec.Domains, ds)
	}
	params.Mail = spec
	return domains, nil
}

// applyMailResult records what the agent did with email, and syncs DNS when a domain's
// records changed.
func (r *Runner) applyMailResult(ctx context.Context, serverID string, domains []store.MailDomain, res *agentproto.MailResult) (ReconcileSummary, error) {
	var sum ReconcileSummary
	if err := r.store.MailServerResult(ctx, serverID, res.OK, res.Error); err != nil {
		return sum, err
	}
	if !res.OK {
		sum.Failed++
	}
	dnsChanged := false
	for _, dr := range res.Domains {
		for _, d := range domains {
			if d.ID != dr.ID {
				continue
			}
			if !dr.OK && d.State != "error" && d.State != "deleting" {
				r.alert(ctx, d.OwnerID, "mail_error", "critical", map[string]string{"name": d.Domain, "message": dr.Error}, "/mail/"+d.ID)
			}
			if dr.OK && (d.State == "deleting" || (dr.DNS != nil && !sameRecords(d.DNSRecords, dr.DNS))) {
				dnsChanged = true
			}
		}
		if err := r.store.ApplyMailDomainResult(ctx, dr); err != nil {
			return sum, err
		}
		if dr.OK {
			sum.Applied++
		} else {
			sum.Failed++
		}
	}
	if dnsChanged {
		if has, err := r.store.HasZones(ctx); err == nil && has {
			r.QueueDNSSync(ctx, "", nil)
		}
	}
	return sum, nil
}

func sameRecords(a, b []agentproto.MailDNSRecord) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
