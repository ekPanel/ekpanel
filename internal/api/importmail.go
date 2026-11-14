package api

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"golang.org/x/net/publicsuffix"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/auth"
	"ekpanel/internal/store"
	"ekpanel/internal/tasks"
)

// Email and DNS zones of a cPanel import: checked before anything is made, then made after
// the websites (whose domains they belong to).

// importHosts are the host names of the websites chosen for an import, with their www.
func importHosts(domains []agentproto.CPanelDomain) []string {
	var out []string
	for _, d := range domains {
		for _, h := range append([]string{d.Domain}, d.Aliases...) {
			out = append(out, h, "www."+h)
		}
	}
	return out
}

// ownedHosts are the host names the owner has, or will have once the chosen websites are
// made; nil for administrators (they may take any domain).
func (s *Server) ownedHosts(ctx context.Context, place placement, chosen []agentproto.CPanelDomain) ([]string, error) {
	if place.OwnerID == nil {
		return nil, nil
	}
	hosts, err := s.store.OwnerHostnames(ctx, *place.OwnerID)
	return append(hosts, importHosts(chosen)...), err
}

// checkImportMail finds the chosen mail domains in the plan and checks that they can be
// made: email on the server, names free, the owner's domains, the package's limits.
func (s *Server) checkImportMail(w http.ResponseWriter, r *http.Request, srv store.Server, place placement, plan agentproto.CPanelPlan,
	chosen []string, owned []string) ([]agentproto.CPanelMailDomain, bool) {
	if len(chosen) == 0 {
		return nil, true
	}
	ctx := r.Context()
	var out []agentproto.CPanelMailDomain
	boxes := 0
	for _, name := range chosen {
		i := slices.IndexFunc(plan.Mail, func(d agentproto.CPanelMailDomain) bool { return d.Domain == name })
		if i < 0 {
			writeErrorData(w, http.StatusBadRequest, "not_in_backup", "the backup has no email for "+name, map[string]any{"name": name})
			return nil, false
		}
		out = append(out, plan.Mail[i])
		boxes += len(plan.Mail[i].Mailboxes)
		if owned != nil && !slices.Contains(owned, name) && !slices.Contains(owned, "www."+name) {
			writeErrorData(w, http.StatusForbidden, "import_mail_not_owned", "email for "+name+" needs a website with that domain",
				map[string]any{"name": name})
			return nil, false
		}
	}
	ms, err := s.store.GetMailServer(ctx, srv.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "mail_not_enabled", "this server has no email")
		return nil, false
	}
	taken, err := s.store.MailDomainsTaken(ctx, chosen)
	if err != nil {
		s.internalError(w, r, err)
		return nil, false
	}
	if slices.Contains(chosen, ms.Hostname) {
		taken = append(taken, ms.Hostname) // the mail host name cannot be a mail domain
	}
	if len(taken) > 0 {
		writeErrorData(w, http.StatusConflict, "import_mail_taken", "email is set up here already for "+strings.Join(taken, ", "),
			map[string]any{"names": strings.Join(taken, ", ")})
		return nil, false
	}
	if place.Package != nil {
		usage, err := s.store.OwnerUsage(ctx, *place.OwnerID)
		if err != nil {
			s.internalError(w, r, err)
			return nil, false
		}
		switch {
		case usage.MailDomains+len(out) > place.Package.MaxMailDomains:
			writeError(w, http.StatusConflict, "limit_reached_mail_domain", "the package does not allow that many mail domains")
			return nil, false
		case usage.Mailboxes+boxes > place.Package.MaxMailboxes:
			writeError(w, http.StatusConflict, "limit_reached_mailbox", "the package does not allow that many mailboxes")
			return nil, false
		}
		if !s.resellerAllows(w, r, place.OwnerID, store.Usage{MailDomains: len(out), Mailboxes: boxes}, srv.ID) {
			return nil, false
		}
	}
	return out, true
}

// checkImportZones finds the chosen zones and checks that they can be made on ekPanel's
// name servers.
func (s *Server) checkImportZones(w http.ResponseWriter, r *http.Request, place placement, plan agentproto.CPanelPlan,
	chosen []string, owned []string) ([]agentproto.CPanelZone, bool) {
	if len(chosen) == 0 {
		return nil, true
	}
	ctx := r.Context()
	ns, err := s.store.ListNameServers(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return nil, false
	}
	if !slices.ContainsFunc(ns, func(n store.NameServer) bool { return n.Primary }) {
		s.writeHostedError(w, r, tasks.ErrNoNameServers)
		return nil, false
	}
	zones, err := s.store.ListHostedZones(ctx, true, "")
	if err != nil {
		s.internalError(w, r, err)
		return nil, false
	}
	var out []agentproto.CPanelZone
	for _, name := range chosen {
		i := slices.IndexFunc(plan.Zones, func(z agentproto.CPanelZone) bool { return z.Name == name })
		if i < 0 {
			writeErrorData(w, http.StatusBadRequest, "not_in_backup", "the backup has no DNS zone "+name, map[string]any{"name": name})
			return nil, false
		}
		if suffix, _ := publicsuffix.PublicSuffix(name); name == suffix {
			writeError(w, http.StatusBadRequest, "invalid_zone", name+" cannot be a zone")
			return nil, false
		}
		if taken, err := s.store.ZoneNameTaken(ctx, name); err != nil {
			s.internalError(w, r, err)
			return nil, false
		} else if taken {
			writeErrorData(w, http.StatusConflict, "import_zone_taken", "ekPanel already has the zone "+name, map[string]any{"name": name})
			return nil, false
		}
		if z := zoneOverlap(zones, name, place.OwnerID); z != nil {
			writeError(w, http.StatusConflict, "zone_overlaps", "this domain is inside, or contains, another account's zone "+z.Name)
			return nil, false
		}
		if owned != nil && !slices.ContainsFunc(owned, func(h string) bool { return h == name || strings.HasSuffix(h, "."+name) }) {
			writeErrorData(w, http.StatusForbidden, "import_zone_not_owned", "the zone "+name+" needs a website on that domain",
				map[string]any{"name": name})
			return nil, false
		}
		out = append(out, plan.Zones[i])
	}
	return out, true
}

// makeImportMail makes the mail domains, their mailboxes and forwarders, and returns the
// mailboxes whose messages the import task copies in.
func (s *Server) makeImportMail(ctx context.Context, srv store.Server, place placement, by string,
	domains []agentproto.CPanelMailDomain, report *tasks.ImportReport) ([]agentproto.ImportMailbox, error) {
	var copyIn []agentproto.ImportMailbox
	note := func(n tasks.ImportNote) { report.NotImported = append(report.NotImported, n) }
	for _, d := range domains {
		md, err := s.store.CreateMailDomain(ctx, store.NewMailDomain{ServerID: srv.ID, Domain: d.Domain, OwnerID: place.OwnerID, CreatedBy: &by})
		if err != nil {
			return nil, err
		}
		report.MailDomains = append(report.MailDomains, d.Domain)
		// cPanel delivers to a mailbox and to a forwarder of the same address: here that is
		// the mailbox forwarding a copy.
		forwards := map[string][]string{}
		for _, f := range d.Forwarders {
			forwards[f.Name] = f.To
		}
		var made []string
		for _, mb := range d.Mailboxes {
			address := mb.Name + "@" + d.Domain
			if !agentproto.ValidLocalPart(mb.Name) {
				note(tasks.ImportNote{Code: "mail_mailbox", Name: address})
				continue
			}
			m := store.Mailbox{DomainID: md.ID, LocalPart: mb.Name, KeepCopy: true, QuotaMB: min(mb.QuotaMB, agentproto.MaxMailboxQuotaMB)}
			if place.Package != nil && place.Package.MailboxQuotaMB != nil {
				m.QuotaMB = *place.Package.MailboxQuotaMB
			}
			if to, ok := forwards[mb.Name]; ok {
				delete(forwards, mb.Name)
				if agentproto.ValidateForwards(to) == nil {
					m.ForwardTo = to
				} else {
					note(tasks.ImportNote{Code: "mail_forwarder", Name: address})
				}
			}
			m.PasswordHash = mb.PasswordHash
			if !agentproto.ValidSHA512Crypt(m.PasswordHash) {
				if m.PasswordHash, err = auth.MailPasswordHash(auth.NewDatabasePassword()); err != nil {
					return nil, err
				}
				report.NewMailPassword = append(report.NewMailPassword, address)
			}
			if _, err := s.store.CreateMailbox(ctx, m); err != nil {
				return nil, err
			}
			made = append(made, mb.Name)
			report.Mailboxes++
			switch {
			case mb.Mdbox:
				note(tasks.ImportNote{Code: "mail_mdbox", Name: address})
			case mb.Messages > 0:
				copyIn = append(copyIn, agentproto.ImportMailbox{Domain: d.Domain, Name: mb.Name})
			}
		}
		for _, f := range d.Forwarders {
			to, ok := forwards[f.Name]
			if !ok {
				continue // a mailbox's
			}
			if !agentproto.ValidLocalPart(f.Name) || agentproto.ValidateForwards(to) != nil {
				note(tasks.ImportNote{Code: "mail_forwarder", Name: f.Name + "@" + d.Domain})
				continue
			}
			if _, err := s.store.CreateMailForwarder(ctx, store.MailForwarder{DomainID: md.ID, LocalPart: f.Name, Destinations: to}); err != nil {
				return nil, err
			}
			report.Forwarders++
		}
		if d.CatchAll != "" {
			if !slices.Contains(made, d.CatchAll) {
				// Here the catch-all must be one of the domain's mailboxes.
				note(tasks.ImportNote{Code: "mail_catch_all", Name: d.CatchAll + "@" + d.Domain, Detail: d.Domain})
			} else if _, err := s.store.SetMailCatchAll(ctx, md.ID, &d.CatchAll); err != nil {
				return nil, err
			}
		}
		for _, o := range d.Other {
			note(tasks.ImportNote{Code: "mail_unusable", Name: o, Detail: d.Domain})
		}
	}
	return copyIn, nil
}

// makeImportZones makes the zones on ekPanel's name servers with the records worth keeping.
func (s *Server) makeImportZones(ctx context.Context, place placement, by string, plan agentproto.CPanelPlan,
	zones []agentproto.CPanelZone, chosenHosts []string, report *tasks.ImportReport) error {
	for _, z := range zones {
		var names []string
		for _, rec := range z.Records {
			names = append(names, rec.Name, rec.Content)
		}
		managed := map[string]bool{}
		taken, err := s.store.HostnamesTaken(ctx, append(names, chosenHosts...))
		if err != nil {
			return err
		}
		for _, h := range append(taken, chosenHosts...) {
			managed[h] = true
		}
		mailHere, err := s.store.MailDomainsTaken(ctx, []string{z.Name})
		if err != nil {
			return err
		}
		keep, notes := tasks.FilterCPanelZone(z, tasks.CPanelZoneFilter{Managed: managed, MailHere: len(mailHere) > 0, OldIP: plan.OldIP})
		report.NotImported = append(report.NotImported, notes...)
		if err := agentproto.CheckRecords(z.Name, keep); err != nil {
			report.NotImported = append(report.NotImported, tasks.ImportNote{Code: "zone_failed", Name: z.Name, Detail: err.Error()})
			continue
		}
		if _, err := s.store.CreateHostedZone(ctx, z.Name, place.OwnerID, &by, keep); err != nil {
			return err
		}
		report.Zones = append(report.Zones, z.Name)
	}
	return nil
}
